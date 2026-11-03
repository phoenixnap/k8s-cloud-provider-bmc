// empty loadbalancer that does nothing, but exists to enable bgp functionality
package kubevip

import (
	"context"

	"github.com/phoenixnap/k8s-cloud-provider-bmc/phoenixnap/loadbalancers"
	"k8s.io/client-go/kubernetes"
)

type LB struct {
}

func NewLB(_ kubernetes.Interface, _ string) *LB {
	return &LB{}
}

func (l *LB) AddService(_ context.Context, _, _, _ string, _ []loadbalancers.Node) error {
	return nil
}

func (l *LB) RemoveService(_ context.Context, _, _, _ string) error {
	return nil
}

func (l *LB) UpdateService(_ context.Context, _, _ string, _ []loadbalancers.Node) error {
	return nil
}
