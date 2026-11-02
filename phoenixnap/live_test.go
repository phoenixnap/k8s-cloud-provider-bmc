//go:build live

// Live tests against the production PhoenixNAP API. They are excluded from normal
// test runs by the "live" build tag, and the ones that create billable resources
// need explicit opt-in env vars. Credentials come from PNAP_CLIENT_ID and
// PNAP_CLIENT_SECRET, as for the CCM itself.
//
//	go test -tags live -run TestLiveReadOnly -v ./phoenixnap/
//	PNAP_LIVE_LOCATION=PHX go test -tags live -run TestLiveLoadBalancer -v -timeout 30m ./phoenixnap/
//	PNAP_LIVE_LOCATION=PHX PNAP_LIVE_SERVER_TYPE=s0.d1.small go test -tags live -run TestLiveInstances -v -timeout 90m ./phoenixnap/
//	PNAP_LIVE_SERVER_ID=<existing id> go test -tags live -run TestLiveInstances -v ./phoenixnap/
package phoenixnap

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/phoenixnap/go-sdk-bmc/billingapi/v4"
	"github.com/phoenixnap/go-sdk-bmc/bmcapi/v3"
	netapi "github.com/phoenixnap/go-sdk-bmc/networkapi/v4"
	"golang.org/x/oauth2/clientcredentials"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	cloudprovider "k8s.io/cloud-provider"
)

// liveCloud builds the provider exactly as the CCM does at startup, from env vars only
func liveCloud(t *testing.T) *cloud {
	t.Helper()
	if os.Getenv(clientIDName) == "" || os.Getenv(clientSecretName) == "" {
		t.Skipf("%s and %s must be set", clientIDName, clientSecretName)
	}
	c, err := cloudprovider.GetCloudProvider(ProviderName, nil)
	if err != nil {
		t.Fatalf("unable to initialize provider: %v", err)
	}
	return c.(*cloud)
}

// liveHTTPClient is an authenticated client with the API client's default scopes,
// for the test's own setup and teardown calls outside of the provider
func liveHTTPClient() *http.Client {
	cc := clientcredentials.Config{
		ClientID:     os.Getenv(clientIDName),
		ClientSecret: os.Getenv(clientSecretName),
		TokenURL:     tokenURL,
	}
	return cc.Client(context.Background())
}

func liveRequire(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if os.Getenv(n) == "" {
			t.Skipf("set %s to run this test; it creates billable resources", n)
		}
	}
}

// TestLiveReadOnly checks authentication and scopes through the provider's own clients,
// and reports which server products are available and what they cost. It creates nothing.
func TestLiveReadOnly(t *testing.T) {
	c := liveCloud(t)
	ctx := context.Background()

	servers, _, err := c.bmcClient.ServersAPI.ServersGet(ctx).Execute()
	if err != nil {
		t.Fatalf("provider bmc client cannot list servers: %v", err)
	}
	t.Logf("provider bmc client OK: %d servers in account", len(servers))
	for _, s := range servers {
		t.Logf("  server %s %s %s %s %s", s.Id, s.Hostname, s.Type, s.Location, s.Status)
	}
	blocks, _, err := c.ipClient.IPBlocksAPI.IpBlocksGet(ctx).Execute()
	if err != nil {
		t.Errorf("provider ip client cannot list IP blocks: %v", err)
	} else {
		t.Logf("provider ip client OK: %d IP blocks in account", len(blocks))
	}
	tags, _, err := c.tagClient.TagsAPI.TagsGet(ctx).Execute()
	if err != nil {
		t.Errorf("provider tag client cannot list tags: %v", err)
	} else {
		t.Logf("provider tag client OK: %d tags in account", len(tags))
	}
	networks, _, err := c.netClient.PublicNetworksAPI.PublicNetworksGet(ctx).Execute()
	if err != nil {
		t.Errorf("provider network client cannot list public networks: %v", err)
	} else {
		t.Logf("provider network client OK: %d public networks in account", len(networks))
	}

	// what can be provisioned right now, cheapest hourly first
	billingConfig := billingapi.NewConfiguration()
	billingConfig.HTTPClient = liveHTTPClient()
	billing := billingapi.NewAPIClient(billingConfig)
	availability, _, err := billing.ProductAvailabilityAPI.ProductAvailabilityGet(ctx).ProductCategory([]string{serverCategory}).ShowOnlyMinQuantityAvailable(true).Execute()
	if err != nil {
		t.Fatalf("unable to get product availability: %v", err)
	}
	available := map[string]float32{}
	for _, p := range availability {
		for _, l := range p.LocationAvailabilityDetails {
			if l.MinQuantityAvailable && l.AvailableQuantity > 0 {
				available[p.ProductCode+"/"+string(l.Location)] = l.AvailableQuantity
			}
		}
	}
	type offer struct {
		code, location string
		price, net     float32
		quantity       float32
	}
	var offers []offer
	for _, p := range liveProducts(t, billing, serverCategory) {
		for _, plan := range p.Plans {
			if q, ok := available[p.ProductCode+"/"+plan.Location]; ok && plan.PricingModel == "HOURLY" {
				offers = append(offers, offer{p.ProductCode, plan.Location, plan.Price, netPrice(plan), q})
			}
		}
	}
	sort.Slice(offers, func(i, j int) bool {
		return offers[i].net < offers[j].net || (offers[i].net == offers[j].net && offers[i].price < offers[j].price)
	})
	t.Logf("%d available hourly server offers, cheapest first (list price, price after this account's discounts):", len(offers))
	for i, o := range offers {
		if i == 15 {
			break
		}
		t.Logf("  %-16s %s  $%.2f/hour, $%.2f/hour after discounts  (%v available)", o.code, o.location, o.price, o.net, o.quantity)
	}

	// the load balancer creates one block of this size per Service
	blockProduct := fmt.Sprintf("range-%d", serviceBlockCidr)
	for _, p := range liveProducts(t, billing, "PUBLIC_IP") {
		if p.ProductCode != blockProduct {
			continue
		}
		for _, plan := range p.Plans {
			t.Logf("/%d IP block (%s) %s %s: $%.2f per %s, $%.2f after discounts", serviceBlockCidr, p.ProductCode, plan.Location, plan.PricingModel, plan.Price, plan.PriceUnit, netPrice(plan))
		}
	}
}

