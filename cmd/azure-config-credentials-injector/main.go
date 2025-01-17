package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

const (
	clientIDEnvKey       = "AZURE_CLIENT_ID"
	clientSecretEnvKey   = "AZURE_CLIENT_SECRET"
	tenantIDEnvKey       = "AZURE_TENANT_ID"
	federatedTokenEnvKey = "AZURE_FEDERATED_TOKEN_FILE"

	clientIDCloudConfigKey               = "aadClientId"
	clientSecretCloudConfigKey           = "aadClientSecret"
	useManagedIdentityExtensionConfigKey = "useManagedIdentityExtension"

	tenantIdConfigKey                              = "tenantId"
	aadFederatedTokenFileConfigKey                 = "aadFederatedTokenFile"
	useFederatedWorkloadIdentityExtensionConfigKey = "useFederatedWorkloadIdentityExtension"
)

var (
	injectorCmd = &cobra.Command{
		Use:   "azure-config-credentials-injector [OPTIONS]",
		Short: "Cloud config credentials injection tool for azure cloud platform",
		RunE:  mergeCloudConfig,
	}

	injectorOpts struct {
		cloudConfigFilePath          string
		outputFilePath               string
		enableWorkloadIdentity       string
		disableIdentityExtensionAuth bool
	}
)

func init() {
	klog.InitFlags(flag.CommandLine)
	injectorCmd.PersistentFlags().AddGoFlagSet(flag.CommandLine)
	injectorCmd.PersistentFlags().StringVar(&injectorOpts.cloudConfigFilePath, "cloud-config-file-path", "/tmp/cloud-config/cloud.conf", "Location of the original cloud config file.")
	injectorCmd.PersistentFlags().StringVar(&injectorOpts.outputFilePath, "output-file-path", "/tmp/merged-cloud-config/cloud.conf", "Location of the generated cloud config file with injected credentials.")
	injectorCmd.PersistentFlags().BoolVar(&injectorOpts.disableIdentityExtensionAuth, "disable-identity-extension-auth", false, "Disable managed identity authentication, if it's set in cloudConfig.")
	injectorCmd.PersistentFlags().StringVar(&injectorOpts.enableWorkloadIdentity, "enable-azure-workload-identity", "false", "Enable workload identity authentication.")
}

func main() {
	if err := injectorCmd.Execute(); err != nil {
		klog.Fatal(err)
	}
}

func mergeCloudConfig(_ *cobra.Command, args []string) error {
	var (
		azureClientId      string
		azureClientSecret  string
		tenantId           string
		federatedTokenFile string
		secretFound        bool
		err                error
	)

	if _, err := os.Stat(injectorOpts.cloudConfigFilePath); os.IsNotExist(err) {
		return err
	}

	azureClientId, found := mustLookupEnvValue(clientIDEnvKey)
	if !found {
		return fmt.Errorf("%s env variable should be set up", clientIDEnvKey)
	}

	azureClientSecret, secretFound = mustLookupEnvValue(clientSecretEnvKey)

	// Ensure there is only one authentication method.
	if injectorOpts.enableWorkloadIdentity == "true" {
		tenantId, federatedTokenFile, err = lookupWorkloadIdentityEnv(tenantIDEnvKey, federatedTokenEnvKey)
		if err != nil {
			return fmt.Errorf("workload identity method failed: %v", err)
		}
		if secretFound {
			return fmt.Errorf("%s env variable is set while workload identity is enabled, this should never happen.\nPlease consider reporting a bug: https://issues.redhat.com", clientSecretEnvKey)
		}
	} else {
		if !secretFound {
			return fmt.Errorf("%s env variable should be set up", clientSecretEnvKey)
		}
	}

	cloudConfig, err := readCloudConfig(injectorOpts.cloudConfigFilePath)
	if err != nil {
		return fmt.Errorf("couldn't read cloud config from file: %w", err)
	}

	preparedCloudConfig, err := prepareCloudConfig(cloudConfig, azureClientId, azureClientSecret, tenantId, federatedTokenFile)
	if err != nil {
		return fmt.Errorf("couldn't prepare cloud config: %w", err)
	}

	if err := writeCloudConfig(injectorOpts.outputFilePath, preparedCloudConfig); err != nil {
		return fmt.Errorf("couldn't write prepared cloud config to file: %w", err)
	}

	return nil
}

func readCloudConfig(path string) (map[string]interface{}, error) {
	var data map[string]interface{}

	rawData, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawData, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func prepareCloudConfig(cloudConfig map[string]interface{}, clientId, clientSecret, tenantId, federatedTokenFile string) ([]byte, error) {
	cloudConfig[clientIDCloudConfigKey] = clientId

	if value, found := cloudConfig[useManagedIdentityExtensionConfigKey]; found {
		if injectorOpts.disableIdentityExtensionAuth {
			klog.Infof("%s cleared\n", useManagedIdentityExtensionConfigKey)
			cloudConfig[useManagedIdentityExtensionConfigKey] = false
		} else {
			if value == true {
				klog.Warningf("Warning: %s is set to \"true\", injected credentials may not be used\n", useManagedIdentityExtensionConfigKey)
			}
		}
	}

	if len(tenantId) != 0 && len(federatedTokenFile) != 0 {
		cloudConfig[tenantIdConfigKey] = tenantId
		cloudConfig[aadFederatedTokenFileConfigKey] = federatedTokenFile
		cloudConfig[useFederatedWorkloadIdentityExtensionConfigKey] = true
	} else {
		klog.V(4).Info("%s env variable is set, client secret authentication will be used", clientSecretEnvKey)
		cloudConfig[clientSecretCloudConfigKey] = clientSecret
	}

	marshalled, err := json.Marshal(cloudConfig)
	if err != nil {
		return nil, err
	}

	return marshalled, nil
}

func writeCloudConfig(path string, preparedConfig []byte) error {
	if err := os.WriteFile(path, preparedConfig, 0644); err != nil {
		return err
	}
	return nil
}

// lookupWorkloadIdentityEnv loads tenantID and federatedTokenFile values from environment, which are both required for
// workload identity. Return error if any or both values are missing.
func lookupWorkloadIdentityEnv(tenantEnvKey, tokenEnvKey string) (tenantId, federatedTokenFile string, err error) {
	tenantId, tenantIdFound := mustLookupEnvValue(tenantEnvKey)
	federatedTokenFile, federatedTokenFileFound := mustLookupEnvValue(tokenEnvKey)
	klog.V(4).Infof("env vars required for workload identity auth are set to: %v=%v and %v=%v", tenantEnvKey, tenantId, tokenEnvKey, federatedTokenFile)

	if !tenantIdFound && !federatedTokenFileFound {
		err = fmt.Errorf("%v and %v environment variables not found or empty", tenantEnvKey, tokenEnvKey)
		return
	}

	if !tenantIdFound {
		err = fmt.Errorf("%v environment variable not found or empty", tenantEnvKey)
		return
	}

	if !federatedTokenFileFound {
		err = fmt.Errorf("%v environment variable not found or empty", tokenEnvKey)
	}

	return
}

func mustLookupEnvValue(key string) (string, bool) {
	value, found := os.LookupEnv(key)
	if !found || len(value) == 0 {
		return "", false
	}
	return value, true
}
