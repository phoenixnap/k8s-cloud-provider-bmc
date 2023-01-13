package phoenixnap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cloudprovider "k8s.io/cloud-provider"
)

// testNode provides a simple Node object satisfying the lookup requirements of InstanceMetadata()
func testNode(providerID, nodeName string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec: v1.NodeSpec{
			ProviderID: providerID,
		},
	}
}

func TestNodeAddresses(t *testing.T) {
	vc, backend := testGetValidCloud(t, "")
	inst, _ := vc.InstancesV2()
	if inst == nil {
		t.Fatal("inst is nil")
	}
	serverName := testGetNewServerName()
	location, err := testGetOrCreateValidLocation(validLocationName, backend)
	if err != nil {
		t.Fatalf("unable to get or create valid location %s: %v", validLocationName, err)
	}
	product, err := testGetOrCreateValidServerProduct(validProductName, location, backend)
	if err != nil {
		t.Fatalf("unable to get or create valid server product %s: %v", validProductName, err)
	}
	server, err := backend.CreateServer(serverName, product.ProductCode, location)
	if err != nil {
		t.Fatalf("unable to get or create server %s at %s: %v", validProductName, location, err)
	}

	validAddresses := []v1.NodeAddress{
		{Type: v1.NodeHostName, Address: serverName},
		{Type: v1.NodeInternalIP, Address: server.PrivateIpAddresses[0]},
		{Type: v1.NodeExternalIP, Address: server.PublicIpAddresses[0]},
	}

	tests := []struct {
		testName  string
		node      *v1.Node
		addresses []v1.NodeAddress
		err       error
	}{
		{"empty node name", testNode("", ""), nil, fmt.Errorf("node name cannot be empty")},
		{"empty ID", testNode("", nodeName), nil, cloudprovider.InstanceNotFound},
		{"invalid id", testNode("phoenixnap://abc123", nodeName), nil, cloudprovider.InstanceNotFound},
		{"unknown id", testNode(fmt.Sprintf("phoenixnap://%s", randomID), nodeName), nil, cloudprovider.InstanceNotFound},
		{"valid both", testNode(fmt.Sprintf("phoenixnap://%s", server.Id), serverName), validAddresses, nil},
		{"valid provider id", testNode(fmt.Sprintf("phoenixnap://%s", server.Id), nodeName), validAddresses, nil},
		{"valid node name", testNode("", serverName), validAddresses, nil},
	}

	for i, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			var addresses []v1.NodeAddress

			md, err := inst.InstanceMetadata(context.TODO(), tt.node)
			if md != nil {
				addresses = md.NodeAddresses
			}
			switch {
			case (err == nil && tt.err != nil) || (err != nil && tt.err == nil) || (err != nil && tt.err != nil && !strings.HasPrefix(err.Error(), tt.err.Error())):
				t.Errorf("%d: mismatched errors, actual %v expected %v", i, err, tt.err)
			case !compareAddresses(addresses, tt.addresses):
				t.Errorf("%d: mismatched addresses, actual %v expected %v", i, addresses, tt.addresses)
			}
		})
	}
}

func TestInstanceType(t *testing.T) {
	vc, backend := testGetValidCloud(t, "")
	inst, _ := vc.InstancesV2()
	serverName := testGetNewServerName()
	location, _ := testGetOrCreateValidLocation(validLocationName, backend)
	product, _ := testGetOrCreateValidServerProduct(validProductName, location, backend)
	server, err := backend.CreateServer(serverName, product.ProductCode, location)
	if err != nil {
		t.Fatalf("unable to get or create server %s at %s: %v", validProductName, location, err)
	}

	tests := []struct {
		testName string
		name     string
		plan     string
		err      error
	}{
		{"empty name", "", "", cloudprovider.InstanceNotFound},
		{"invalid id", "thisdoesnotexist", "", cloudprovider.InstanceNotFound},
		{"unknown name", randomID, "", cloudprovider.InstanceNotFound},
		{"valid", fmt.Sprintf("phoenixnap://%s", server.Id), server.Type, nil},
	}

	for i, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			var plan string
			md, err := inst.InstanceMetadata(context.TODO(), testNode(tt.name, nodeName))
			if md != nil {
				plan = md.InstanceType
			}
			switch {
			case (err == nil && tt.err != nil) || (err != nil && tt.err == nil) || (err != nil && tt.err != nil && !strings.HasPrefix(err.Error(), tt.err.Error())):
				t.Errorf("%d: mismatched errors, actual %v expected %v", i, err, tt.err)
			case plan != tt.plan:
				t.Errorf("%d: mismatched id, actual %v expected %v", i, plan, tt.plan)
			}
		})
	}
}

func TestInstanceRegion(t *testing.T) {
	vc, backend := testGetValidCloud(t, "")
	inst, _ := vc.InstancesV2()
	devName := testGetNewServerName()
	location, _ := testGetOrCreateValidLocation(validLocationName, backend)
	product, _ := testGetOrCreateValidServerProduct(validProductName, location, backend)
	server, err := backend.CreateServer(devName, product.ProductCode, location)
	if err != nil {
		t.Fatalf("unable to create server: %v", err)
	}

	tests := []struct {
		testName string
		name     string
		region   string
		err      error
	}{
		{"empty name", "", "", cloudprovider.InstanceNotFound},
		{"invalid id", "thisdoesnotexist", "", cloudprovider.InstanceNotFound},
		{"unknown name", randomID, "", cloudprovider.InstanceNotFound},
		{"valid", fmt.Sprintf("phoenixnap://%s", server.Id), server.Location, nil},
	}

	for i, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			var region string
			md, err := inst.InstanceMetadata(context.TODO(), testNode(tt.name, nodeName))
			if md != nil {
				region = md.Region
			}
			switch {
			case (err == nil && tt.err != nil) || (err != nil && tt.err == nil) || (err != nil && tt.err != nil && !strings.HasPrefix(err.Error(), tt.err.Error())):
				t.Errorf("%d: mismatched errors, actual %v expected %v", i, err, tt.err)
			case region != tt.region:
				t.Errorf("%d: mismatched region, actual %v expected %v", i, region, tt.region)
			}
		})
	}
}

