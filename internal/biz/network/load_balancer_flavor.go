package biz

// LoadBalancerFlavor describes the capacity of one dedicated EnvoyProxy.
// Installation and runtime observation use the same fixed capacity contract.
type LoadBalancerFlavor struct {
	Name                                             string
	Replicas                                         int64
	RequestCPU, RequestMemory, LimitCPU, LimitMemory string
}

func LoadBalancerFlavors() [3]LoadBalancerFlavor {
	return [3]LoadBalancerFlavor{
		{Name: "small", Replicas: 2, RequestCPU: "1", RequestMemory: "1Gi", LimitCPU: "2", LimitMemory: "2Gi"},
		{Name: "medium", Replicas: 4, RequestCPU: "2", RequestMemory: "2Gi", LimitCPU: "4", LimitMemory: "4Gi"},
		{Name: "large", Replicas: 6, RequestCPU: "4", RequestMemory: "4Gi", LimitCPU: "8", LimitMemory: "8Gi"},
	}
}

func FindLoadBalancerFlavor(name string) (LoadBalancerFlavor, bool) {
	if name == "" {
		name = "small"
	}
	for _, flavor := range LoadBalancerFlavors() {
		if flavor.Name == name {
			return flavor, true
		}
	}
	return LoadBalancerFlavor{}, false
}

const LoadBalancerGatewayClass = "lb-common"
