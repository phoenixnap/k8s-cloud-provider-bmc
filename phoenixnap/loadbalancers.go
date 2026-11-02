package phoenixnap

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"time"

	"github.com/phoenixnap/go-sdk-bmc/ipapi/v3"
	netapi "github.com/phoenixnap/go-sdk-bmc/networkapi/v4"
	"github.com/phoenixnap/go-sdk-bmc/tagapi/v3"
	"github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/loadbalancers"
	kubevip "github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/loadbalancers/kubevip"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type loadBalancers struct {
	ipClient             *ipapi.APIClient
	tagClient            *tagapi.APIClient
	netClient            *netapi.APIClient
	k8sclient            kubernetes.Interface
	location             string
	clusterID            string
	implementor          loadbalancers.LB
	implementorConfig    string
	ipLocationAnnotation string
	network              string
	nodeSelector         labels.Selector
}

func newLoadBalancers(ipClient *ipapi.APIClient, tagClient *tagapi.APIClient, netclient *netapi.APIClient, k8sclient kubernetes.Interface, location, config string, ipLocationAnnotation, nodeSelector string, stop <-chan struct{}) (*loadBalancers, error) {
	selector := labels.Everything()
	if nodeSelector != "" {
		var err error
		if selector, err = labels.Parse(nodeSelector); err != nil {
			return nil, fmt.Errorf("invalid service node selector %q: %w", nodeSelector, err)
		}
	}

	l := &loadBalancers{ipClient, tagClient, netclient, k8sclient, location, "", nil, config, ipLocationAnnotation, "", selector}

	// parse the implementor config and see what kind it is - allow for no config
	if l.implementorConfig == "" {
		klog.V(2).Info("loadBalancers.init(): no loadbalancer implementation config, skipping")
		return nil, nil
	}

	// a global location is optional; without one, each Service must carry the location annotation
	if location == "" {
		klog.Infof("loadBalancers.init(): no global location set, Services must set the %s annotation", ipLocationAnnotation)
	}

	// get the UID of the kube-system namespace
	systemNamespace, err := k8sclient.CoreV1().Namespaces().Get(context.Background(), "kube-system", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kube-system namespace: %w", err)
	}
	if systemNamespace == nil {
		return nil, fmt.Errorf("kube-system namespace is missing unexplainably")
	}

	u, err := url.Parse(l.implementorConfig)
	if err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid config: no public network provided")
	}
	lbconfig := u.Path
	var impl loadbalancers.LB
	switch u.Scheme {
	case "kube-vip":
		klog.Infof("loadbalancer implementation enabled: kube-vip on public network %s", lbconfig)
		impl = kubevip.NewLB(k8sclient, lbconfig)
	default:
		return nil, fmt.Errorf("unsupported loadbalancer type %q, supported types are: kube-vip", u.Scheme)
	}

	l.clusterID = string(systemNamespace.UID)
	l.implementor = impl
	l.network = u.Host

	// start the reaper for blocks indicated for deletion
	go l.reapBlocks(stop)

	klog.V(2).Info("loadBalancers.init(): complete")
	return l, nil
}

// reapBlocks periodically unassigns and deletes the IP blocks marked for deletion, until stop is closed
func (l *loadBalancers) reapBlocks(stop <-chan struct{}) {
	ticker := time.NewTicker(gcIterationSeconds * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			l.reapBlocksOnce(context.Background())
		}
	}
}

func (l *loadBalancers) reapBlocksOnce(ctx context.Context) {
	// get deleted only
	blocks, err := l.getIPBlocks(ctx, "", "", false, true)
	if err != nil {
		klog.Errorf("unable to retrieve IP blocks: %v", err)
		return
	}
	if len(blocks) == 0 {
		klog.V(5).Info("no IP blocks marked for deletion")
		return
	}
	for _, block := range blocks {
		switch block.GetStatus() {
		case "unassigned":
			klog.Infof("deleting unassigned block %s", block.GetId())
			// it is unassigned, delete the block
			if _, _, err := l.ipClient.IPBlocksAPI.IpBlocksIpBlockIdDelete(ctx, block.GetId()).Execute(); err != nil {
				klog.Errorf("unable to delete IP block %s: %v", block.GetId(), err)
			}
		case "unassigning":
			klog.Infof("block %s still unassigning, waiting", block.GetId())
		default:
			// unassign it from whichever network it is on
			network := l.network
			if block.AssignedResourceId != nil && *block.AssignedResourceId != "" {
				network = *block.AssignedResourceId
			}
			klog.Infof("unassigning block %s from network %s", block.GetId(), network)
			if _, _, err := l.netClient.PublicNetworksAPI.PublicNetworksNetworkIdIpBlocksIpBlockIdDelete(ctx, network, block.GetId()).Execute(); err != nil {
				klog.Errorf("unable to unassign IP block %s from network %s: %v", block.GetId(), network, err)
			}
		}
	}
}

