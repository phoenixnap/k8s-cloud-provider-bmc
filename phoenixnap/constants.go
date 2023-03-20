package phoenixnap

const (
	pnapIdentifier              = "cloud-provider-phoenixnap-auto"
	pnapTag                     = "usage"
	pnapValue                   = pnapIdentifier
	deleteTag                   = "delete"
	activeValue                 = "true"
	serviceNamespaceTag         = "serviceNamespace"
	serviceNameTag              = "serviceName"
	ccmIPDescription            = "PhoenixNAP Kubernetes CCM auto-generated for Load Balancer"
	DefaultAnnotationIPLocation = "phoenixnap.com/ip-location"
	serviceBlockCidr            = 29
	gcIterationSeconds          = 30
	serverCategory              = "SERVER"
	publicNetworkCaps           = "PUBLIC_NETWORK"
	publicNetwork               = "public network"
)

var (
	instanceStatuses = []instanceStatus{
		InstanceStatusRebooting,
		InstanceStatusCreating,
		InstanceStatusResetting,
		InstanceStatusPoweredOn,
		InstanceStatusPoweredOff,
		InstanceStatusError,
		InstanceStatusDeleting,
	}
)

type instanceStatus string

const (
	InstanceStatusRebooting  instanceStatus = "rebooting"
	InstanceStatusCreating   instanceStatus = "creating"
	InstanceStatusResetting  instanceStatus = "resetting"
	InstanceStatusPoweredOn  instanceStatus = "powered-on"
	InstanceStatusPoweredOff instanceStatus = "powered-off"
	InstanceStatusError      instanceStatus = "error"
	InstanceStatusDeleting   instanceStatus = "deleting"
)
