package phoenixnap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"k8s.io/klog/v2"
)

const (
	clientIDName               = "PNAP_CLIENT_ID"
	clientSecretName           = "PNAP_CLIENT_SECRET"
	locationName               = "PNAP_LOCATION"
	loadBalancerSettingName    = "PNAP_LOAD_BALANCER"
	envVarAnnotationIPLocation = "PNAP_ANNOTATION_IP_LOCATION"
	envVarAPIServerPort        = "PNAP_API_SERVER_PORT"
	envVarServiceNodeSelector  = "PNAP_SERVICE_NODE_SELECTOR"
)

// Config configuration for a provider, includes authentication token, and optional override URL to talk to a different PhoenixNAP API endpoint
type Config struct {
	ClientID             string  `json:"clientID"`
	ClientSecret         string  `json:"clientSecret"`
	BaseURL              *string `json:"base-url,omitempty"`
	TokenURL             *string `json:"token-url,omitempty"`
	LoadBalancerSetting  string  `json:"loadbalancer"`
	Location             string  `json:"location,omitempty"`
	AnnotationIPLocation string  `json:"annotationIPLocation,omitempty"`
	APIServerPort        int32   `json:"apiServerPort,omitempty"`
	ServiceNodeSelector  string  `json:"serviceNodeSelector,omitempty"`
}

// String converts the Config structure to a string, while masking hidden fields.
// Is not 100% a String() conversion, as it adds some intelligence to the output,
// and masks sensitive data
func (c Config) Strings() []string {
	ret := []string{fmt.Sprintf("ClientID: '%s'", c.ClientID)}
	if c.ClientSecret != "" {
		ret = append(ret, "ClientSecret: '<masked>'")
	} else {
		ret = append(ret, "ClientSecret: ''")
	}
	if c.LoadBalancerSetting == "" {
		ret = append(ret, "loadbalancer config: disabled")
	} else {
		ret = append(ret, fmt.Sprintf("loadbalancer config: '%s'", c.LoadBalancerSetting))
	}
	if c.BaseURL != nil {
		ret = append(ret, fmt.Sprintf("base-url: '%s'", *c.BaseURL))
	}
	if c.TokenURL != nil {
		ret = append(ret, fmt.Sprintf("token-url: '%s'", *c.TokenURL))
	}
	ret = append(ret, fmt.Sprintf("location: '%s'", c.Location))
	ret = append(ret, fmt.Sprintf("IP Location annotation: '%s'", c.AnnotationIPLocation))
	ret = append(ret, fmt.Sprintf("api server port: %d", c.APIServerPort))
	ret = append(ret, fmt.Sprintf("service node selector: '%s'", c.ServiceNodeSelector))

	return ret
}

func getConfig(providerConfig io.Reader) (Config, error) {
	// get our config, most importantly our client secret and client ID.
	// providerConfig is nil when --cloud-config is not set, in which case
	// everything has to come from env vars
	var config, rawConfig Config
	if providerConfig != nil {
		configBytes, err := io.ReadAll(providerConfig)
		if err != nil {
			return config, fmt.Errorf("failed to read configuration: %w", err)
		}
		if len(bytes.TrimSpace(configBytes)) > 0 {
			if err := json.Unmarshal(configBytes, &rawConfig); err != nil {
				return config, fmt.Errorf("failed to process json of configuration file: %w", err)
			}
		}
	}

	// read env vars; if not set, use rawConfig
	ClientID := os.Getenv(clientIDName)
	if ClientID == "" {
		ClientID = rawConfig.ClientID
	}
	config.ClientID = ClientID

	ClientSecret := os.Getenv(clientSecretName)
	if ClientSecret == "" {
		ClientSecret = rawConfig.ClientSecret
	}
	config.ClientSecret = ClientSecret

	loadBalancerSetting := os.Getenv(loadBalancerSettingName)
	config.LoadBalancerSetting = rawConfig.LoadBalancerSetting
	// rule for processing: any setting in env var overrides setting from file
	if loadBalancerSetting != "" {
		config.LoadBalancerSetting = loadBalancerSetting
	}

	location := os.Getenv(locationName)
	if location == "" {
		location = rawConfig.Location
	}

	if ClientID == "" {
		return config, fmt.Errorf("environment variable %q or config field %q is required", clientIDName, "clientID")
	}
	if ClientSecret == "" {
		return config, fmt.Errorf("environment variable %q or config field %q is required", clientSecretName, "clientSecret")
	}

	config.Location = location
	config.BaseURL = rawConfig.BaseURL
	config.TokenURL = rawConfig.TokenURL

	// set the annotations
	config.AnnotationIPLocation = DefaultAnnotationIPLocation
	if rawConfig.AnnotationIPLocation != "" {
		config.AnnotationIPLocation = rawConfig.AnnotationIPLocation
	}
	annotationIPLocation := os.Getenv(envVarAnnotationIPLocation)
	if annotationIPLocation != "" {
		config.AnnotationIPLocation = annotationIPLocation
	}

	config.ServiceNodeSelector = rawConfig.ServiceNodeSelector
	if serviceNodeSelector := os.Getenv(envVarServiceNodeSelector); serviceNodeSelector != "" {
		config.ServiceNodeSelector = serviceNodeSelector
	}

	apiServer := os.Getenv(envVarAPIServerPort)
	switch {
	case apiServer != "":
		apiServerNo, err := strconv.ParseInt(apiServer, 10, 32)
		if err != nil {
			return config, fmt.Errorf("env var %s must be a number, was %s: %w", envVarAPIServerPort, apiServer, err)
		}
		config.APIServerPort = int32(apiServerNo)
	case rawConfig.APIServerPort != 0:
		config.APIServerPort = rawConfig.APIServerPort
	default:
		// if nothing else set it, we set it to 0, to indicate that it should use whatever the kube-apiserver port is
		config.APIServerPort = 0
	}

	return config, nil
}

// printConfig report the config to startup logs
func printConfig(config Config) {
	lines := config.Strings()
	for _, l := range lines {
		klog.Info(l)
	}
}
