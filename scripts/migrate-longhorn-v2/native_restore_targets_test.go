package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

func nativeRestoreTargetsFixture() ([]nativeRestoreTarget, []object, []object) {
	var targets []nativeRestoreTarget
	var nodes, storage []object
	conditions := func() []any {
		return []any{object{"type": "Ready", "status": "True"}, object{"type": "Schedulable", "status": "True"}}
	}
	for index, selection := range longhornmigration.ActivationSelections() {
		i := index + 1
		role := strings.Split(selection, "-")[1]
		name := fmt.Sprintf("autoscale-longhorn-%s-%016x", role, i)
		uid := fmt.Sprintf("kube-node-%d", i)
		preparation := nativeRestorePreparationFixture(selection, i)
		targets = append(targets, nativeRestoreTarget{Name: name, UID: uid, Preparation: preparation})
		nodes = append(nodes, object{"apiVersion": "v1", "kind": "Node", "metadata": object{"name": name, "uid": uid, "resourceVersion": "11",
			"labels":      object{longhornmigration.PoolLabel: role, "ksail.io/autoscaled": "true", "node.kubernetes.io/instance-type": "cpx42"},
			"annotations": object{longhornmigration.ScaleDownDisabled: "true", longhornmigration.OwnerAnnotation: longhornmigration.OwnerValue}},
			"spec": object{"providerID": fmt.Sprintf("hcloud://%d", i)},
			"status": object{"capacity": object{"hugepages-2Mi": "2Gi"}, "allocatable": object{"hugepages-2Mi": "2Gi"},
				"nodeInfo":   object{"systemUUID": fmt.Sprintf("machine-%d", i), "architecture": "amd64", "bootID": preparation.BootID},
				"conditions": []any{object{"type": "Ready", "status": "True"}, object{"type": "DiskPressure", "status": "False"}, object{"type": "MemoryPressure", "status": "False"}, object{"type": "PIDPressure", "status": "False"}}}})
		rawID := temporaryRawID(selection)
		storage = append(storage, object{"apiVersion": "longhorn.io/v1beta2", "kind": "Node", "metadata": object{"name": name, "namespace": "longhorn-system",
			"uid": fmt.Sprintf("storage-node-%d", i), "resourceVersion": "22", "labels": object{longhornmigration.EnabledLabel: "true"},
			"annotations": object{temporaryActivationOwner: preparation.Activation.UID}},
			"spec":   object{"allowScheduling": true, "evictionRequested": false, "disks": blockDisksFor(rawID)},
			"status": object{"conditions": conditions(), "diskStatus": object{"longhorn-v2": object{"diskUUID": fmt.Sprintf("disk-%d", i), "diskPath": "/dev/mapper/luks2-" + rawID, "diskType": "block", "diskDriver": "aio", "conditions": conditions()}}}})
	}
	return targets, nodes, storage
}

func TestNativeRestoreTargetsBindOriginalNodesAndRawDisks(t *testing.T) {
	targets, nodes, storage := nativeRestoreTargetsFixture()
	before, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", false, poolConfigFixture())
	if err != nil || len(before) != 3 || before[targets[0].Name].Node.UID != targets[0].UID || before[targets[0].Name].Storage.UID != "storage-node-1" || before[targets[0].Name].DiskUUID != "disk-1" {
		t.Fatal("the original three raw targets were not qualified:", err)
	}
	for _, n := range storage {
		valueObject(n, "spec")["tags"] = []any{"longhorn-native-v2-5678"}
		valueObject(n, "metadata", "annotations")[benchmarkOwner] = "longhorn-native-v2-5678"
	}
	after, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", true, poolConfigFixture())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("the owned tag replaced a target identity:", err)
	}
	valueObject(storage[0], "status", "diskStatus", "longhorn-v2")["diskUUID"] = "replacement-disk"
	if replaced, err := qualifyNativeRestoreTargets(targets, nodes, storage, "longhorn-native-v2-5678", true, poolConfigFixture()); err == nil || replaced != nil {
		t.Fatal("a replaced disk was accepted behind the original preparation receipt")
	}
}