// netPrice is the price after this account's discounts
func netPrice(plan billingapi.PricingPlan) float32 {
	if d := plan.ApplicableDiscounts; d != nil && d.DiscountedPrice != nil {
		return *d.DiscountedPrice
	}
	return plan.Price
}

type liveProduct struct {
	ProductCode string
	Plans       []billingapi.PricingPlan
}

// liveProducts returns the products of a category, whichever of the SDK's product types each one decodes to
func liveProducts(t *testing.T, billing *billingapi.APIClient, category string) []liveProduct {
	t.Helper()
	products, _, err := billing.ProductsAPI.ProductsGet(context.Background()).ProductCategory(category).Execute()
	if err != nil {
		t.Fatalf("unable to get %s products: %v", category, err)
	}
	var ret []liveProduct
	for _, p := range products {
		switch {
		case p.ServerProduct != nil:
			ret = append(ret, liveProduct{p.ServerProduct.ProductCode, p.ServerProduct.Plans})
		case p.Product != nil:
			ret = append(ret, liveProduct{p.Product.ProductCode, p.Product.Plans})
		}
	}
	return ret
}

// TestLiveInstances checks InstancesV2 against a real server: an existing one from
// PNAP_LIVE_SERVER_ID, or a new hourly one of PNAP_LIVE_SERVER_TYPE in PNAP_LIVE_LOCATION,
// which is deleted at the end.
func TestLiveInstances(t *testing.T) {
	c := liveCloud(t)
	ctx := context.Background()

	id := os.Getenv("PNAP_LIVE_SERVER_ID")
	if id == "" {
		liveRequire(t, "PNAP_LIVE_SERVER_TYPE", "PNAP_LIVE_LOCATION")
		id = liveCreateServer(t, os.Getenv("PNAP_LIVE_SERVER_TYPE"), os.Getenv("PNAP_LIVE_LOCATION"))
	}

	inst := newInstances(c.bmcClient)
	server, err := serverByID(ctx, c.bmcClient, id)
	if err != nil {
		t.Fatalf("serverByID(%s): %v", id, err)
	}

	byID := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ignored"}, Spec: v1.NodeSpec{ProviderID: "phoenixnap://" + id}}
	byName := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: server.Hostname}}
	for name, node := range map[string]*v1.Node{"by provider ID": byID, "by name": byName} {
		md, err := inst.InstanceMetadata(ctx, node)
		if err != nil {
			t.Errorf("InstanceMetadata %s: %v", name, err)
			continue
		}
		t.Logf("InstanceMetadata %s: providerID=%s type=%s region=%s addresses=%v", name, md.ProviderID, md.InstanceType, md.Region, md.NodeAddresses)
		if md.ProviderID != "phoenixnap://"+id || md.InstanceType != server.Type || md.Region != server.Location {
			t.Errorf("InstanceMetadata %s returned %+v for server %+v", name, md, server)
		}
	}

	exists, err := inst.InstanceExists(ctx, byID)
	if err != nil || !exists {
		t.Errorf("InstanceExists = %v, %v; expected true", exists, err)
	}
	shutdown, err := inst.InstanceShutdown(ctx, byID)
	if err != nil || shutdown != (server.Status == string(InstanceStatusPoweredOff)) {
		t.Errorf("InstanceShutdown = %v, %v for status %s", shutdown, err, server.Status)
	}

	gone := &v1.Node{Spec: v1.NodeSpec{ProviderID: "phoenixnap://" + uuid.New().String()}}
	if exists, err := inst.InstanceExists(ctx, gone); err != nil || exists {
		t.Errorf("InstanceExists for unknown server = %v, %v; expected false, nil", exists, err)
	}
	if _, err := serverByName(ctx, c.bmcClient, types.NodeName("no-such-host-"+uuid.New().String()[:8])); err != cloudprovider.InstanceNotFound {
		t.Errorf("serverByName for unknown host returned %v; expected InstanceNotFound", err)
	}
}

