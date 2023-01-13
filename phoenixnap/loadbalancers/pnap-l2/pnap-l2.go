// empty loadbalancer that does nothing, but exists to enable bgp functionality
package empty

import (
	"context"

	"github.com/phoenixnap/cloud-provider-pnap/phoenixnap/loadbalancers"
	"k8s.io/client-go/kubernetes"
)

type LB struct {
}

func NewLB(k8sclient kubernetes.Interface, config string) *LB {
	return &LB{}
}

func (l *LB) AddService(ctx context.Context, svcNamespace, svcName, ip string, nodes []loadbalancers.Node) error {
	return nil
}

func (l *LB) RemoveService(ctx context.Context, svcNamespace, svcName, ip string) error {
	return nil
}

func (l *LB) UpdateService(ctx context.Context, svcNamespace, svcName string, nodes []loadbalancers.Node) error {
	return nil
}
