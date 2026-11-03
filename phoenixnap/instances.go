package phoenixnap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/phoenixnap/go-sdk-bmc/bmcapi/v3"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
)

type instances struct {
	bmcClient *bmcapi.APIClient
}

var (
	_ cloudprovider.InstancesV2 = (*instances)(nil)
)

func newInstances(client *bmcapi.APIClient) *instances {
	return &instances{bmcClient: client}
}

// InstanceShutdown returns true if the node is shutdown in cloudprovider
func (i *instances) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	klog.V(2).Infof("called InstanceShutdown for node %s with providerID %s", node.GetName(), node.Spec.ProviderID)
	server, err := i.serverFromProviderID(ctx, node.Spec.ProviderID)
	if err != nil {
		return false, err
	}

	return server.Status == string(InstanceStatusPoweredOff), nil
}

// InstanceExists returns true if the node exists in cloudprovider
func (i *instances) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	klog.V(2).Infof("called InstanceExists for node %s with providerID %s", node.GetName(), node.Spec.ProviderID)
	_, err := i.serverFromProviderID(ctx, node.Spec.ProviderID)

	switch {
	case errors.Is(err, cloudprovider.InstanceNotFound):
		return false, nil
	case err != nil:
		return false, err
	}

	return true, nil
}

// InstanceMetadata returns instancemetadata for the node according to the cloudprovider
func (i *instances) InstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	server, err := i.serverByNode(ctx, node)
	if err != nil {
		return nil, err
	}
	nodeAddresses, err := nodeAddresses(*server)
	if err != nil {
		return nil, err
	}
	// "A zone represents a logical failure domain"
	// "A region represents a larger domain, made up of one or more zones"
	//
	// PhoenixNAP just have locations, which match K8s topology regions. We do not have zones for now.
	//
	// https://kubernetes.io/docs/reference/labels-annotations-taints/#topologykubernetesiozone

	return &cloudprovider.InstanceMetadata{
		ProviderID:    providerIDFromServer(server),
		InstanceType:  server.Type,
		NodeAddresses: nodeAddresses,
		Region:        server.Location,
	}, nil
}

func nodeAddresses(server bmcapi.Server) ([]v1.NodeAddress, error) {
	var addresses []v1.NodeAddress
	addresses = append(addresses, v1.NodeAddress{Type: v1.NodeHostName, Address: server.Hostname})

	// public IPs are optional: servers can be provisioned on private networks only
	for _, address := range server.PublicIpAddresses {
		if ip, ok := serverAddress(server.Id, address); ok {
			addresses = append(addresses, v1.NodeAddress{Type: v1.NodeExternalIP, Address: ip})
		}
	}
	var privateIP bool
	for _, address := range server.PrivateIpAddresses {
		if ip, ok := serverAddress(server.Id, address); ok {
			addresses = append(addresses, v1.NodeAddress{Type: v1.NodeInternalIP, Address: ip})
			privateIP = true
		}
	}

	if !privateIP {
		return nil, errors.New("could not get at least one private ip")
	}

	return addresses, nil
}

// serverAddress returns the IP for an address as the API reports it. The API reports
// IP blocks assigned to a server as a range, "first - last", for which the first address is used.
func serverAddress(serverID, address string) (string, bool) {
	first, last, isRange := strings.Cut(address, " - ")
	ip, err := netip.ParseAddr(strings.TrimSpace(first))
	if err == nil && isRange {
		_, err = netip.ParseAddr(strings.TrimSpace(last))
	}
	if err != nil {
		klog.V(2).Infof("server %s: skipping address %q that is neither an IP nor an IP range", serverID, address)
		return "", false
	}
	return ip.String(), true
}

func (i *instances) serverByNode(ctx context.Context, node *v1.Node) (*bmcapi.Server, error) {
	if node.Spec.ProviderID != "" {
		return i.serverFromProviderID(ctx, node.Spec.ProviderID)
	}

	return serverByName(ctx, i.bmcClient, types.NodeName(node.GetName()))
}

func serverByID(ctx context.Context, client *bmcapi.APIClient, id string) (*bmcapi.Server, error) {
	klog.V(2).Infof("called serverByID with ID %s", id)
	server, resp, err := client.ServersAPI.ServersServerIdGet(ctx, id).Execute()

	// resp is nil on transport errors, so check it before reading the status code
	if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden) {
		return nil, cloudprovider.InstanceNotFound
	}
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, cloudprovider.InstanceNotFound
	}
	return server, nil
}

// serverByName returns an instance whose hostname matches the kubernetes node.Name
func serverByName(ctx context.Context, client *bmcapi.APIClient, nodeName types.NodeName) (*bmcapi.Server, error) {
	klog.V(2).Infof("called serverByName nodeName %s", nodeName)
	if string(nodeName) == "" {
		return nil, errors.New("node name cannot be empty string")
	}
	servers, _, err := client.ServersAPI.ServersGet(ctx).Execute()

	if err != nil {
		klog.V(2).Infof("error listing servers: %v", err)
		return nil, err
	}

	for _, server := range servers {
		if server.Hostname == string(nodeName) {
			klog.V(2).Infof("Found server %s for nodeName %s", server.Id, nodeName)
			return &server, nil
		}
	}

	klog.V(2).Infof("No server found for nodeName %s", nodeName)
	return nil, cloudprovider.InstanceNotFound
}

// serverIDFromProviderID returns a server's ID from providerID.
//
// The providerID spec should be retrievable from the Kubernetes
// node object. The expected format is: phoenixnap://server-id or just server-id
func serverIDFromProviderID(providerID string) (string, error) {
	klog.V(2).Infof("called serverIDFromProviderID with providerID %s", providerID)
	if providerID == "" {
		return "", errors.New("providerID cannot be empty string")
	}

	split := strings.Split(providerID, "://")
	var serverID string
	switch len(split) {
	case 2:
		serverID = split[1]
		if split[0] != ProviderName {
			return "", fmt.Errorf("provider name from providerID should be %s, was %s", ProviderName, split[0])
		}
	case 1:
		serverID = providerID
	default:
		return "", fmt.Errorf("unexpected providerID format: %s, format should be: 'server-id' or 'phoenixnap://server-id'", providerID)
	}

	return serverID, nil
}

// serverFromProviderID uses providerID to get the server id and return the server
func (i *instances) serverFromProviderID(ctx context.Context, providerID string) (*bmcapi.Server, error) {
	klog.V(2).Infof("called serverFromProviderID with providerID %s", providerID)
	id, err := serverIDFromProviderID(providerID)
	if err != nil {
		return nil, err
	}

	return serverByID(ctx, i.bmcClient, id)
}

// providerIDFromServer returns a providerID from a server
func providerIDFromServer(server *bmcapi.Server) string {
	return fmt.Sprintf("%s://%s", ProviderName, server.Id)
}