// implementation of cloudprovider.LoadBalancer

// GetLoadBalancer returns whether the specified load balancer exists, and
// if so, what its status is.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (l *loadBalancers) GetLoadBalancer(ctx context.Context, _ string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	svcName := serviceRep(service)

	// if no service IP, then there is no load balancer for it
	if service.Spec.LoadBalancerIP == "" {
		return nil, false, nil
	}
	svcIP, err := netip.ParseAddr(service.Spec.LoadBalancerIP)
	if err != nil {
		return nil, false, fmt.Errorf("invalid service IP %s: %w", service.Spec.LoadBalancerIP, err)
	}

	// get active only
	blocks, err := l.getIPBlocks(ctx, service.Namespace, service.Name, true, false)
	if err != nil {
		return nil, false, err
	}

	if len(blocks) == 0 {
		klog.V(2).Infof("no blocks with reservation found")
		return nil, false, nil
	}
	if len(blocks) > 1 {
		klog.V(2).Infof("multiple blocks with reservation found")
		return nil, false, fmt.Errorf("more than one block found for service %s", svcName)
	}

	// one block, it has our IP
	block := blocks[0]
	network, err := netip.ParsePrefix(block.GetCidr())
	if err != nil {
		klog.V(2).Infof("invalid CIDR %s: %s", block.GetCidr(), err)
		return nil, false, fmt.Errorf("invalid CIDR in block %s: %w", block.GetCidr(), err)
	}
	if !network.Contains(svcIP) {
		klog.V(2).Infof("block %s does not contain IP %s", block.GetCidr(), svcIP)
		return nil, false, fmt.Errorf("block %s does not contain IP %s", block.GetCidr(), svcIP)
	}

	// see that it is connected to the correct network
	if block.AssignedResourceType == nil {
		klog.V(2).Infof("block %s has no assigned resource type", block.GetCidr())
		return nil, false, fmt.Errorf("block %s has no assigned resource type", block.GetCidr())
	}
	if *block.AssignedResourceType != publicNetwork && *block.AssignedResourceType != publicNetworkCaps {
		klog.V(2).Infof("block %s is not assigned to a public network", block.GetCidr())
		return nil, false, fmt.Errorf("block %s is not assigned to a public network", block.GetCidr())
	}
	if block.AssignedResourceId == nil {
		klog.V(2).Infof("block %s has no assigned resource ID", block.GetCidr())
		return nil, false, fmt.Errorf("block %s has no assigned resource ID", block.GetCidr())
	}
	if *block.AssignedResourceId != l.network {
		klog.V(2).Infof("block %s is assigned to network %s instead of expected %s", block.GetCidr(), *block.AssignedResourceId, l.network)
		return nil, false, fmt.Errorf("block %s is assigned to network %s instead of expected %s", block.GetCidr(), *block.AssignedResourceId, l.network)
	}

	klog.V(2).Infof("GetLoadBalancer(): %s with existing IP assignment %s", svcName, svcIP)
	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{IP: svcIP.String()},
		},
	}, true, nil
}

