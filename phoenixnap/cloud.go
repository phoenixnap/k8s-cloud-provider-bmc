package phoenixnap

import (
	"context"
	"fmt"
	"io"
	"net/url"

	"github.com/phoenixnap/go-sdk-bmc/bmcapi/v3"
	"github.com/phoenixnap/go-sdk-bmc/ipapi/v3"
	netapi "github.com/phoenixnap/go-sdk-bmc/networkapi/v4"
	"github.com/phoenixnap/go-sdk-bmc/tagapi/v3"
	"golang.org/x/oauth2/clientcredentials"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/component-base/version"
	"k8s.io/klog/v2"
)

const (
	ProviderName string = "phoenixnap"

	// ConsumerToken token for metal consumer
	ConsumerToken string = "cloud-provider-phoenixnap"

	// tokenURL URL to open ID connect token endpoint
	tokenURL = "https://auth.phoenixnap.com/auth/realms/BMC/protocol/openid-connect/token"
)

// cloud implements cloudprovider.Interface
type cloud struct {
	bmcClient    *bmcapi.APIClient
	ipClient     *ipapi.APIClient
	tagClient    *tagapi.APIClient
	netClient    *netapi.APIClient
	config       Config
	instances    *instances
	loadBalancer *loadBalancers
}

var _ cloudprovider.Interface = (*cloud)(nil)

func newCloud(pnapConfig Config, bmcClient *bmcapi.APIClient, ipClient *ipapi.APIClient, tagClient *tagapi.APIClient, netClient *netapi.APIClient) (cloudprovider.Interface, error) {
	return &cloud{
		bmcClient: bmcClient,
		ipClient:  ipClient,
		tagClient: tagClient,
		netClient: netClient,
		config:    pnapConfig,
	}, nil
}

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		// by the time we get here, there is no error, as it would have been handled earlier
		pnapConfig, err := getConfig(config)
		// register the provider
		if err != nil {
			return nil, fmt.Errorf("provider config error: %w", err)
		}

		// report the config
		printConfig(pnapConfig)

		// set up our client and create the cloud interface

		// an API endpoint other than production usually has its own auth endpoint too
		authURL := tokenURL
		if pnapConfig.TokenURL != nil && *pnapConfig.TokenURL != "" {
			authURL = *pnapConfig.TokenURL
		}
		ccConfig := clientcredentials.Config{
			ClientID:     pnapConfig.ClientID,
			ClientSecret: pnapConfig.ClientSecret,
			TokenURL:     authURL,
			Scopes:       []string{"bmc", "bmc.read", "tags", "tags.read"},
		}
		// one client, and so one cached token source, shared by all of the API clients
		httpClient := ccConfig.Client(context.Background())
		userAgent := fmt.Sprintf("cloud-provider-phoenixnap/%s", version.Get())

		// optional override of the API endpoint; only scheme and host are replaced,
		// each API keeps its own path
		var host, scheme string
		if pnapConfig.BaseURL != nil && *pnapConfig.BaseURL != "" {
			u, err := url.Parse(*pnapConfig.BaseURL)
			if err != nil || u.Host == "" {
				return nil, fmt.Errorf("invalid base-url %q: must be of the form https://host[:port]", *pnapConfig.BaseURL)
			}
			host, scheme = u.Host, u.Scheme
		}

		bmcConfiguration := bmcapi.NewConfiguration()
		bmcConfiguration.HTTPClient = httpClient
		bmcConfiguration.UserAgent = userAgent
		bmcConfiguration.Host, bmcConfiguration.Scheme = host, scheme
		bmcClient := bmcapi.NewAPIClient(bmcConfiguration)

		ipConfiguration := ipapi.NewConfiguration()
		ipConfiguration.HTTPClient = httpClient
		ipConfiguration.UserAgent = userAgent
		ipConfiguration.Host, ipConfiguration.Scheme = host, scheme
		ipClient := ipapi.NewAPIClient(ipConfiguration)

		tagConfiguration := tagapi.NewConfiguration()
		tagConfiguration.HTTPClient = httpClient
		tagConfiguration.UserAgent = userAgent
		tagConfiguration.Host, tagConfiguration.Scheme = host, scheme
		tagClient := tagapi.NewAPIClient(tagConfiguration)

		netConfiguration := netapi.NewConfiguration()
		netConfiguration.HTTPClient = httpClient
		netConfiguration.UserAgent = userAgent
		netConfiguration.Host, netConfiguration.Scheme = host, scheme
		netClient := netapi.NewAPIClient(netConfiguration)

		cloud, err := newCloud(pnapConfig, bmcClient, ipClient, tagClient, netClient)
		if err != nil {
			return nil, fmt.Errorf("failed to create new cloud handler: %w", err)
		}
		// note that this is not fully initialized until it calls cloud.Initialize()

		return cloud, nil
	})
}

// Initialize provides the cloud with a kubernetes client builder and may spawn goroutines
// to perform housekeeping activities within the cloud provider.
func (c *cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	klog.V(5).Info("called Initialize")
	clientset := clientBuilder.ClientOrDie("cloud-provider-phoenixnap-shared-informers")

	// initialize the individual services
	lb, err := newLoadBalancers(c.ipClient, c.tagClient, c.netClient, clientset, c.config.Location, c.config.LoadBalancerSetting, c.config.AnnotationIPLocation, c.config.ServiceNodeSelector, stop)
	if err != nil {
		klog.Fatalf("could not initialize LoadBalancers: %v", err)
	}

	c.loadBalancer = lb
	c.instances = newInstances(c.bmcClient)

	klog.Info("Initialize of cloud provider complete")
}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	klog.V(5).Info("called LoadBalancer")
	return c.loadBalancer, c.loadBalancer != nil
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) Instances() (cloudprovider.Instances, bool) {
	klog.V(5).Info("called Instances")
	return nil, false
}

// InstancesV2 returns an implementation of cloudprovider.InstancesV2.
func (c *cloud) InstancesV2() (cloudprovider.InstancesV2, bool) {
	klog.V(5).Info("called InstancesV2")
	return c.instances, true
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
// DEPRECATED. Will not be called if InstancesV2 is implemented
func (c *cloud) Zones() (cloudprovider.Zones, bool) {
	klog.V(5).Info("called Zones")
	return nil, false
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (c *cloud) Clusters() (cloudprovider.Clusters, bool) {
	klog.V(5).Info("called Clusters")
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (c *cloud) Routes() (cloudprovider.Routes, bool) {
	klog.V(5).Info("called Routes")
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (c *cloud) ProviderName() string {
	klog.V(2).Infof("called ProviderName, returning %s", ProviderName)
	return ProviderName
}

// HasClusterID returns true if a ClusterID is required and set
func (c *cloud) HasClusterID() bool {
	klog.V(5).Info("called HasClusterID")
	return true
}
