package phoenixnap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/phoenixnap/go-sdk-bmc/bmcapi/v3"
	"github.com/phoenixnap/go-sdk-bmc/ipapi/v3"
	"github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/loadbalancers/kubevip"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

const testNetworkID = "net-1"

// fakeIPAPI serves the parts of the ip and network APIs used by the load balancer, recording each request
type fakeIPAPI struct {
	mu       sync.Mutex
	blocks   []ipapi.IpBlock
	requests []string
}

func (f *fakeIPAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Path == "/ips/v1/ip-blocks" {
		_ = json.NewEncoder(w).Encode(f.blocks)
		return
	}
	_, _ = w.Write([]byte("{}"))
}

func (f *fakeIPAPI) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ret := append([]string(nil), f.requests...)
	sort.Strings(ret)
	return ret
}

func testLoadBalancers(t *testing.T, blocks []ipapi.IpBlock, objects ...*v1.Service) (*loadBalancers, *fakeIPAPI, *k8sfake.Clientset) {
	api := &fakeIPAPI{blocks: blocks}
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	_, _, ip, tag, netClient, err := constructClients(token, ts.URL)
	if err != nil {
		t.Fatalf("unable to construct clients: %v", err)
	}
	k8sclient := k8sfake.NewSimpleClientset()
	for _, svc := range objects {
		if _, err := k8sclient.CoreV1().Services(svc.Namespace).Create(context.Background(), svc, metav1.CreateOptions{}); err != nil {
			t.Fatalf("unable to create service: %v", err)
		}
	}
	return &loadBalancers{
		ipClient:             ip,
		tagClient:            tag,
		netClient:            netClient,
		k8sclient:            k8sclient,
		clusterID:            "cluster-1",
		implementor:          kubevip.NewLB(k8sclient, ""),
		ipLocationAnnotation: DefaultAnnotationIPLocation,
		network:              testNetworkID,
		nodeSelector:         labels.Everything(),
	}, api, k8sclient
}

func deleteTagged(id, status string, assignedTo *string) ipapi.IpBlock {
	val := activeValue
	return ipapi.IpBlock{
		Id:                 ipapi.PtrString(id),
		Status:             ipapi.PtrString(status),
		AssignedResourceId: assignedTo,
		Tags:               []ipapi.TagAssignment{{Name: deleteTag, Value: &val}},
	}
}

// every block marked for deletion is handled, not just the first one
func TestReapBlocksOnce(t *testing.T) {
	network := testNetworkID
	l, api, _ := testLoadBalancers(t, []ipapi.IpBlock{
		deleteTagged("a", "assigned", &network),
		deleteTagged("b", "assigned", &network),
		deleteTagged("c", "unassigned", nil),
		deleteTagged("d", "unassigning", &network),
		{Id: ipapi.PtrString("e"), Status: ipapi.PtrString("assigned"), AssignedResourceId: &network}, // not marked for deletion
	})

	l.reapBlocksOnce(context.Background())

	expected := []string{
		"DELETE /ips/v1/ip-blocks/c",
		"DELETE /networks/v1/public-networks/net-1/ip-blocks/a",
		"DELETE /networks/v1/public-networks/net-1/ip-blocks/b",
		"GET /ips/v1/ip-blocks",
	}
	actual := api.recorded()
	if len(actual) != len(expected) {
		t.Fatalf("requests %v, expected %v", actual, expected)
	}
	for i := range expected {
		if actual[i] != expected[i] {
			t.Errorf("request %d: %s, expected %s", i, actual[i], expected[i])
		}
	}
}

