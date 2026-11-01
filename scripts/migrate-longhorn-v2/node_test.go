package main

import "testing"

func nodeFixture() object {
	return object{"metadata": object{"name": "prod-worker-1", "uid": "node-uid", "resourceVersion": "1", "labels": object{"node.longhorn.io/create-default-disk": "config"}, "annotations": object{"node.longhorn.io/default-disks-config": `[{"name":"longhorn-v2","path":"/dev/mapper/luks2-r-longhorn","diskType":"block","diskDriver":"aio","allowScheduling":true,"storageReserved":4294967296}]`}}, "spec": object{"providerID": "hcloud://42", "unschedulable": false}, "status": object{"nodeInfo": object{"systemUUID": "machine-uuid", "bootID": "boot-id"}, "capacity": object{"hugepages-2Mi": "2Gi"}, "allocatable": object{"hugepages-2Mi": "2Gi"}, "addresses": []any{object{"type": "InternalIP", "address": "10.0.99.4"}}, "conditions": healthyWorkerConditions()}}
}

func healthyWorkerConditions() []any {
	return []any{object{"type": "Ready", "status": "True"}, object{"type": "DiskPressure", "status": "False"}, object{"type": "MemoryPressure", "status": "False"}, object{"type": "PIDPressure", "status": "False"}}
}

func TestRefreshTargetMustRetainHardwareIdentityAndPrivateAddress(t *testing.T) {
	j := journal{Target: "prod-worker-1", NodeUID: "node-uid", ProviderID: "hcloud://42", MachineUUID: "machine-uuid"}
	address, err := verifyNode(nodeFixture(), j)
	if err != nil || address != "10.0.99.4" {
		t.Fatalf("known worker rejected: %v", err)
	}
	for name, mutate := range map[string]func(object){
		"replacement server": func(n object) { n["spec"].(object)["providerID"] = "hcloud://43" },
		"different machine":  func(n object) { n["status"].(object)["nodeInfo"].(object)["systemUUID"] = "other" },
		"replacement node":   func(n object) { n["metadata"].(object)["uid"] = "replacement" },
		"public address": func(n object) {
			n["status"].(object)["addresses"] = []any{object{"type": "InternalIP", "address": "203.0.113.1"}}
		},
		"ambiguous addresses": func(n object) {
			n["status"].(object)["addresses"] = []any{object{"type": "InternalIP", "address": "10.0.99.4"}, object{"type": "InternalIP", "address": "10.0.99.5"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			n := nodeFixture()
			mutate(n)
			if _, err := verifyNode(n, j); err == nil {
				t.Fatal("changed reset target accepted")
			}
		})
	}
}
