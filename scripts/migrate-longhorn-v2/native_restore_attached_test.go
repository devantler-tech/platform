package main

import (
	"fmt"
	"strings"
	"testing"
)

func nativeRestoreAttachedFixture(t *testing.T, reattached bool) (benchmarkRecord, canarySeedState, nativeRestoreState, nativeRestoreAttachedObservation) {
	t.Helper()
	r, seed, state, pod, digest := nativeRestoreConsumerFixture(t, reattached)
	value(pod, "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("d", 64)
	_, _, _, claim, class, pv, volume := nativeRestoreStorageFixture(t)
	_, nodes, storage := nativeRestoreTargetsFixture()
	tag := "longhorn-native-v2-" + state.Run
	for _, node := range storage {
		valueObject(node, "metadata", "annotations")[benchmarkOwner] = tag
		valueObject(node, "spec")["tags"] = []any{tag}
	}
	consumer := text(pod, "spec", "nodeName")
	engineNode := state.Targets[2].Name
	valueObject(volume, "spec")["nodeID"] = consumer
	valueObject(volume, "spec")["frontend"] = "blockdev"
	valueObject(volume, "status")["state"] = "attached"
	valueObject(volume, "status")["robustness"] = "healthy"
	valueObject(volume, "status")["currentNodeID"] = consumer
	valueObject(volume, "status")["currentEngineNodeID"] = engineNode
	valueObject(volume, "status")["frontendDisabled"] = false
	name := text(volume, "metadata", "name")
	meta := func(resourceName string) object {
		return object{"name": resourceName, "namespace": "longhorn-system", "uid": resourceName + "-uid", "resourceVersion": "13", "ownerReferences": []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": name, "uid": text(volume, "metadata", "uid")}}}
	}
	engine := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Engine", "metadata": meta("native-engine"),
		"spec":   object{"volumeName": name, "volumeSize": "4294967296", "nodeID": engineNode, "dataEngine": "v2", "active": true, "desireState": "running"},
		"status": object{"currentState": "running", "currentSize": "4294967296", "storageIP": "198.51.100.24", "port": float64(9501), "replicaModeMap": object{}, "instanceManagerName": "instance-manager-" + engineNode}}
	frontend := object{"apiVersion": "longhorn.io/v1beta2", "kind": "EngineFrontend", "metadata": meta("native-frontend"),
		"spec":   object{"volumeName": name, "nodeID": consumer, "dataEngine": "v2", "active": true, "desireState": "running", "engineName": "native-engine", "frontend": "blockdev", "disableFrontend": false, "size": "4294967296", "targetIP": "198.51.100.24", "targetPort": float64(9501)},
		"status": object{"currentState": "running", "endpoint": "/dev/longhorn/" + name, "targetIP": "198.51.100.24", "targetPort": float64(9501), "currentSize": "4294967296", "instanceManagerName": "instance-manager-" + consumer}}
	s := inventory{nodes: nodes, storageNodes: storage, volumes: []object{volume}, engines: []object{engine}, pods: []object{pod}}
	managers := map[string]nativeRestoreV2ManagerIdentity{}
	for i, target := range state.Targets {
		mapper, err := target.Preparation.mapper(target.Name)
		if err != nil {
			t.Fatal(err)
		}
		replicaName := fmt.Sprintf("native-replica-%d", i)
		s.replicas = append(s.replicas, object{"apiVersion": "longhorn.io/v1beta2", "kind": "Replica", "metadata": meta(replicaName),
			"spec":   object{"volumeName": name, "volumeSize": "4294967296", "engineName": "native-engine", "dataEngine": "v2", "active": true, "nodeID": target.Name, "diskID": target.Preparation.DiskUUID, "diskPath": mapper, "healthyAt": "2026-11-15T10:00:00Z", "failedAt": ""},
			"status": object{"currentState": "running", "instanceManagerName": "instance-manager-" + target.Name}})
		valueObject(engine, "status", "replicaModeMap")[replicaName] = "RW"
		manager, pod := nativeRestoreAttachedManagerFixture(target, i)
		managers[target.Name], err = qualifyNativeRestoreV2ManagerIdentity(target, storage[i], manager, pod)
		if err != nil {
			t.Fatal("fixture does not qualify a V2 manager", err)
		}
	}
	return r, seed, state, nativeRestoreAttachedObservation{Claim: claim, Class: class, PV: pv, Pod: pod, Inventory: s, Frontends: []object{frontend}, Managers: managers, Config: poolConfigFixture(), HelperDigest: digest}
}

func nativeRestoreAttachedManagerFixture(target nativeRestoreTarget, i int) (object, object) {
	name := "instance-manager-" + target.Name
	return nativeRestoreManagerObjects(target, name, "manager-"+target.Name, "pod-"+target.Name, fmt.Sprint(i+1))
}

func TestNativeRestoreAttachmentRequiresTheQualifiedManagerForEachRunningInstance(t *testing.T) {
	for _, kind := range []string{"engine", "frontend", "replica"} {
		for _, fault := range []string{"missing", "foreign", "wrong qualified node"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				record, seed, state, observed := nativeRestoreAttachedFixture(t, false)
				instance := map[string]object{"engine": observed.Inventory.engines[0], "frontend": observed.Frontends[0], "replica": observed.Inventory.replicas[0]}[kind]
				status := valueObject(instance, "status")
				switch fault {
				case "missing":
					delete(status, "instanceManagerName")
				case "foreign":
					status["instanceManagerName"] = "instance-manager-unqualified"
				case "wrong qualified node":
					for _, target := range state.Targets {
						if target.Name != text(instance, "spec", "nodeID") {
							status["instanceManagerName"] = "instance-manager-" + target.Name
							break
						}
					}
				}
				if _, err := verifyNativeRestoreAttached(record, seed, state, observed); err == nil {
					t.Fatal("a running instance was accepted without its placement's qualified manager")
				}
			})
		}
	}
}