func liveCreateServer(t *testing.T, serverType, location string) string {
	t.Helper()
	ctx := context.Background()
	config := bmcapi.NewConfiguration()
	config.HTTPClient = liveHTTPClient()
	bmc := bmcapi.NewAPIClient(config)

	osName := os.Getenv("PNAP_LIVE_OS")
	if osName == "" {
		// noble supports PhoenixNAP's rapid provisioning, which takes minutes instead of tens of minutes
		osName = "ubuntu/noble"
	}
	hourly := "HOURLY"
	req := bmcapi.NewServerCreate("ccm-live-"+uuid.New().String()[:8], osName, serverType, location)
	req.PricingModel = &hourly
	server, _, err := bmc.ServersAPI.ServersPost(ctx).ServerCreate(*req).Execute()
	if err != nil {
		t.Fatalf("unable to create %s server in %s: %v", serverType, location, err)
	}
	t.Logf("created server %s (%s) %s in %s, status %s", server.Id, server.Hostname, serverType, location, server.Status)
	t.Cleanup(func() {
		// retry, as a server cannot always be deleted the moment it finishes provisioning
		for i := 0; i < 20; i++ {
			if _, _, err := bmc.ServersAPI.ServersServerIdDelete(context.Background(), server.Id).Execute(); err == nil {
				t.Logf("deleted server %s", server.Id)
				return
			} else if i == 19 {
				t.Errorf("UNABLE TO DELETE SERVER %s, delete it manually: %v", server.Id, err)
			}
			time.Sleep(30 * time.Second)
		}
	})

	deadline := time.Now().Add(60 * time.Minute)
	for time.Now().Before(deadline) {
		s, _, err := bmc.ServersAPI.ServersServerIdGet(ctx, server.Id).Execute()
		switch {
		case err != nil:
			t.Logf("polling server %s: %v", server.Id, err)
		case s.Status == string(InstanceStatusPoweredOn):
			t.Logf("server %s is powered-on", server.Id)
			return server.Id
		case s.Status == string(InstanceStatusError):
			t.Fatalf("server %s failed to provision", server.Id)
		default:
			t.Logf("server %s status %s", server.Id, s.Status)
		}
		time.Sleep(60 * time.Second)
	}
	t.Fatalf("server %s did not power on within 60 minutes", server.Id)
	return ""
}