// GetLoadBalancerName returns the name of the load balancer. Implementations must treat the
// *v1.Service parameter as read-only and not modify it.
func (l *loadBalancers) GetLoadBalancerName(_ context.Context, _ string, service *v1.Service) string {
	clsTag, clsValue := clusterTag(l.clusterID)
	return fmt.Sprintf("%s=%s:%s=%s:%s=%s", pnapTag, pnapValue, "service", serviceRep(service), clsTag, clsValue)
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (l *loadBalancers) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	klog.V(2).Infof("EnsureLoadBalancer(): add: service %s/%s", service.Namespace, service.Name)
	// first check if one already exists for this service
	status, exists, err := l.GetLoadBalancer(ctx, clusterName, service)
	if err != nil {
		return nil, err
	}
	if exists {
		return status, nil
	}

	// no error, but no existing load balancer, so create one
	svcName := serviceRep(service)
	// get active only
	blocks, err := l.getIPBlocks(ctx, service.Namespace, service.Name, true, false)
	if err != nil {
		return nil, err
	}

	var (
		foundIP string
	)
	if len(blocks) > 1 {
		klog.V(2).Infof("multiple blocks with reservation found")
		return nil, fmt.Errorf("more than one block found for service %s", svcName)
	}
	var block *ipapi.IpBlock
	if len(blocks) == 1 {
		// we have a block, but it doesn't have an IP assigned
		block = &blocks[0]
	} else {
		location, err := l.serviceLocation(service)
		if err != nil {
			return nil, err
		}
		clsTag, clsValue := clusterTag(l.clusterID)
		ipBlockCreate := ipapi.NewIpBlockCreate(location, fmt.Sprintf("/%d", serviceBlockCidr))
		// copy because we cannot take pointer to constant to use here
		pnapVal := pnapValue
		tags := []ipapi.TagAssignmentRequest{
			{Name: pnapTag, Value: &pnapVal},
			{Name: clsTag, Value: &clsValue},
			{Name: serviceNamespaceTag, Value: &service.Namespace},
			{Name: serviceNameTag, Value: &service.Name},
		}
		if err := ensureTags(ctx, l.tagClient, pnapTag, clsTag, serviceNamespaceTag, serviceNameTag, deleteTag); err != nil {
			return nil, fmt.Errorf("unable to ensure tags exist: %w", err)
		}
		ipBlockCreate.Tags = append(ipBlockCreate.Tags, tags...)

		block, _, err = l.ipClient.IPBlocksAPI.IpBlocksPost(ctx).IpBlockCreate(*ipBlockCreate).Execute()
		if err != nil {
			return nil, fmt.Errorf("unable to create new IP block: %w", err)
		}
	}
	if block.AssignedResourceType != nil {
		if *block.AssignedResourceType != publicNetwork && *block.AssignedResourceType != publicNetworkCaps {
			return nil, fmt.Errorf("block %s is assigned to %s and not to a public network", block.GetCidr(), *block.AssignedResourceType)
		}
		if block.AssignedResourceId == nil {
			return nil, fmt.Errorf("block %s has an assigned resource type %s but not ID", block.GetCidr(), *block.AssignedResourceType)
		}
		if *block.AssignedResourceId != l.network {
			return nil, fmt.Errorf("block %s is assigned to network %s instead of expected %s", block.GetCidr(), *block.AssignedResourceId, l.network)
		}
		// at this point, it is assigned and to our network
	} else {
		// it all was nil, so assign it
		if _, _, err := l.netClient.PublicNetworksAPI.PublicNetworksNetworkIdIpBlocksPost(ctx, l.network).PublicNetworkIpBlockCreate(*netapi.NewPublicNetworkIpBlockCreate(block.GetId())).Execute(); err != nil {
			return nil, fmt.Errorf("unable to assign block %s to network %s: %w", block.GetCidr(), l.network, err)
		}
	}

	prefix, err := netip.ParsePrefix(block.GetCidr())
	if err != nil {
		klog.V(2).Infof("invalid CIDR %s: %s", block.GetCidr(), err)
		return nil, fmt.Errorf("invalid CIDR in block %s: %w", block.GetCidr(), err)
	}
	network := prefix.Addr()
	// get the first free address, after network and router
	foundIP = network.Next().Next().String()

	// assign the third IP in the block to this service
	ip, err := l.addService(ctx, service, foundIP, filterNodes(nodes, l.nodeSelector))
	if err != nil {
		return nil, fmt.Errorf("failed to add service %s: %w", service.Name, err)
	}

	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{IP: ip},
		},
	}, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (l *loadBalancers) UpdateLoadBalancer(ctx context.Context, _ string, service *v1.Service, nodes []*v1.Node) error {
	klog.V(2).Infof("UpdateLoadBalancer(): service %s", service.Name)
	// get IP address reservations and check if any exists for this svc

	var n []loadbalancers.Node
	for _, node := range filterNodes(nodes, l.nodeSelector) {
		klog.V(2).Infof("UpdateLoadBalancer(): %s", node.Name)
		// get the node provider ID
		id := node.Spec.ProviderID
		if id == "" {
			klog.V(2).Infof("UpdateLoadBalancer(): no provider ID given for node %s, skipping", node.Name)
			continue
		}
		n = append(n, loadbalancers.Node{
			Node: node,
		})
	}
	return l.implementor.UpdateService(ctx, service.Namespace, service.Name, n)
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it
// exists, returning nil if the load balancer specified either didn't exist or
// was successfully deleted.
// This construction is useful because many cloud providers' load balancers
// have multiple underlying components, meaning a Get could say that the LB
// doesn't exist even if some part of it is still laying around.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (l *loadBalancers) EnsureLoadBalancerDeleted(ctx context.Context, _ string, service *v1.Service) error {
	// REMOVAL
	klog.V(2).Infof("EnsureLoadBalancerDeleted(): remove: %s", service.Name)
	svcName := serviceRep(service)
	svcIP := service.Spec.LoadBalancerIP

	// first remove the IP from the loadbalancer, so it gets released
	klog.V(2).Infof("removing IP %s from %s", svcIP, svcName)
	intf := l.k8sclient.CoreV1().Services(service.Namespace)
	existing, err := intf.Get(ctx, service.Name, metav1.GetOptions{})
	switch {
	case err != nil || existing == nil:
		klog.V(2).Infof("failed to get latest for service, moving on to delete IP assignment %s: %v", svcName, err)
	case existing.Spec.LoadBalancerIP == "" && existing.Annotations[kubeVipLoadBalancerIPsAnnotation] == "":
		klog.V(2).Infof("service %s has no IP assigned, nothing to remove", svcName)
	default:
		existing.Spec.LoadBalancerIP = ""
		delete(existing.Annotations, kubeVipLoadBalancerIPsAnnotation)
		_, err = intf.Update(ctx, existing, metav1.UpdateOptions{})
		if err != nil {
			klog.V(2).Infof("failed to update service to remove IP %s: %v", svcName, err)
			return fmt.Errorf("failed to update service %s: %w", svcName, err)
		}
		klog.V(2).Infof("successfully removed %s from service %s", svcIP, svcName)
	}

	// tags for Get() are separated via '.', so '<key>.<value>'
	// get IP address blocks and check if any exist for this svc
	// active blocks only
	blocks, err := l.getIPBlocks(ctx, service.Namespace, service.Name, true, false)
	if err != nil {
		return fmt.Errorf("unable to retrieve IP reservations: %w", err)
	}

	klog.V(2).Infof("EnsureLoadBalancerDeleted(): remove: %s with existing IP assignment %s", svcName, svcIP)
	if len(blocks) == 0 {
		klog.V(2).Infof("EnsureLoadBalancerDeleted(): remove: no IP reservation found for %s, nothing to delete", svcName)
		return nil
	}
	if len(blocks) > 1 {
		return fmt.Errorf("multiple IP blocks found for %s, cannot delete", svcName)
	}
	// add the delete tag to the block; this will cause the other loop to unassign it and delete it
	tags := blocks[0].Tags
	var tagRequest []ipapi.TagAssignmentRequest
	for _, tag := range tags {
		if tag.Name == serviceNameTag || tag.Name == serviceNamespaceTag || tag.Name == deleteTag {
			continue
		}
		tagRequest = append(tagRequest, ipapi.TagAssignmentRequest{
			Name:  tag.Name,
			Value: tag.Value,
		})
	}
	valtrue := activeValue
	tagRequest = append(tagRequest, ipapi.TagAssignmentRequest{Name: deleteTag, Value: &valtrue})

	if _, _, err := l.ipClient.IPBlocksAPI.IpBlocksIpBlockIdTagsPut(ctx, blocks[0].GetId()).TagAssignmentRequest(tagRequest).Execute(); err != nil {
		return fmt.Errorf("unable to add 'delete' tag from IP block %s: %w", blocks[0].GetId(), err)
	}

	if err := l.implementor.RemoveService(ctx, service.Namespace, service.Name, svcIP); err != nil {
		return fmt.Errorf("unable to remove service %s from loadbalancer implementation: %w", svcName, err)
	}
	klog.V(2).Infof("EnsureLoadBalancerDeleted(): remove: removed service %s from implementation", svcName)
	return nil
}