func TestNativeRestoreAttachmentRequiresConsumerProcessIdentity(t *testing.T) {
	for _, fault := range []string{"missing ID", "invalid ID", "missing start", "future start"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, o := nativeRestoreAttachedFixture(t, false)
			process := value(o.Pod, "status", "containerStatuses").([]any)[0].(object)
			switch fault {
			case "missing ID":
				delete(process, "containerID")
			case "invalid ID":
				process["containerID"] = "unknown-process"
			case "missing start":
				delete(valueObject(process, "state", "running"), "startedAt")
			case "future start":
				valueObject(process, "state", "running")["startedAt"] = "2099-11-16T00:00:00Z"
			}
			if _, err := verifyNativeRestoreAttached(r, seed, state, o); err == nil {
				t.Fatal("a Pod identity without its current process epoch was accepted:", fault)
			}
		})
	}
}

func TestNativeRestoreAttachmentWaitsForTheEntireBackendSize(t *testing.T) {
	for _, fault := range []string{"engine desired size", "engine current size", "replica desired size", "engine expanding", "volume expansion required"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, o := nativeRestoreAttachedFixture(t, false)
			switch fault {
			case "engine desired size":
				valueObject(o.Inventory.engines[0], "spec")["volumeSize"] = "2147483648"
			case "engine current size":
				valueObject(o.Inventory.engines[0], "status")["currentSize"] = "2147483648"
			case "replica desired size":
				valueObject(o.Inventory.replicas[0], "spec")["volumeSize"] = "2147483648"
			case "engine expanding":
				valueObject(o.Inventory.engines[0], "status")["isExpanding"] = true
			case "volume expansion required":
				valueObject(o.Inventory.volumes[0], "status")["expansionRequired"] = true
			}
			if _, err := verifyNativeRestoreAttached(r, seed, state, o); err == nil {
				t.Fatal("a ready frontend hid an incomplete backend expansion:", fault)
			}
		})
	}
}

func TestNativeRestoreAttachmentAllowsASettledExpansionWithAStaleError(t *testing.T) {
	r, seed, state, o := nativeRestoreAttachedFixture(t, false)
	valueObject(o.Inventory.engines[0], "status")["lastExpansionError"] = "a previous expansion failed"
	if _, err := verifyNativeRestoreAttached(r, seed, state, o); err != nil {
		t.Fatal("settled size and health were rejected because an earlier error remains", err)
	}
}

func TestNativeRestoreAttachmentRequiresOriginalVolumeOwner(t *testing.T) {
	for _, kind := range []string{"engine", "frontend", "replica"} {
		for _, fault := range []string{"missing", "replaced UID", "wrong name", "wrong kind", "wrong api", "ambiguous"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				r, seed, state, o := nativeRestoreAttachedFixture(t, false)
				instance := map[string]object{"engine": o.Inventory.engines[0], "frontend": o.Frontends[0], "replica": o.Inventory.replicas[0]}[kind]
				refs := value(instance, "metadata", "ownerReferences").([]any)
				owner := refs[0].(object)
				switch fault {
				case "missing":
					delete(valueObject(instance, "metadata"), "ownerReferences")
				case "replaced UID":
					owner["uid"] = "replaced-volume-uid"
				case "wrong name":
					owner["name"] = "another-volume"
				case "wrong kind":
					owner["kind"] = "ConfigMap"
				case "wrong api":
					owner["apiVersion"] = "unreviewed/v1"
				case "ambiguous":
					valueObject(instance, "metadata")["ownerReferences"] = append(refs, owner)
				}
				if _, err := verifyNativeRestoreAttached(r, seed, state, o); err == nil {
					t.Fatal("a same-named instance without its original Volume owner was accepted")
				}
			})
		}
	}
}

func TestNativeRestoreAttachmentKeepsEngineTargetSeparateFromConsumer(t *testing.T) {
	for _, reattached := range []bool{false, true} {
		r, seed, state, observed := nativeRestoreAttachedFixture(t, reattached)
		got, err := verifyNativeRestoreAttached(r, seed, state, observed)
		if err != nil || got.Consumer != benchmarkIdentity(observed.Pod) || got.EngineNode == got.ConsumerNode || got.Frontend != benchmarkIdentity(observed.Frontends[0]) || got.Replicas[0].UID == "" || got.Replicas[2].UID == "" {
			t.Fatal("the original complete V2 graph was not proven across distinct target and consumer nodes", err, got)
		}
	}
}