// TestLiveLoadBalancer runs the Service type=LoadBalancer lifecycle against the real IP, network
// and tag APIs: a new public network in PNAP_LIVE_LOCATION, and a /29 IP block that is assigned,
// released, unassigned and deleted. The network is deleted at the end.
func TestLiveLoadBalancer(t *testing.T) {
	liveRequire(t, "PNAP_LIVE_LOCATION")
	c := liveCloud(t)
	ctx := context.Background()
	location := os.Getenv("PNAP_LIVE_LOCATION")

	// the CCM creates the tag names it needs; remember which already existed, to remove only the new ones
	existingTags := liveTagNames(t, c)

	network, _, err := c.netClient.PublicNetworksAPI.PublicNetworksPost(ctx).PublicNetworkCreate(*netapi.NewPublicNetworkCreate("ccm-live-"+uuid.New().String()[:8], location)).Execute()
	if err != nil {
		t.Fatalf("unable to create public network in %s: %v", location, err)
	}
	t.Logf("created public network %s (%s) in %s", network.Id, network.Name, location)

	// a unique cluster per run, so the tags only ever match this run's blocks
	clusterUID := uuid.New().String()
	k8sclient := k8sfake.NewSimpleClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(clusterUID)}})
	stop := make(chan struct{})
	l, err := newLoadBalancers(c.ipClient, c.tagClient, c.netClient, k8sclient, location, "kube-vip://"+network.Id, DefaultAnnotationIPLocation, "", stop)
	if err != nil {
		t.Fatalf("newLoadBalancers: %v", err)
	}
	t.Cleanup(func() {
		close(stop)
		liveCleanupBlocks(t, l)
		liveCleanupTags(t, c, existingTags)
		if _, err := c.netClient.PublicNetworksAPI.PublicNetworksNetworkIdDelete(context.Background(), network.Id).Execute(); err != nil {
			t.Errorf("UNABLE TO DELETE PUBLIC NETWORK %s, delete it manually: %v", network.Id, err)
		} else {
			t.Logf("deleted public network %s", network.Id)
		}
	})

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ccm-live"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: []v1.ServicePort{{Port: 80}}},
	}
	if _, err := k8sclient.CoreV1().Services("default").Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("unable to create service: %v", err)
	}

	status, err := l.EnsureLoadBalancer(ctx, "", svc, nil)
	if err != nil {
		t.Fatalf("EnsureLoadBalancer: %v", err)
	}
	ip := status.Ingress[0].IP
	t.Logf("EnsureLoadBalancer assigned %s", ip)

	blocks, err := l.getIPBlocks(ctx, "default", "ccm-live", true, false)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("expected one active block for the service, got %d: %v", len(blocks), err)
	}
	block := blocks[0]
	prefix, err := netip.ParsePrefix(block.GetCidr())
	if err != nil || !prefix.Contains(netip.MustParseAddr(ip)) {
		t.Errorf("ingress IP %s is not in block %s", ip, block.GetCidr())
	}
	if block.AssignedResourceId == nil || *block.AssignedResourceId != network.Id {
		t.Errorf("block %s is not assigned to network %s: %+v", block.GetId(), network.Id, block)
	}
	t.Logf("block %s %s status %s assigned to %v", block.GetId(), block.GetCidr(), block.GetStatus(), deref(block.AssignedResourceId))

	// idempotent: a second Ensure returns the same IP and creates nothing
	updated, _ := k8sclient.CoreV1().Services("default").Get(ctx, "ccm-live", metav1.GetOptions{})
	status, err = l.EnsureLoadBalancer(ctx, "", updated, nil)
	if err != nil || status.Ingress[0].IP != ip {
		t.Errorf("second EnsureLoadBalancer returned %v, %v; expected %s", status, err, ip)
	}

	if err := l.EnsureLoadBalancerDeleted(ctx, "", updated); err != nil {
		t.Fatalf("EnsureLoadBalancerDeleted: %v", err)
	}
	if active, _ := l.getIPBlocks(ctx, "default", "ccm-live", true, false); len(active) != 0 {
		t.Errorf("block still active after EnsureLoadBalancerDeleted: %+v", active)
	}

	// drive the reaper by hand until the block is unassigned and deleted
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		l.reapBlocksOnce(ctx)
		if _, _, err := c.ipClient.IPBlocksAPI.IpBlocksIpBlockIdGet(ctx, block.GetId()).Execute(); err != nil {
			t.Logf("block %s deleted by the reaper", block.GetId())
			return
		}
		time.Sleep(20 * time.Second)
	}
	t.Errorf("block %s was not deleted by the reaper within 15 minutes", block.GetId())
}

func liveTagNames(t *testing.T, c *cloud) map[string]bool {
	t.Helper()
	tags, _, err := c.tagClient.TagsAPI.TagsGet(context.Background()).Execute()
	if err != nil {
		t.Fatalf("unable to list tags: %v", err)
	}
	names := map[string]bool{}
	for _, tag := range tags {
		names[tag.Name] = true
	}
	return names
}

// ccmTagNames are the tag names the CCM creates for its IP blocks
func ccmTagNames() []string {
	clsTag, _ := clusterTag("")
	return []string{pnapTag, clsTag, serviceNamespaceTag, serviceNameTag, deleteTag}
}

// liveCleanupTags deletes the CCM's tag names that did not exist before the test, if nothing uses them
func liveCleanupTags(t *testing.T, c *cloud, existing map[string]bool) {
	ctx := context.Background()
	tags, _, err := c.tagClient.TagsAPI.TagsGet(ctx).Execute()
	if err != nil {
		t.Errorf("unable to list tags for cleanup: %v", err)
		return
	}
	for _, tag := range tags {
		if existing[tag.Name] || !contains(ccmTagNames(), tag.Name) {
			continue
		}
		if len(tag.ResourceAssignments) > 0 {
			t.Logf("keeping tag %s, still assigned to %d resources", tag.Name, len(tag.ResourceAssignments))
			continue
		}
		if err := liveRetry(func() error {
			_, _, err := c.tagClient.TagsAPI.TagsTagIdDelete(ctx, tag.Id).Execute()
			return err
		}); err != nil {
			t.Errorf("UNABLE TO DELETE TAG %s (%s), delete it manually: %v", tag.Name, tag.Id, err)
		} else {
			t.Logf("deleted tag %s created by this test", tag.Name)
		}
	}
}

