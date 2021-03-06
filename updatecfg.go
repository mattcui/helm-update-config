package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"gopkg.in/yaml.v1"
	"k8s.io/client-go/util/homedir"
	"k8s.io/helm/pkg/helm"
	helmEnv "k8s.io/helm/pkg/helm/environment"
	"k8s.io/helm/pkg/strvals"
	"k8s.io/helm/pkg/tlsutil"
)

//const (
//	// DefaultTLSCaCert is the default value for HELM_TLS_CA_CERT
//	DefaultTLSCaCert = "$HELM_HOME/ca.pem"
//	// DefaultTLSCert is the default value for HELM_TLS_CERT
//	DefaultTLSCert = "$HELM_HOME/cert.pem"
//	// DefaultTLSKeyFile is the default value for HELM_TLS_KEY_FILE
//	DefaultTLSKeyFile = "$HELM_HOME/key.pem"
//	// DefaultTLSEnable is the default value for HELM_TLS_ENABLE
//	DefaultTLSEnable = false
//	// DefaultTLSVerify is the default value for HELM_TLS_VERIFY
//	DefaultTLSVerify = false
//)

var (
	settings        helmEnv.EnvSettings
	DefaultHelmHome = filepath.Join(homedir.HomeDir(), ".helm")
)

func addCommonCmdOptions(f *flag.FlagSet) {
	settings.AddFlagsTLS(f)
	settings.InitTLS(f)

	f.StringVar((*string)(&settings.Home), "home", DefaultHelmHome, "location of your Helm config. Overrides $HELM_HOME")
}

type cmdFlags struct {
	cliValues  []string
	valueFiles ValueFiles

	// TillerHost is the host and port of Tiller.
	TillerHost string
	// TLSEnable tells helm to communicate with Tiller via TLS
	TLSEnable bool
	// TLSVerify tells helm to communicate with Tiller via TLS and to verify remote certificates served by Tiller
	TLSVerify bool
	// TLSServerName tells helm to verify the hostname on the returned certificates from Tiller
	TLSServerName string
	// TLSCaCertFile is the path to a TLS CA certificate file
	TLSCaCertFile string
	// TLSCertFile is the path to a TLS certificate file
	TLSCertFile string
	// TLSKeyFile is the path to a TLS key file
	TLSKeyFile string
}

type ValueFiles []string

func (v *ValueFiles) String() string {
	return fmt.Sprint(*v)
}

func (v *ValueFiles) Type() string {
	return "valueFiles"
}

func (v *ValueFiles) Set(value string) error {
	for _, filePath := range strings.Split(value, ",") {
		*v = append(*v, filePath)
	}
	return nil
}

func createHelmClient() helm.Interface {
	options := []helm.Option{helm.Host(os.Getenv("TILLER_HOST")), helm.ConnectTimeout(int64(30))}

	if settings.TLSVerify || settings.TLSEnable {
		tlsopts := tlsutil.Options{
			ServerName:         settings.TLSServerName,
			KeyFile:            settings.TLSKeyFile,
			CertFile:           settings.TLSCertFile,
			InsecureSkipVerify: true,
		}

		if settings.TLSVerify {
			tlsopts.CaCertFile = settings.TLSCaCertFile
			tlsopts.InsecureSkipVerify = false
		}

		tlscfg, err := tlsutil.ClientConfig(tlsopts)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}

		options = append(options, helm.WithTLS(tlscfg))
	}

	return helm.NewClient(options...)
}

func isHelm3() bool {
	return os.Getenv("TILLER_HOST") == ""
}

func newUpdatecfgCmd() *cobra.Command {
	var flags cmdFlags

	cmd := &cobra.Command{
		Use:   "helm update-config [flags] RELEASE",
		Short: "update config values of an existing release",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vals := make(map[string]interface{})
			for _, v := range flags.cliValues {
				if err := strvals.ParseInto(v, vals); err != nil {
					return err
				}
			}

			update := updateConfigCommand{
				client:     createHelmClient(),
				release:    args[0],
				values:     flags.cliValues,
				valueFiles: flags.valueFiles,
				useTLS:     flags.TLSEnable,
			}

			return update.run()
		},
	}
	f := cmd.Flags()

	f.StringArrayVar(&flags.cliValues, "set-value", []string{}, "set values on the command line (can specify multiple or separate values with commas: key1=val1,key2=val2)")
	f.VarP(&flags.valueFiles, "values", "f", "specify values in a YAML file")

	if !isHelm3() {
		addCommonCmdOptions(f)
	}
	return cmd
}