func TestNativeRestoreAttachmentRejectsIncompleteOrBorrowedDataPaths(t *testing.T) {
	for _, fault := range []string{"unowned volume", "unowned claim", "consumer replaced", "volume not attached", "degraded", "wrong current consumer", "wrong target node", "engine stopped", "duplicate active engine", "no frontends", "frontend replaced with engine UID", "frontend terminating", "frontend wrong engine", "frontend wrong node", "frontend stopped", "frontend disabled", "volume frontend disabled", "unsupported frontend", "mismatched frontend", "empty endpoint", "switchover", "desired target differs", "observed target differs", "observed port differs", "bad target port", "bad target address", "frontend size differs", "two copies", "non-RW copy", "unqualified disk UUID", "partition UUID instead of disk UUID", "duplicate replica node", "replaced target boot", "unrelated unhealthy volume"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, o := nativeRestoreAttachedFixture(t, false)
			v, e, f := o.Inventory.volumes[0], o.Inventory.engines[0], o.Frontends[0]
			switch fault {
			case "unowned volume":
				valueObject(v, "metadata")["uid"] = "other-volume"
			case "unowned claim":
				valueObject(o.Claim, "metadata")["uid"] = "other-claim"
			case "consumer replaced":
				valueObject(o.Pod, "metadata")["uid"] = "other-consumer"
			case "volume not attached":
				valueObject(v, "status")["state"] = "attaching"
			case "degraded":
				valueObject(v, "status")["robustness"] = "degraded"
			case "wrong current consumer":
				valueObject(v, "status")["currentNodeID"] = state.Targets[1].Name
			case "wrong target node":
				valueObject(v, "status")["currentEngineNodeID"] = state.Targets[1].Name
			case "engine stopped":
				valueObject(e, "status")["currentState"] = "stopped"
			case "duplicate active engine":
				o.Inventory.engines = append(o.Inventory.engines, e)
			case "no frontends":
				o.Frontends = nil
			case "frontend replaced with engine UID":
				valueObject(f, "metadata")["uid"] = text(e, "metadata", "uid")
			case "frontend terminating":
				valueObject(f, "metadata")["deletionTimestamp"] = "2026-11-16T08:00:00Z"
			case "frontend wrong engine":
				valueObject(f, "spec")["engineName"] = "another-engine"
			case "frontend wrong node":
				valueObject(f, "spec")["nodeID"] = state.Targets[1].Name
			case "frontend stopped":
				valueObject(f, "status")["currentState"] = "stopped"
			case "frontend disabled":
				valueObject(f, "spec")["disableFrontend"] = true
			case "volume frontend disabled":
				valueObject(v, "status")["frontendDisabled"] = true
			case "unsupported frontend":
				valueObject(v, "spec")["frontend"], valueObject(f, "spec")["frontend"] = "iscsi", "iscsi"
			case "mismatched frontend":
				valueObject(f, "spec")["frontend"] = "ublk"
			case "empty endpoint":
				valueObject(f, "status")["endpoint"] = ""
			case "switchover":
				valueObject(f, "status")["switchoverPhase"] = "switching"
			case "desired target differs":
				valueObject(f, "spec")["targetIP"] = "198.51.100.25"
			case "observed target differs":
				valueObject(f, "status")["targetIP"] = "198.51.100.25"
			case "observed port differs":
				valueObject(f, "status")["targetPort"] = float64(9502)
			case "bad target port":
				valueObject(e, "status")["port"] = float64(0)
			case "bad target address":
				valueObject(e, "status")["storageIP"] = "not-an-ip"
			case "frontend size differs":
				valueObject(f, "status")["currentSize"] = "2147483648"
			case "two copies":
				o.Inventory.replicas = o.Inventory.replicas[:2]
			case "non-RW copy":
				valueObject(e, "status", "replicaModeMap")[text(o.Inventory.replicas[0], "metadata", "name")] = "WO"
			case "unqualified disk UUID":
				valueObject(o.Inventory.replicas[0], "spec")["diskID"] = "other-disk"
			case "partition UUID instead of disk UUID":
				valueObject(o.Inventory.replicas[0], "spec")["diskID"] = state.Targets[0].Preparation.RawPartitionUUID
			case "duplicate replica node":
				valueObject(o.Inventory.replicas[0], "spec")["nodeID"] = state.Targets[1].Name
			case "replaced target boot":
				valueObject(o.Inventory.nodes[0], "status", "nodeInfo")["bootID"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			case "unrelated unhealthy volume":
				o.Inventory.volumes = append(o.Inventory.volumes, object{"metadata": object{"name": "application", "uid": "application-uid"}, "spec": object{"dataEngine": "v1", "numberOfReplicas": float64(3)}, "status": object{"state": "attached", "robustness": "faulted"}})
			}
			if _, err := verifyNativeRestoreAttached(r, seed, state, o); err == nil {
				t.Fatal("unsafe V2 attachment was accepted:", fault)
			}
		})
	}
}
