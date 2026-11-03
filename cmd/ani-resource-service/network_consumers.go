package main

import networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"

// networkConsumerDirectory composes whichever consumer domains this process
// actually opened. A missing module stays nil so its rows are reported as
// unknown instead of dereferencing a database that was never configured.
func networkConsumerDirectory(instances *instanceComponents, vms *vmComponents) networkbiz.ConsumerDirectory {
	var container networkbiz.ConsumerReader
	if instances != nil && instances.repo != nil {
		container = instances.repo
	}
	var machine networkbiz.ConsumerReader
	if vms != nil && vms.repo != nil {
		machine = vms.repo
	}
	return networkbiz.NewConsumerDirectory(container, machine)
}
