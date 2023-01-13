package phoenixnap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/apparentlymart/go-cidr/cidr"
	"github.com/phoenixnap/cloud-provider-pnap/phoenixnap/loadbalancers"
	pnapl2 "github.com/phoenixnap/cloud-provider-pnap/phoenixnap/loadbalancers/pnap-l2"
	"github.com/phoenixnap/go-sdk-bmc/ipapi"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	serviceTagPrefix = "service-ip-"
)

type loadBalancers struct {
	client               *ipapi.APIClient
	k8sclient            kubernetes.Interface
	location             string
	clusterID            string
	implementor          loadbalancers.LB
	implementorConfig    string
	ipLocationAnnotation string
	nodeSelector         labels.Selector
}

func newLoadBalancers(client *ipapi.APIClient, k8sclient kubernetes.Interface, location, config string, ipLocationAnnotation, nodeSelector string) (*loadBalancers, error) {
	selector := labels.Everything()
	if nodeSelector != "" {
		selector, _ = labels.Parse(nodeSelector)
	}

	l := &loadBalancers{client, k8sclient, location, "", nil, config, ipLocationAnnotation, selector}

	// parse the implementor config and see what kind it is - allow for no config
	if l.implementorConfig == "" {
		klog.V(2).Info("loadBalancers.init(): no loadbalancer implementation config, skipping")
		return nil, nil
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
	svcTag := serviceTag(service)
	clsTag, clsValue := clusterTag(l.clusterID)
	svcIP := service.Spec.LoadBalancerIP

	// tags for Get() are separated via '.', so '<key>.<value>'
	tags := []string{svcTag, fmt.Sprintf("%s.%s", clsTag, clsValue), fmt.Sprintf("%s.%s", pnapTag, pnapValue)}
	// get IP address blocks and check if any exist for this svc
	blocks, _, err := l.client.IPBlocksApi.IpBlocksGet(context.Background()).Tag(tags).Execute()

	if err != nil {
		return nil, false, fmt.Errorf("unable to retrieve IP reservations: %w", err)
	}
	klog.V(2).Infof("got ip blocks %d", len(blocks))

	if len(blocks) == 0 {
		klog.V(2).Infof("no blocks with reservation found")
		return nil, false, nil
	}

	if len(blocks) > 1 {
		klog.V(2).Infof("too many blocks found for same service %d for %s", len(blocks), svcName)
		return nil, false, fmt.Errorf("too many blocks found for same service %s", svcName)
	}
	var targetIP string
	for _, tag := range blocks[0].Tags {
		if tag.Name != svcTag {
			continue
		}
		if tag.Value == nil {
			return nil, false, fmt.Errorf("tag %s has no value", svcTag)
		}
		targetIP = *tag.Value
		break
	}

	klog.V(2).Infof("GetLoadBalancer(): %s with existing IP assignment %s", svcName, svcIP)

	// get the IPs and see if there is anything to clean up
	if targetIP == "" {
		klog.V(2).Infof("no reservation found")
		return nil, false, nil
	}
	klog.V(2).Infof("reservation found: %s", targetIP)
	return &v1.LoadBalancerStatus{
		Ingress: []v1.LoadBalancerIngress{
			{IP: targetIP},
		},
	}, true, nil
}

// GetLoadBalancerName returns the name of the load balancer. Implementations must treat the
// *v1.Service parameter as read-only and not modify it.
func (l *loadBalancers) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	svcTag := serviceTag(service)
	clsTag, clsValue := clusterTag(l.clusterID)
	return fmt.Sprintf("%s=%s:%s=%s:%s=%s", pnapTag, pnapValue, svcTag, service.Spec.LoadBalancerIP, clsTag, clsValue)
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

	// get IP blocks, see if any have spare IPs
	// tags for Get() are separated via '.', so '<key>.<value>'
	clsTag, clsValue := clusterTag(l.clusterID)
	tags := []string{fmt.Sprintf("%s.%s", clsTag, clsValue), fmt.Sprintf("%s.%s", pnapTag, pnapValue)}
	// get IP address blocks and check if any exist for this svc
	blocks, _, err := l.client.IPBlocksApi.IpBlocksGet(context.Background()).Tag(tags).Execute()
	if err != nil {
		return nil, err
	}
	var (
		foundBlock *ipapi.IpBlock
		foundIP    string
	)
	for _, block := range blocks {
		sizeAsInt, err := strconv.ParseInt(block.CidrBlockSize[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid cidr size: %s %v", block.CidrBlockSize, err)
		}
		size := 32 - sizeAsInt
		used := map[string]bool{}
		for _, tag := range block.Tags {
			if strings.HasPrefix(tag.Name, serviceTagPrefix) {
				addr := strings.TrimPrefix(tag.Name, serviceTagPrefix)
				used[addr] = true
			}
		}
		if len(used) >= int(size) {
			continue
		}
		foundBlock = &block
		// figure out which IPs were not used and pick one
		ip := strings.SplitN(block.Cidr, "/", 2)
		if len(ip) != 2 {
			return nil, fmt.Errorf("invalid cidr: %s", block.Cidr)
		}
		start, finish := cidr.AddressRange(&net.IPNet{
			IP:   net.ParseIP(ip[0]),
			Mask: net.CIDRMask(int(sizeAsInt), 32),
		})
		// ignore the first address
		for i := cidr.Inc(start); !i.Equal(finish); i = cidr.Inc(i) {
			addr := i.String()
			if used[addr] {
				continue
			}
			foundIP = addr
			break
		}
		break
	}

	// if no block found with space, allocate a new block
	if foundBlock == nil {
		ipBlockCreate := ipapi.NewIpBlockCreate(l.location, fmt.Sprintf("/%d", serviceBlockCidr))
		// copy because we cannot take pointer to constant to use here
		val := pnapValue
		tags := []ipapi.TagAssignmentRequest{
			{Name: pnapTag, Value: &val},
			{Name: clsTag, Value: &clsValue},
		}
		ipBlockCreate.Tags = append(ipBlockCreate.Tags, tags...)

		block, _, err := l.client.IPBlocksApi.IpBlocksPost(context.Background()).IpBlockCreate(*ipBlockCreate).Execute()
		if err != nil {
			return nil, fmt.Errorf("unable to create new IP block: %w", err)
		}
		foundBlock = block
	}
	newTag := serviceTag(service)

	// TODO: need to assign all tags from foundBlock.Tags plus new one, not just new ones.
	requests := tagAssignmentsIntoRequests(foundBlock.Tags)
	requests = append(requests, ipapi.TagAssignmentRequest{
		Name:  newTag,
		Value: &foundIP,
	})
	// concatenate tag
	if _, _, err := l.client.IPBlocksApi.IpBlocksIpBlockIdTagsPut(context.Background(), foundBlock.Id).TagAssignmentRequest(requests).Execute(); err != nil {
		return nil, fmt.Errorf("unable to update tags reservations: %w", err)
	}
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
	svcTag := serviceTag(service)
	clsTag, clsValue := clusterTag(l.clusterID)
	svcIP := service.Spec.LoadBalancerIP

	// tags for Get() are separated via '.', so '<key>.<value>'
	tags := []string{fmt.Sprintf("%s.%s", svcTag, svcIP), fmt.Sprintf("%s.%s", clsTag, clsValue), fmt.Sprintf("%s.%s", pnapTag, pnapValue)}
	// get IP address blocks and check if any exist for this svc
	blocks, _, err := l.client.IPBlocksApi.IpBlocksGet(context.Background()).Tag(tags).Execute()
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
	// TODO: update tags to remove our tag
	var targetTags []ipapi.TagAssignment
	for _, tag := range blocks[0].Tags {
		if tag.Name == svcTag {
			continue
		}
		targetTags = append(targetTags, tag)
	}

	// REMOVE
	//removedTag := RemoveTagFromIpBlock(*addedTag, 0)

	requests := tagAssignmentsIntoRequests(targetTags)
	if _, _, err := l.client.IPBlocksApi.IpBlocksIpBlockIdTagsPut(context.Background(), blocks[0].Id).TagAssignmentRequest(requests).Execute(); err != nil {
		return fmt.Errorf("unable to update tags reservations: %w", err)
	}

	klog.V(2).Infof("EnsureLoadBalancerDeleted(): remove: removed service %s from implementation", svcName)
	return nil
}

// utility funcs

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

func serviceAnnotation(svc *v1.Service, annotation string) string {
	if svc == nil {
		return ""
	}
	if svc.ObjectMeta.Annotations == nil {
		return ""
	}
	return svc.ObjectMeta.Annotations[annotation]
}

func serviceTag(svc *v1.Service) string {
	if svc == nil {
		return ""
	}
	hash := sha256.Sum256([]byte(serviceRep(svc)))
	return fmt.Sprintf("%s%s", serviceTagPrefix, base64.StdEncoding.EncodeToString(hash[:]))
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