func TestInstanceExistsByProviderID(t *testing.T) {
	vc, backend := testGetValidCloud(t, "")
	inst, _ := vc.InstancesV2()
	serverName := testGetNewServerName()
	location, _ := testGetOrCreateValidLocation(validLocationName, backend)
	product, _ := testGetOrCreateValidServerProduct(validProductName, location, backend)
	server, err := backend.CreateServer(serverName, product.ProductCode, location)
	if err != nil {
		t.Fatalf("unable to create server: %v", err)
	}

	tests := []struct {
		id     string
		exists bool
		err    error
	}{
		{"", false, fmt.Errorf("providerID cannot be empty")}, // empty name
		{randomID, false, nil},                                // invalid format
		{fmt.Sprintf("aws://%s", randomID), false, fmt.Errorf("provider name from providerID should be phoenixnap")}, // not phoenixnap
		{fmt.Sprintf("phoenixnap://%s", randomID), false, nil},                                                       // unknown ID
		{fmt.Sprintf("phoenixnap://%s", server.Id), true, nil},                                                       // valid
		{server.Id, true, nil}, // valid
	}

	for i, tt := range tests {
		exists, err := inst.InstanceExists(context.TODO(), testNode(tt.id, nodeName))
		switch {
		case (err == nil && tt.err != nil) || (err != nil && tt.err == nil) || (err != nil && tt.err != nil && !strings.HasPrefix(err.Error(), tt.err.Error())):
			t.Errorf("%d: mismatched errors, actual %v expected %v", i, err, tt.err)
		case exists != tt.exists:
			t.Errorf("%d: mismatched exists, actual %v expected %v", i, exists, tt.exists)
		}
	}
}

func TestInstanceShutdownByProviderID(t *testing.T) {
	vc, backend := testGetValidCloud(t, "")
	inst, _ := vc.InstancesV2()
	serverNameActive := testGetNewServerName()
	serverNameInactive := testGetNewServerName()
	location, _ := testGetOrCreateValidLocation(validLocationName, backend)
	product, _ := testGetOrCreateValidServerProduct(validProductName, location, backend)

	serverActive, err := backend.CreateServer(serverNameActive, product.ProductCode, location)
	if err != nil {
		t.Fatalf("unable to create active server: %v", err)
	}
	serverInactive, err := backend.CreateServer(serverNameInactive, product.ProductCode, location)
	if err != nil {
		t.Fatalf("unable to create inactive server: %v", err)
	}
	serverInactive.Status = string(InstanceStatusPoweredOff)
	if err := backend.UpdateServer(serverInactive); err != nil {
		t.Fatalf("unable to update inactive server: %v", err)
	}

	tests := []struct {
		id   string
		down bool
		err  error
	}{
		{"", false, fmt.Errorf("providerID cannot be empty")},                                                        // empty name
		{randomID, false, cloudprovider.InstanceNotFound},                                                            // invalid format
		{fmt.Sprintf("aws://%s", randomID), false, fmt.Errorf("provider name from providerID should be phoenixnap")}, // not phoenixnap
		{fmt.Sprintf("phoenixnap://%s", randomID), false, cloudprovider.InstanceNotFound},                            // unknown ID
		{fmt.Sprintf("phoenixnap://%s", serverActive.Id), false, nil},                                                // valid
		{serverActive.Id, false, nil},                                                                                // valid
		{fmt.Sprintf("phoenixnap://%s", serverInactive.Id), true, nil},                                               // valid
		{serverInactive.Id, true, nil},                                                                               // valid
	}

	for i, tt := range tests {
		down, err := inst.InstanceShutdown(context.TODO(), testNode(tt.id, nodeName))
		switch {
		case (err == nil && tt.err != nil) || (err != nil && tt.err == nil) || (err != nil && tt.err != nil && !strings.HasPrefix(err.Error(), tt.err.Error())):
			t.Errorf("%d: mismatched errors, actual %v expected %v", i, err, tt.err)
		case down != tt.down:
			t.Errorf("%d: mismatched down, actual %v expected %v", i, down, tt.down)
		}
	}
}

func compareAddresses(a1, a2 []v1.NodeAddress) bool {
	switch {
	case (a1 == nil && a2 != nil) || (a1 != nil && a2 == nil):
		return false
	case a1 == nil && a2 == nil:
		return true
	case len(a1) != len(a2):
		return false
	default:
		// sort them
		sort.SliceStable(a1, func(i, j int) bool {
			return a1[i].Type < a1[j].Type
		})
		sort.SliceStable(a2, func(i, j int) bool {
			return a2[i].Type < a2[j].Type
		})
		for i := range a1 {
			if a1[i] != a2[i] {
				return false
			}
		}
		return true
	}

}
