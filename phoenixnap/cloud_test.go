package phoenixnap

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/phoenixnap/go-sdk-bmc/billingapi"
	"github.com/phoenixnap/go-sdk-bmc/bmcapi"
	"github.com/phoenixnap/go-sdk-bmc/ipapi"
	"github.com/phoenixnap/go-sdk-bmc/tagapi"
	pnapServer "github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/server"
	"github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/server/store"

	clientset "k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/component-base/version"
)

const (
	token             = "12345678"
	nodeName          = "ccm-test"
	validLocationName = "ASH"
	validProductName  = "d1.c1.small"
)

// mockControllerClientBuilder mock implementation of https://pkg.go.dev/k8s.io/cloud-provider#ControllerClientBuilder
// so we can pass it to cloud.Initialize()
type mockControllerClientBuilder struct {
}

func (m mockControllerClientBuilder) Config(name string) (*restclient.Config, error) {
	return &restclient.Config{}, nil
}
func (m mockControllerClientBuilder) ConfigOrDie(name string) *restclient.Config {
	return &restclient.Config{}
}
func (m mockControllerClientBuilder) Client(name string) (clientset.Interface, error) {
	return k8sfake.NewSimpleClientset(), nil
}
func (m mockControllerClientBuilder) ClientOrDie(name string) clientset.Interface {
	return k8sfake.NewSimpleClientset()
}

type apiServerError struct {
	t *testing.T
}

func (a *apiServerError) Error(err error) {
	a.t.Fatal(err)
}

// create a valid cloud with a client
func testGetValidCloud(t *testing.T, LoadBalancerSetting string) (*cloud, *store.Memory) {
	// mock endpoint so our client can handle it
	backend, _ := store.NewMemory()
	fake := pnapServer.Server{
		Store: backend,
		ErrorHandler: &apiServerError{
			t: t,
		},
	}
	// ensure we have a single location
	_, _ = backend.CreateLocation(validLocationName)
	ts := httptest.NewServer(fake.CreateHandler())

	url, _ := url.Parse(ts.URL)
	urlString := url.String()

	bmc, _, ip, tag, err := constructClients(token, urlString)
	if err != nil {
		t.Fatalf("unable to construct testing phoenixnap API client: %v", err)
	}

	// now just need to create a client
	config := Config{
		LoadBalancerSetting: LoadBalancerSetting,
	}
	c, _ := newCloud(config, bmc, ip, tag)
	ccb := &mockControllerClientBuilder{}
	c.Initialize(ccb, nil)

	return c.(*cloud), backend
}

func TestLoadBalancerDefaultDisabled(t *testing.T) {
	vc, _ := testGetValidCloud(t, "")
	response, supported := vc.LoadBalancer()
	var (
		expectedSupported = false
		expectedResponse  = response
	)
	if supported != expectedSupported {
		t.Errorf("supported returned %v instead of expected %v", supported, expectedSupported)
	}
	if response != expectedResponse {
		t.Errorf("value returned %v instead of expected %v", response, expectedResponse)
	}
}

func TestLoadBalancerPNAPL2(t *testing.T) {
	t.Skip("Test needs a k8s client to work")
	vc, _ := testGetValidCloud(t, "pnap-l2://")
	response, supported := vc.LoadBalancer()
	var (
		expectedSupported = true
		expectedResponse  = response
	)
	if supported != expectedSupported {
		t.Errorf("supported returned %v instead of expected %v", supported, expectedSupported)
	}
	if response != expectedResponse {
		t.Errorf("value returned %v instead of expected %v", response, expectedResponse)
	}
}

func TestInstances(t *testing.T) {
	vc, _ := testGetValidCloud(t, "")
	response, supported := vc.Instances()
	expectedSupported := false
	expectedResponse := cloudprovider.Instances(nil)
	if supported != expectedSupported {
		t.Errorf("supported returned %v instead of expected %v", supported, expectedSupported)
	}
	if response != expectedResponse {
		t.Errorf("value returned %v instead of expected %v", response, expectedResponse)
	}
}

func TestClusters(t *testing.T) {
	vc, _ := testGetValidCloud(t, "")
	response, supported := vc.Clusters()
	var (
		expectedSupported = false
		expectedResponse  cloudprovider.Clusters // defaults to nil
	)
	if supported != expectedSupported {
		t.Errorf("supported returned %v instead of expected %v", supported, expectedSupported)
	}
	if response != expectedResponse {
		t.Errorf("value returned %v instead of expected %v", response, expectedResponse)
	}

}

func TestRoutes(t *testing.T) {
	vc, _ := testGetValidCloud(t, "")
	response, supported := vc.Routes()
	var (
		expectedSupported = false
		expectedResponse  cloudprovider.Routes // defaults to nil
	)
	if supported != expectedSupported {
		t.Errorf("supported returned %v instead of expected %v", supported, expectedSupported)
	}
	if response != expectedResponse {
		t.Errorf("value returned %v instead of expected %v", response, expectedResponse)
	}

}
func TestProviderName(t *testing.T) {
	vc, _ := testGetValidCloud(t, "")
	name := vc.ProviderName()
	if name != ProviderName {
		t.Errorf("returned %s instead of expected %s", name, ProviderName)
	}
}

func TestHasClusterID(t *testing.T) {
	vc, _ := testGetValidCloud(t, "")
	cid := vc.HasClusterID()
	expectedCid := true
	if cid != expectedCid {
		t.Errorf("returned %v instead of expected %v", cid, expectedCid)
	}

}

// builds a phoenixnap client
func constructClients(authToken, baseURL string) (bmc *bmcapi.APIClient, billing *billingapi.APIClient, ip *ipapi.APIClient, tag *tagapi.APIClient, err error) {
	// set up our client and create the cloud interface

	var u *url.URL
	u, err = url.Parse(baseURL)
	if err != nil {
		return
	}

	// these are for overriding oauth
	//nolint: revive // because the consuming client specifically checks for the string "accessToken"
	ctx := context.WithValue(context.Background(), "accessToken", "RANDOMSTRING")
	//nolint: revive // because the consuming client specifically checks for the string "serverIndex"
	ctx = context.WithValue(ctx, "serverIndex", nil)

	bmcConfiguration := bmcapi.NewConfiguration()
	bmcConfiguration.UserAgent = fmt.Sprintf("cloud-provider-phoenixnap/%s", version.Get())

	// these are for changing the server target
	bmcConfiguration.Host = u.Host
	bmcConfiguration.Scheme = u.Scheme
	bmc = bmcapi.NewAPIClient(bmcConfiguration)

	billingConfiguration := billingapi.NewConfiguration()
	billingConfiguration.UserAgent = fmt.Sprintf("cloud-provider-phoenixnap/%s", version.Get())

	// these are for changing the server target
	billingConfiguration.Host = u.Host
	billingConfiguration.Scheme = u.Scheme
	billing = billingapi.NewAPIClient(billingConfiguration)

	ipConfiguration := ipapi.NewConfiguration()
	ipConfiguration.UserAgent = fmt.Sprintf("cloud-provider-phoenixnap/%s", version.Get())

	// these are for changing the server target
	ipConfiguration.Host = u.Host
	ipConfiguration.Scheme = u.Scheme
	ip = ipapi.NewAPIClient(ipConfiguration)

	tagConfiguration := tagapi.NewConfiguration()
	tagConfiguration.UserAgent = fmt.Sprintf("cloud-provider-phoenixnap/%s", version.Get())

	// these are for changing the server target
	tagConfiguration.Host = u.Host
	tagConfiguration.Scheme = u.Scheme
	tag = tagapi.NewAPIClient(tagConfiguration)

	return

}
