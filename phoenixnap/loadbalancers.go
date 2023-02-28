package phoenixnap

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/phoenixnap/go-sdk-bmc/ipapi"
	"github.com/phoenixnap/go-sdk-bmc/tagapi"
	"github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/loadbalancers"
	pnapl2 "github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/loadbalancers/pnap-l2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type loadBalancers struct {
	ipClient             *ipapi.APIClient
	tagClient            *tagapi.APIClient
	k8sclient            kubernetes.Interface
	location             string
	clusterID            string
	implementor          loadbalancers.LB
	implementorConfig    string
	ipLocationAnnotation string
	nodeSelector         labels.Selector
}

func newLoadBalancers(ipClient *ipapi.APIClient, tagClient *tagapi.APIClient, k8sclient kubernetes.Interface, location, config string, ipLocationAnnotation, nodeSelector string) (*loadBalancers, error) {
	selector := labels.Everything()
	if nodeSelector != "" {
		selector, _ = labels.Parse(nodeSelector)
	}

	l := &loadBalancers{ipClient, tagClient, k8sclient, location, "", nil, config, ipLocationAnnotation, selector}

	// parse the implementor config and see what kind it is - allow for no config
	if l.implementorConfig == "" {
		klog.V(2).Info("loadBalancers.init(): no loadbalancer implementation config, skipping")
		return nil, nil
	}

	// if we did not specify a location, then we cannot proceed
	if location == "" {
		return nil, fmt.Errorf("no location specified, cannot proceed")
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
	lbconfig := u.Path
	var impl loadbalancers.LB
	switch u.Scheme {
	case "pnap-l2":
		klog.Info("loadbalancer implementation enabled: pnap-l2")
		impl = pnapl2.NewLB(k8sclient, lbconfig)
	default:
		klog.Info("loadbalancer implementation disabled")
		impl = nil
	}

	l.clusterID = string(systemNamespace.UID)
	l.implementor = impl
	klog.V(2).Info("loadBalancers.init(): complete")
	return l, nil
}

// implementation of cloudprovider.LoadBalancer

// GetLoadBalancer returns whether the specified load balancer exists, and
// if so, what its status is.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (l *loadBalancers) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	svcName := serviceRep(service)

	// if no service IP, then there is no load balancer for it
	if service.Spec.LoadBalancerIP == "" {
		return nil, false, nil
	}
	svcIP, err := netip.ParseAddr(service.Spec.LoadBalancerIP)
	if err != nil {
		return nil, false, fmt.Errorf("invalid service IP %s: %w", service.Spec.LoadBalancerIP, err)
	}

	blocks, err := l.getIPBlocks(service.Namespace, service.Name)
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
	network, err := netip.ParsePrefix(block.Cidr)
	if err != nil {
		klog.V(2).Infof("invalid CIDR %s: %s", block.Cidr, err)
		return nil, false, fmt.Errorf("invalid CIDR in block %s: %w", block.Cidr, err)
	}
	if !network.Contains(svcIP) {
		klog.V(2).Infof("block %s does not contain IP %s", block.Cidr, svcIP)
		return nil, false, fmt.Errorf("block %s does not contain IP %s", block.Cidr, svcIP)
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
func (l *loadBalancers) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
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
	blocks, err := l.getIPBlocks(service.Namespace, service.Name)
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
		clsTag, clsValue := clusterTag(l.clusterID)
		ipBlockCreate := ipapi.NewIpBlockCreate(l.location, fmt.Sprintf("/%d", serviceBlockCidr))
		// copy because we cannot take pointer to constant to use here
		pnapVal := pnapValue
		tags := []ipapi.TagAssignmentRequest{
			{Name: pnapTag, Value: &pnapVal},
			{Name: clsTag, Value: &clsValue},
			{Name: serviceNamespaceTag, Value: &service.Namespace},
			{Name: serviceNameTag, Value: &service.Name},
		}
		if err := ensureTags(l.tagClient, pnapTag, clsTag, serviceNamespaceTag, serviceNameTag); err != nil {
			return nil, fmt.Errorf("unable to ensure tags exist: %w", err)
		}
		ipBlockCreate.Tags = append(ipBlockCreate.Tags, tags...)

		block, _, err = l.ipClient.IPBlocksApi.IpBlocksPost(context.Background()).IpBlockCreate(*ipBlockCreate).Execute()
		if err != nil {
			return nil, fmt.Errorf("unable to create new IP block: %w", err)
		}
	}
	network, err := netip.ParsePrefix(block.Cidr)
	if err != nil {
		klog.V(2).Infof("invalid CIDR %s: %s", block.Cidr, err)
		return nil, fmt.Errorf("invalid CIDR in block %s: %w", block.Cidr, err)
	}
	foundIP = network.Addr().Next().String()
	// assign the second IP in the block to this service

	ipCidr, err := l.addService(ctx, service, foundIP, filterNodes(nodes, l.nodeSelector))
	if err != nil {
		return nil, fmt.Errorf("failed to add service %s: %w", service.Name, err)
	}
	// get the IP only
	ip := strings.SplitN(ipCidr, "/", 2)

	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{IP: ip[0]},
		},
	}, nil
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (l *loadBalancers) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	klog.V(2).Infof("UpdateLoadBalancer(): service %s", service.Name)
	// get IP address reservations and check if any exists for this svc

	var n []loadbalancers.Node
	for _, node := range filterNodes(nodes, l.nodeSelector) {
		klog.V(2).Infof("UpdateLoadBalancer(): %s", node.Name)
		// get the node provider ID
		id := node.Spec.ProviderID
		if id == "" {
			return fmt.Errorf("no provider ID given for node %s, skipping", node.Name)
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
func (l *loadBalancers) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	// REMOVAL
	klog.V(2).Infof("EnsureLoadBalancerDeleted(): remove: %s", service.Name)
	svcName := serviceRep(service)
	svcIP := service.Spec.LoadBalancerIP

	// tags for Get() are separated via '.', so '<key>.<value>'
	// get IP address blocks and check if any exist for this svc
	blocks, err := l.getIPBlocks(service.Namespace, service.Name)
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

	if _, _, err := l.ipClient.IPBlocksApi.IpBlocksIpBlockIdDelete(context.Background(), blocks[0].Id).Execute(); err != nil {
		return fmt.Errorf("unable to update tags reservations: %w", err)
	}

	klog.V(2).Infof("EnsureLoadBalancerDeleted(): remove: removed service %s from implementation", svcName)
	return nil
}

// utility funcs

// getIPBlocks returns cluster-related IP blocks
func (l *loadBalancers) getIPBlocks(namespace, name string) (blocks []ipapi.IpBlock, err error) {
	clsTag, clsValue := clusterTag(l.clusterID)

	// tags for Get() are separated via '.', so '<key>.<value>'
	tags := []string{fmt.Sprintf("%s.%s", serviceNamespaceTag, namespace), fmt.Sprintf("%s.%s", serviceNameTag, name), fmt.Sprintf("%s.%s", clsTag, clsValue), fmt.Sprintf("%s.%s", pnapTag, pnapValue)}
	// get IP address blocks and check if any has an IP that matches this service
	blocks, _, err = l.ipClient.IPBlocksApi.IpBlocksGet(context.Background()).Tag(tags).Execute()

	return
}

// addService add a single service; wraps the implementation
func (l *loadBalancers) addService(ctx context.Context, svc *v1.Service, ip string, nodes []*v1.Node) (string, error) {
	svcName := serviceRep(svc)
	svcIP := svc.Spec.LoadBalancerIP

	var (
		svcIPCidr string
	)

	klog.V(2).Infof("processing %s with existing IP assignment %s", svcName, svcIP)
	// if it already has an IP, no need to get it one
	if svcIP == "" {
		klog.V(2).Infof("no IP assigned for service %s; searching reservations", svcName)

		// we have an IP, either found from existing reservations or a new reservation.
		// map and assign it
		svcIP = ip

		// assign the IP and save it
		klog.V(2).Infof("assigning IP %s to %s", svcIP, svcName)
		intf := l.k8sclient.CoreV1().Services(svc.Namespace)
		existing, err := intf.Get(ctx, svc.Name, metav1.GetOptions{})
		if err != nil || existing == nil {
			klog.V(2).Infof("failed to get latest for service %s: %v", svcName, err)
			return "", fmt.Errorf("failed to get latest for service %s: %w", svcName, err)
		}
		existing.Spec.LoadBalancerIP = svcIP

		_, err = intf.Update(ctx, existing, metav1.UpdateOptions{})
		if err != nil {
			klog.V(2).Infof("failed to update service %s: %v", svcName, err)
			return "", fmt.Errorf("failed to update service %s: %w", svcName, err)
		}
		klog.V(2).Infof("successfully assigned %s update service %s", svcIP, svcName)
	}
	// now need to pass it the nodes

	var n []loadbalancers.Node
	for _, node := range nodes {
		n = append(n, loadbalancers.Node{
			Node: node,
		})
	}

	return svcIPCidr, l.implementor.AddService(ctx, svc.Namespace, svc.Name, svcIPCidr, n)
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
