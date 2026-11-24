package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func nativeRestoreV2ManagerIdentityFixture() (nativeRestoreTarget, object, object, object) {
	targets, _, storage := nativeRestoreTargetsFixture()
	target, lh := targets[1], storage[1]
	manager, pod := nativeRestoreManagerObjects(target, "instance-manager-v2-fixture", "v2-manager-uid", "v2-pod-uid", "a")
	return target, lh, manager, pod
}

func nativeRestoreManagerObjects(target nativeRestoreTarget, name, managerUID, podUID, containerHex string) (object, object) {
	labels := func() object {
		return object{"longhorn.io/managed-by": "longhorn-manager", "longhorn.io/component": "instance-manager", "longhorn.io/node": target.Name,
			"longhorn.io/instance-manager-type": "aio", "longhorn.io/data-engine": "v2", "longhorn.io/instance-manager-image": "imi-326dadd9"}
	}
	im := object{"apiVersion": "longhorn.io/v1beta2", "kind": "InstanceManager", "metadata": object{"name": name, "namespace": "longhorn-system", "uid": managerUID, "resourceVersion": "7", "labels": labels(),
		"ownerReferences": []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Node", "name": target.Name, "uid": target.Preparation.LonghornUID, "blockOwnerDeletion": true}}},
		"spec": object{"nodeID": target.Name, "image": benchmarkRetirementImage, "type": "aio", "dataEngine": "v2"}, "status": object{"currentState": "running", "ownerID": target.Name}}
	pod := object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"name": name, "namespace": "longhorn-system", "uid": podUID, "resourceVersion": "9", "labels": labels(),
		"ownerReferences": []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "InstanceManager", "name": name, "uid": managerUID, "controller": true, "blockOwnerDeletion": true}}},
		"spec": object{"nodeName": target.Name, "serviceAccountName": "longhorn-service-account", "restartPolicy": "Never", "containers": []any{object{"name": "instance-manager", "image": benchmarkRetirementImage, "securityContext": object{"privileged": true}}}},
		"status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}, "containerStatuses": []any{object{"name": "instance-manager", "ready": true, "started": true, "restartCount": float64(0),
			"imageID": benchmarkRetirementRepository + "@" + benchmarkRetirementImageDigest, "containerID": "containerd://" + strings.Repeat(containerHex, 64), "state": object{"running": object{"startedAt": "2026-11-16T12:00:00Z"}}}}}}
	return im, pod
}

func TestNativeRestoreV2ManagerIdentityBindsOwnedNativeProcess(t *testing.T) {
	target, lh, im, pod := nativeRestoreV2ManagerIdentityFixture()
	before, err := qualifyNativeRestoreV2ManagerIdentity(target, lh, im, pod)
	if err != nil || before.PodUID != "v2-pod-uid" || before.ManagerUID != "v2-manager-uid" || before.Node != target.Name || before.ContainerID == "" || before.StartedAt.IsZero() {
		t.Fatal("the V2 producer identity was not bound:", before, err)
	}
	value(pod, "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("b", 64)
	after, err := qualifyNativeRestoreV2ManagerIdentity(target, lh, im, pod)
	if err != nil || reflect.DeepEqual(before, after) {
		t.Fatal("a replaced process was invisible in the identity epoch:", after, err)
	}
}

func TestNativeRestoreV2ManagerIdentityRejectsUnownedOrAmbiguousProcesses(t *testing.T) {
	for _, fault := range []string{"node owner uid", "node owner controller", "pod owner uid", "pod owner deletion", "manager image", "pod image", "image id", "image label", "pod label", "manager mode", "manager state", "manager owner", "pod node", "pod namespace", "pod revision", "manager deleting", "pod deleting", "identity alias", "service account", "restart policy", "host pid", "sidecar", "init", "ephemeral", "not ready", "status count", "not started", "restart", "missing restart", "container id", "future process", "waiting process"} {
		t.Run(fault, func(t *testing.T) {
			target, lh, im, pod := nativeRestoreV2ManagerIdentityFixture()
			status := value(pod, "status", "containerStatuses").([]any)[0].(object)
			switch fault {
			case "node owner uid":
				value(im, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "other"
			case "node owner controller":
				value(im, "metadata", "ownerReferences").([]any)[0].(object)["controller"] = true
			case "pod owner uid":
				value(pod, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "other"
			case "pod owner deletion":
				value(pod, "metadata", "ownerReferences").([]any)[0].(object)["blockOwnerDeletion"] = false
			case "manager image":
				valueObject(im, "spec")["image"] = "unreviewed"
			case "pod image":
				value(pod, "spec", "containers").([]any)[0].(object)["image"] = "unreviewed"
			case "image id":
				status["imageID"] = "unreviewed"
			case "image label":
				valueObject(im, "metadata", "labels")["longhorn.io/instance-manager-image"] = "imi-wrong"
			case "pod label":
				valueObject(pod, "metadata", "labels")["longhorn.io/data-engine"] = "v1"
			case "manager mode":
				valueObject(im, "spec")["dataEngine"] = "v1"
			case "manager state":
				valueObject(im, "status")["currentState"] = "starting"
			case "manager owner":
				valueObject(im, "status")["ownerID"] = "other"
			case "pod node":
				valueObject(pod, "spec")["nodeName"] = "other"
			case "pod namespace":
				valueObject(pod, "metadata")["namespace"] = "other"
			case "pod revision":
				delete(valueObject(pod, "metadata"), "resourceVersion")
			case "manager deleting":
				valueObject(im, "metadata")["deletionTimestamp"] = "2026-11-16T12:00:00Z"
			case "pod deleting":
				valueObject(pod, "metadata")["deletionTimestamp"] = "2026-11-16T12:00:00Z"
			case "identity alias":
				valueObject(pod, "metadata")["uid"] = target.UID
			case "service account":
				valueObject(pod, "spec")["serviceAccountName"] = "other"
			case "restart policy":
				valueObject(pod, "spec")["restartPolicy"] = "Always"
			case "host pid":
				valueObject(pod, "spec")["hostPID"] = true
			case "sidecar":
				valueObject(pod, "spec")["containers"] = append(value(pod, "spec", "containers").([]any), object{"name": "other"})
			case "init":
				valueObject(pod, "spec")["initContainers"] = []any{object{"name": "other"}}
			case "ephemeral":
				valueObject(pod, "spec")["ephemeralContainers"] = []any{object{"name": "other"}}
			case "not ready":
				status["ready"] = false
			case "status count":
				valueObject(pod, "status")["containerStatuses"] = []any{}
			case "not started":
				status["started"] = false
			case "restart":
				status["restartCount"] = float64(1)
			case "missing restart":
				delete(status, "restartCount")
			case "container id":
				status["containerID"] = "unknown"
			case "future process":
				valueObject(status, "state", "running")["startedAt"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			case "waiting process":
				valueObject(status, "state")["waiting"] = object{"reason": "Unknown"}
			}
			if got, err := qualifyNativeRestoreV2ManagerIdentity(target, lh, im, pod); err == nil || got != (nativeRestoreV2ManagerIdentity{}) {
				t.Fatal("unqualified V2 manager identity was accepted:", fault, got)
			}
		})
	}
}