func TestNativeRestoreTargetQualificationRejectsIncompleteOrUnsafeNodes(t *testing.T) {
	for _, fault := range []string{"missing node", "missing storage", "duplicate node", "duplicate storage", "replaced node", "missing revision", "node api", "storage api", "storage namespace", "terminating", "cordoned", "fenced", "pressure", "hugepages", "architecture", "provider alias", "machine alias", "storage uid alias", "disk alias", "missing disk uuid", "filesystem", "wrong path", "wrong driver", "extra disk", "not schedulable", "eviction", "storage not ready", "disk not ready", "unowned tag", "missing owned tag", "bad tag", "missing roster", "aliased roster", "unsorted roster"} {
		t.Run(fault, func(t *testing.T) {
			targets, nodes, storage := nativeRestoreTargetsFixture()
			tag, tagged := "longhorn-native-v2-5678", false
			switch fault {
			case "missing node":
				nodes = nodes[:2]
			case "missing storage":
				storage = storage[:2]
			case "duplicate node":
				nodes = append(nodes, nodes[0])
			case "duplicate storage":
				storage = append(storage, storage[0])
			case "replaced node":
				valueObject(nodes[0], "metadata")["uid"] = "other"
			case "missing revision":
				delete(valueObject(nodes[0], "metadata"), "resourceVersion")
			case "node api":
				nodes[0]["apiVersion"] = "other/v1"
			case "storage api":
				storage[0]["apiVersion"] = "other/v1"
			case "storage namespace":
				valueObject(storage[0], "metadata")["namespace"] = "other"
			case "terminating":
				valueObject(storage[0], "metadata")["deletionTimestamp"] = "2026-11-15T12:00:00Z"
			case "cordoned":
				valueObject(nodes[0], "spec")["unschedulable"] = true
			case "fenced":
				valueObject(nodes[0], "metadata")["annotations"] = object{fenceAnnotation: "other"}
			case "pressure":
				valueObject(nodes[0], "status")["conditions"] = []any{object{"type": "Ready", "status": "True"}}
			case "hugepages":
				valueObject(nodes[0], "status", "allocatable")["hugepages-2Mi"] = "0"
			case "architecture":
				valueObject(nodes[0], "status", "nodeInfo")["architecture"] = "arm64"
			case "provider alias":
				valueObject(nodes[1], "spec")["providerID"] = value(nodes[0], "spec", "providerID")
			case "machine alias":
				valueObject(nodes[1], "status", "nodeInfo")["systemUUID"] = value(nodes[0], "status", "nodeInfo", "systemUUID")
			case "storage uid alias":
				valueObject(storage[1], "metadata")["uid"] = value(storage[0], "metadata", "uid")
			case "disk alias":
				valueObject(storage[1], "status", "diskStatus", "longhorn-v2")["diskUUID"] = value(storage[0], "status", "diskStatus", "longhorn-v2", "diskUUID")
			case "missing disk uuid":
				delete(valueObject(storage[0], "status", "diskStatus", "longhorn-v2"), "diskUUID")
			case "filesystem":
				valueObject(storage[0], "spec", "disks", "longhorn-v2")["diskType"] = "filesystem"
			case "wrong path":
				valueObject(storage[0], "status", "diskStatus", "longhorn-v2")["diskPath"] = "/var/mnt/longhorn"
			case "wrong driver":
				valueObject(storage[0], "status", "diskStatus", "longhorn-v2")["diskDriver"] = "spdk"
			case "extra disk":
				valueObject(storage[0], "spec", "disks")["other"] = object{}
			case "not schedulable":
				valueObject(storage[0], "spec")["allowScheduling"] = false
			case "eviction":
				valueObject(storage[0], "spec")["evictionRequested"] = true
			case "storage not ready":
				delete(valueObject(storage[0], "status"), "conditions")
			case "disk not ready":
				delete(valueObject(storage[0], "status", "diskStatus", "longhorn-v2"), "conditions")
			case "unowned tag":
				valueObject(storage[0], "spec")["tags"] = []any{"other"}
			case "missing owned tag":
				tagged = true
			case "bad tag":
				tag = "other"
			case "missing roster":
				targets = targets[:2]
			case "aliased roster":
				targets[1].UID = targets[0].UID
			case "unsorted roster":
				targets[0], targets[1] = targets[1], targets[0]
			}
			if got, err := qualifyNativeRestoreTargets(targets, nodes, storage, tag, tagged, poolConfigFixture()); err == nil || got != nil {
				t.Fatal("unsafe raw target accepted:", fault)
			}
		})
	}
}