// EnsureLoadBalancer returns the block's IP as ingress, and records it on the service
func TestEnsureLoadBalancerExistingBlock(t *testing.T) {
	network := testNetworkID
	resourceType := publicNetworkCaps
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	l, _, k8sclient := testLoadBalancers(t, []ipapi.IpBlock{{
		Id:                   ipapi.PtrString("a"),
		Cidr:                 ipapi.PtrString("192.0.2.8/29"),
		Status:               ipapi.PtrString("assigned"),
		AssignedResourceId:   &network,
		AssignedResourceType: &resourceType,
	}}, svc)

	status, err := l.EnsureLoadBalancer(context.Background(), "", svc, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(status.Ingress) != 1 || status.Ingress[0].IP != "192.0.2.10" {
		t.Fatalf("ingress %v, expected 192.0.2.10", status.Ingress)
	}

	updated, err := k8sclient.CoreV1().Services("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unable to get service: %v", err)
	}
	if updated.Spec.LoadBalancerIP != "192.0.2.10" {
		t.Errorf("spec.loadBalancerIP %q, expected 192.0.2.10", updated.Spec.LoadBalancerIP)
	}
	if ip := updated.Annotations[kubeVipLoadBalancerIPsAnnotation]; ip != "192.0.2.10" {
		t.Errorf("annotation %s %q, expected 192.0.2.10", kubeVipLoadBalancerIPsAnnotation, ip)
	}

	// once the IP is on the service, GetLoadBalancer reports it
	status, exists, err := l.GetLoadBalancer(context.Background(), "", updated)
	if err != nil || !exists || status.Ingress[0].IP != "192.0.2.10" {
		t.Errorf("GetLoadBalancer returned %v, %v, %v", status, exists, err)
	}
}

func TestServiceLocation(t *testing.T) {
	annotated := &v1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:        "web",
		Annotations: map[string]string{DefaultAnnotationIPLocation: "PHX"},
	}}
	plain := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web"}}

	tests := []struct {
		testName string
		global   string
		svc      *v1.Service
		location string
		err      bool
	}{
		{"global wins", "ASH", annotated, "ASH", false},
		{"annotation", "", annotated, "PHX", false},
		{"none", "", plain, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			l := &loadBalancers{location: tt.global, ipLocationAnnotation: DefaultAnnotationIPLocation}
			location, err := l.serviceLocation(tt.svc)
			if (err != nil) != tt.err {
				t.Fatalf("error %v, expected error %v", err, tt.err)
			}
			if location != tt.location {
				t.Errorf("location %q, expected %q", location, tt.location)
			}
		})
	}
}

func TestNodeAddressesPrivateOnly(t *testing.T) {
	addresses, err := nodeAddresses(bmcapi.Server{Hostname: "n1", PrivateIpAddresses: []string{"10.0.0.2"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(addresses) != 2 {
		t.Errorf("addresses %v, expected hostname and internal IP", addresses)
	}
	if _, err := nodeAddresses(bmcapi.Server{Hostname: "n1", PublicIpAddresses: []string{"192.0.2.1"}}); err == nil {
		t.Error("expected error for server without private IP")
	}
}

func TestNewLoadBalancersInvalidConfig(t *testing.T) {
	k8sclient := k8sfake.NewSimpleClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "uid"}})
	if _, err := newLoadBalancers(nil, nil, nil, k8sclient, "ASH", "kube-vip://net", "", "a b c", nil); err == nil {
		t.Error("expected error for invalid node selector")
	}
	if _, err := newLoadBalancers(nil, nil, nil, k8sclient, "ASH", "metallb://net", "", "", nil); err == nil {
		t.Error("expected error for unsupported loadbalancer type")
	}
}

// the API reports IP blocks assigned to a server as "first - last" ranges
func TestNodeAddressesRanges(t *testing.T) {
	addresses, err := nodeAddresses(bmcapi.Server{
		Id:                 "s1",
		Hostname:           "n1",
		PublicIpAddresses:  []string{"203.0.113.10 - 203.0.113.14", "203.0.113.77", "not-an-ip", "1.2.3.4 - nope"},
		PrivateIpAddresses: []string{"10.20.0.14"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []v1.NodeAddress{
		{Type: v1.NodeHostName, Address: "n1"},
		{Type: v1.NodeExternalIP, Address: "203.0.113.10"},
		{Type: v1.NodeExternalIP, Address: "203.0.113.77"},
		{Type: v1.NodeInternalIP, Address: "10.20.0.14"},
	}
	if !compareAddresses(addresses, expected) {
		t.Errorf("addresses %v, expected %v", addresses, expected)
	}
	if _, err := nodeAddresses(bmcapi.Server{Hostname: "n1", PrivateIpAddresses: []string{"garbage"}}); err == nil {
		t.Error("expected error when no private address is a valid IP")
	}
}