// liveRetry retries a cleanup call, as the API returns occasional transient errors such as 503
func liveRetry(f func() error) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = f(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Second)
	}
	return err
}

func contains(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

// liveCleanupBlocks removes every block of this run's cluster, whatever state the test left it in
func liveCleanupBlocks(t *testing.T, l *loadBalancers) {
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		blocks, err := l.getIPBlocks(ctx, "", "", true, true)
		if err == nil && len(blocks) == 0 {
			return
		}
		for _, b := range blocks {
			if b.AssignedResourceId != nil {
				_, _, _ = l.netClient.PublicNetworksAPI.PublicNetworksNetworkIdIpBlocksIpBlockIdDelete(ctx, *b.AssignedResourceId, b.GetId()).Execute()
			} else {
				_, _, _ = l.ipClient.IPBlocksAPI.IpBlocksIpBlockIdDelete(ctx, b.GetId()).Execute()
			}
		}
		time.Sleep(20 * time.Second)
	}
	t.Errorf("UNABLE TO CLEAN UP IP BLOCKS for cluster %s, delete blocks tagged with it manually", l.clusterID)
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestLiveLeftovers fails if the account holds anything the live tests or the CCM created:
// servers or public networks named ccm-live-*, or IP blocks tagged as created by the CCM.
// It only reads; run it after the other live tests, while no CCM uses the account.
func TestLiveLeftovers(t *testing.T) {
	c := liveCloud(t)
	ctx := context.Background()

	servers, _, err := c.bmcClient.ServersAPI.ServersGet(ctx).Execute()
	if err != nil {
		t.Fatalf("unable to list servers: %v", err)
	}
	for _, s := range servers {
		if strings.HasPrefix(s.Hostname, "ccm-live-") {
			t.Errorf("leftover server %s %s, status %s", s.Id, s.Hostname, s.Status)
		}
	}
	networks, _, err := c.netClient.PublicNetworksAPI.PublicNetworksGet(ctx).Execute()
	if err != nil {
		t.Fatalf("unable to list public networks: %v", err)
	}
	for _, n := range networks {
		if strings.HasPrefix(n.Name, "ccm-live-") {
			t.Errorf("leftover public network %s %s", n.Id, n.Name)
		}
	}
	blocks, _, err := c.ipClient.IPBlocksAPI.IpBlocksGet(ctx).Tag([]string{fmt.Sprintf("%s.%s", pnapTag, pnapValue)}).Execute()
	if err != nil {
		t.Fatalf("unable to list IP blocks: %v", err)
	}
	for _, b := range blocks {
		t.Errorf("leftover CCM IP block %s %s, status %s", b.GetId(), b.GetCidr(), b.GetStatus())
	}
	// unused CCM tag names are not an error, as a CCM legitimately creates them, but are worth knowing
	// about; PNAP_LIVE_REMOVE_UNUSED_CCM_TAGS=1 deletes them, e.g. after an interrupted live run
	remove := os.Getenv("PNAP_LIVE_REMOVE_UNUSED_CCM_TAGS") == "1"
	tags, _, err := c.tagClient.TagsAPI.TagsGet(ctx).Execute()
	if err != nil {
		t.Fatalf("unable to list tags: %v", err)
	}
	for _, tag := range tags {
		if !contains(ccmTagNames(), tag.Name) || len(tag.ResourceAssignments) > 0 {
			continue
		}
		if !remove {
			t.Logf("unused CCM tag name %s (%s)", tag.Name, tag.Id)
			continue
		}
		if err := liveRetry(func() error {
			_, _, err := c.tagClient.TagsAPI.TagsTagIdDelete(ctx, tag.Id).Execute()
			return err
		}); err != nil {
			t.Errorf("unable to delete unused CCM tag %s (%s): %v", tag.Name, tag.Id, err)
		} else {
			t.Logf("deleted unused CCM tag %s (%s)", tag.Name, tag.Id)
		}
	}
	t.Logf("checked %d servers, %d public networks, %d CCM-tagged IP blocks, %d tags", len(servers), len(networks), len(blocks), len(tags))
}