// utility funcs

// getIPBlocks returns cluster-related IP blocks. If namespace or name is not blank, filters search
// by IP blocks with those tags. If activeOnly is true, will not return blocks with the delete tag set.
func (l *loadBalancers) getIPBlocks(ctx context.Context, namespace, name string, active, deleted bool) (blocks []ipapi.IpBlock, err error) {
	clsTag, clsValue := clusterTag(l.clusterID)

	// tags for Get() are separated via '.', so '<key>.<value>'
	tags := []string{fmt.Sprintf("%s.%s", clsTag, clsValue), fmt.Sprintf("%s.%s", pnapTag, pnapValue)}
	if name != "" {
		tags = append(tags, fmt.Sprintf("%s.%s", serviceNameTag, name))
	}
	if namespace != "" {
		tags = append(tags, fmt.Sprintf("%s.%s", serviceNamespaceTag, namespace))
	}
	// get IP address blocks and check if any has an IP that matches this service
	blocks, _, err = l.ipClient.IPBlocksAPI.IpBlocksGet(ctx).Tag(tags).Execute()
	if err != nil {
		return
	}

	// if we take all blocks, just return them
	if active && deleted {
		return
	}
	var finalBlocks []ipapi.IpBlock

	// arrange active and passive
	for _, b := range blocks {
		var isDeleted bool
		for _, tags := range b.Tags {
			if tags.Name == deleteTag {
				isDeleted = true
				break
			}
		}
		// only keep the block if we asked for deleted and it is deleted,
		// or if we asked for active and it is not deleted
		if (isDeleted && deleted) || (!isDeleted && active) {
			finalBlocks = append(finalBlocks, b)
		}
	}
	blocks = finalBlocks
	return
}