type updateConfigCommand struct {
	client     helm.Interface
	release    string
	values     []string
	valueFiles ValueFiles
	useTLS     bool
}

func (cmd *updateConfigCommand) run() error {
	// This code supports to check a release from release name directly, no limitation on naming.
	ls, err := cmd.client.ListReleases(helm.ReleaseListFilter(cmd.release))
	if err != nil {
		return err
	}

	if ls.GetCount() == 0 {
		return fmt.Errorf("the count of release %s is zero", cmd.release)
	}

	var preVals map[string]interface{}
	err = yaml.Unmarshal([]byte(ls.Releases[0].Config.Raw), &preVals)
	if err != nil {
		return errors.Wrapf(err, "Failed to unmarshal raw values: %v", ls.Releases[0].Config.Raw)
	}

	preferredVals, err := GenerateUpdatedValues(cmd.valueFiles, cmd.values)
	if err != nil {
		return errors.Wrapf(err, "Failed to generate preferred values: %v", preferredVals)
	}

	mergedVals := mergeValues(preVals, preferredVals)
	valBytes, err := yaml.Marshal(mergedVals)
	if err != nil {
		return errors.Wrapf(err, "Failed to marshal merged values: %v", mergedVals)
	}

	var opt helm.UpdateOption
	opt = helm.ReuseValues(true)

	_, err = cmd.client.UpdateReleaseFromChart(
		ls.Releases[0].Name,
		ls.Releases[0].Chart,
		helm.UpdateValueOverrides(valBytes),
		opt,
	)

	if err != nil {
		return errors.Wrapf(err, "Failed to update release")
	}

	fmt.Printf("Info: update successfully\n")
	return nil
}

// mergeValues merges destination and source map, preferring values from the source map
func mergeValues(dest map[string]interface{}, src map[string]interface{}) map[string]interface{} {
	for k, v := range src {
		// If the key doesn't exist, then just set the key to that value
		if _, exists := dest[k]; !exists {
			dest[k] = v
			continue
		}

		nextMap, ok := v.(map[interface{}]interface{})
		// If it isn't another map, overwrite the value
		if !ok {
			dest[k] = v
			continue
		}

		// Edge case: If the key exists in the destination, but isn't a map
		destMap, isMap := dest[k].(map[interface{}]interface{})
		// If the source map has a map for this key, prefer it
		if !isMap {
			dest[k] = v
			continue
		}

		// If they are both map, merge them
		dest[k] = mergeValues(convertKeyAsString(destMap), convertKeyAsString(nextMap))
	}

	return dest
}

// GenerateUpdatedValues generates values from files specified via -f/--values and directly via --set-value, preferring values via --set-value
func GenerateUpdatedValues(valueFiles ValueFiles, values []string) (map[string]interface{}, error) {
	base := map[string]interface{}{}

	// User specified a values files via -f/--values
	for _, filePath := range valueFiles {
		currentMap := map[string]interface{}{}

		var bytes []byte
		var err error

		bytes, err = ioutil.ReadFile(filePath)

		if err != nil {
			return map[string]interface{}{}, err
		}

		if err := yaml.Unmarshal(bytes, &currentMap); err != nil {
			return map[string]interface{}{}, fmt.Errorf("failed to parse %s: %s", filePath, err)
		}
		// Merge with the previous map
		base = mergeValues(base, currentMap)
	}

	// User specified a value via --set-value
	for _, value := range values {
		if err := strvals.ParseInto(value, base); err != nil {
			return map[string]interface{}{}, fmt.Errorf("failed parsing --set-value data: %s", err)
		}
	}

	return base, nil
}

func convertKeyAsString(ori map[interface{}]interface{}) map[string]interface{} {
	result := map[string]interface{}{}
	for k, v := range ori {
		result[k.(string)] = v
	}

	return result
}
