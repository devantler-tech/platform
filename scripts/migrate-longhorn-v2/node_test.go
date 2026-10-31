package main

import "testing"

func nodeFixture() object {
	return object{"metadata": object{"name": "prod-worker-1", "uid": "node-uid", "resourceVersion": "1", "labels": object{"node.longhorn.io/create-default-disk": "config"}, "annotations": object{"node.longhorn.io/default-disks-config": `[{"name":"longhorn-v2","path":"/dev/mapper/luks2-r-longhorn","diskType":"block","diskDriver":"aio","allowScheduling":true,"storageReserved":4294967296}]`}}, "spec": object{"providerID": "hcloud://42", "unschedulable": false}, "status": object{"nodeInfo": object{"systemUUID": "machine-uuid", "bootID": "boot-id"}, "capacity": object{"hugepages-2Mi": "2Gi"}, "allocatable": object{"hugepages-2Mi": "2Gi"}, "addresses": []any{object{"type": "InternalIP", "address": "10.0.99.4"}}, "conditions": healthyWorkerConditions()}}
}

func healthyWorkerConditions() []any {
	return []any{object{"type": "Ready", "status": "True"}, object{"type": "DiskPressure", "status": "False"}, object{"type": "MemoryPressure", "status": "False"}, object{"type": "PIDPressure", "status": "False"}}
}

func TestEmptyDiskMapCannotRecreateAFilesystemDefault(t *testing.T) {
	if err := verifyDiskDefaults(nodeFixture()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(object){
		"old registration label": func(n object) {
			n["metadata"].(object)["labels"].(object)["node.longhorn.io/create-default-disk"] = "true"
		},
		"missing annotation": func(n object) {
			delete(n["metadata"].(object)["annotations"].(object), "node.longhorn.io/default-disks-config")
		},
		"filesystem fallback": func(n object) {
			n["metadata"].(object)["annotations"].(object)["node.longhorn.io/default-disks-config"] = `[{"name":"default","path":"/var/lib/longhorn","diskType":"filesystem"}]`
		},
	} {
		t.Run(name, func(t *testing.T) {
			n := nodeFixture()
			mutate(n)
			if err := verifyDiskDefaults(n); err == nil {
				t.Fatal("old filesystem defaults could race with disk removal")
			}
		})
	}
}

func TestReviewedBlockDiskAllowsNativeEmptyTagDefaults(t *testing.T) {
	for _, tags := range []any{nil, []any{}} {
		d := blockDisks()
		d["longhorn-v2"].(object)["tags"] = tags
		if !reviewedBlockDisks(d) {
			t.Fatal("native empty tag representation rejected")
		}
	}
	for name, mutate := range map[string]func(object){
		"external path":      func(d object) { d["longhorn-v2"].(object)["path"] = "/dev/sdb" },
		"extra disk":         func(d object) { d["other"] = object{} },
		"foreign tags":       func(d object) { d["longhorn-v2"].(object)["tags"] = []any{"foreign"} },
		"eviction requested": func(d object) { d["longhorn-v2"].(object)["evictionRequested"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			d := blockDisks()
			mutate(d)
			if reviewedBlockDisks(d) {
				t.Fatal("foreign disk state accepted")
			}
		})
	}
}

func TestResetTargetMustRetainHardwareIdentityAndPrivateAddress(t *testing.T) {
	j := journalFixture()
	address, err := verifyNode(nodeFixture(), j, true)
	if err != nil || address != "10.0.99.4" {
		t.Fatalf("known worker rejected: %v", err)
	}
	for name, mutate := range map[string]func(object){
		"replacement server":            func(n object) { n["spec"].(object)["providerID"] = "hcloud://43" },
		"different machine":             func(n object) { n["status"].(object)["nodeInfo"].(object)["systemUUID"] = "other" },
		"replacement node before reset": func(n object) { n["metadata"].(object)["uid"] = "replacement" },
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
			if _, err := verifyNode(n, j, true); err == nil {
				t.Fatal("changed reset target accepted")
			}
		})
	}
	n := nodeFixture()
	n["metadata"].(object)["uid"] = "reregistered"
	if _, err := verifyNode(n, j, false); err != nil {
		t.Fatal("same hardware may register a new Kubernetes UID after reset")
	}
}