// addService add a single service; wraps the implementation. Returns the IP assigned to the service.
func (l *loadBalancers) addService(ctx context.Context, svc *v1.Service, ip string, nodes []*v1.Node) (string, error) {
	svcName := serviceRep(svc)

	klog.V(2).Infof("processing %s with existing IP assignment %q", svcName, svc.Spec.LoadBalancerIP)
	intf := l.k8sclient.CoreV1().Services(svc.Namespace)
	existing, err := intf.Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil || existing == nil {
		klog.V(2).Infof("failed to get latest for service %s: %v", svcName, err)
		return "", fmt.Errorf("failed to get latest for service %s: %w", svcName, err)
	}
	// the IP must always be the one from the service's block, so overwrite anything else
	if existing.Spec.LoadBalancerIP != ip || existing.Annotations[kubeVipLoadBalancerIPsAnnotation] != ip {
		klog.V(2).Infof("assigning IP %s to %s", ip, svcName)
		existing.Spec.LoadBalancerIP = ip
		// kube-vip reads this annotation, as spec.loadBalancerIP is deprecated since Kubernetes 1.24
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[kubeVipLoadBalancerIPsAnnotation] = ip

		if _, err := intf.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			klog.V(2).Infof("failed to update service %s: %v", svcName, err)
			return "", fmt.Errorf("failed to update service %s: %w", svcName, err)
		}
		klog.V(2).Infof("successfully assigned %s update service %s", ip, svcName)
	}

	// now need to pass it the nodes
	var n []loadbalancers.Node
	for _, node := range nodes {
		n = append(n, loadbalancers.Node{
			Node: node,
		})
	}

	return ip, l.implementor.AddService(ctx, svc.Namespace, svc.Name, ip, n)
}

// serviceLocation returns the location in which to create the IP block for a service:
// the global location if set, else the service's location annotation
func (l *loadBalancers) serviceLocation(svc *v1.Service) (string, error) {
	if l.location != "" {
		return l.location, nil
	}
	if location := svc.Annotations[l.ipLocationAnnotation]; location != "" {
		return location, nil
	}
	return "", fmt.Errorf("no location for service %s: set a global location or the %s annotation", serviceRep(svc), l.ipLocationAnnotation)
}

func serviceRep(svc *v1.Service) string {
	if svc == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)
}

func clusterTag(clusterID string) (string, string) {
	return "cluster", clusterID
}

func filterNodes(nodes []*v1.Node, nodeSelector labels.Selector) []*v1.Node {
	filteredNodes := []*v1.Node{}

	for _, node := range nodes {
		if nodeSelector.Matches(labels.Set(node.Labels)) {
			filteredNodes = append(filteredNodes, node)
		}
	}
	return filteredNodes
}
