package main

import (
	"crypto/sha512"
	"encoding/hex"
	"reflect"
	"strings"
	"time"
)

type nativeRestoreV2ManagerIdentity struct {
	Pod, PodUID, ManagerUID, Node, ContainerID string
	StartedAt                                  time.Time
}

// The staging clients expose the Docker Hub v1.13.0 index digest as their image ID.
const nativeRestoreStockImageID = runtimeStockRepository + "@sha256:fc9e1c15e81988b04c146866ce7010b6df3a70f88302809bf53e19b27d1658a3"

// Bind only the native ownership and serving-process identity. This is not a
// runtime or capacity clearance: the caller must also qualify the complete
// cohort, settings, argv, mounts, admission, physical hosts and storage graph.
func qualifyNativeRestoreV2ManagerIdentity(target nativeRestoreTarget, storage, manager, pod object) (nativeRestoreV2ManagerIdentity, error) {
	fail := func() (nativeRestoreV2ManagerIdentity, error) {
		return nativeRestoreV2ManagerIdentity{}, errNativeRestoreEvidence
	}
	if _, err := target.Preparation.mapper(target.Name); err != nil {
		return fail()
	}
	name := text(manager, "metadata", "name")
	for _, item := range []struct {
		o         object
		api, kind string
	}{
		{storage, "longhorn.io/v1beta2", "Node"},
		{manager, "longhorn.io/v1beta2", "InstanceManager"},
		{pod, "v1", "Pod"},
	} {
		if text(item.o, "apiVersion") != item.api || text(item.o, "kind") != item.kind ||
			text(item.o, "metadata", "namespace") != "longhorn-system" || text(item.o, "metadata", "resourceVersion") == "" ||
			value(item.o, "metadata", "deletionTimestamp") != nil {
			return fail()
		}
	}
	seen := map[string]bool{}
	for _, uid := range []string{target.UID, target.Preparation.LonghornUID, target.Preparation.Journal.UID, text(manager, "metadata", "uid"), text(pod, "metadata", "uid")} {
		if !nativeRestoreUID(uid) || seen[uid] {
			return fail()
		}
		seen[uid] = true
	}
	managerOwner := []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Node", "name": target.Name, "uid": target.Preparation.LonghornUID, "blockOwnerDeletion": true}}
	podOwner := []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "InstanceManager", "name": name, "uid": text(manager, "metadata", "uid"), "controller": true, "blockOwnerDeletion": true}}
	image := text(manager, "spec", "image")
	if text(storage, "metadata", "name") != target.Name || text(storage, "metadata", "uid") != target.Preparation.LonghornUID ||
		!strings.HasPrefix(name, "instance-manager-") || safeKubeRequest("pod", "longhorn-system", name, false) != nil || text(pod, "metadata", "name") != name ||
		text(manager, "spec", "nodeID") != target.Name || !nativeRestoreStockImage(image) ||
		text(manager, "spec", "type") != "aio" || text(manager, "spec", "dataEngine") != "v2" ||
		text(manager, "status", "currentState") != "running" || text(manager, "status", "ownerID") != target.Name ||
		!reflect.DeepEqual(value(manager, "metadata", "ownerReferences"), managerOwner) || !reflect.DeepEqual(value(pod, "metadata", "ownerReferences"), podOwner) ||
		text(pod, "spec", "nodeName") != target.Name || text(pod, "spec", "serviceAccountName") != "longhorn-service-account" || text(pod, "spec", "restartPolicy") != "Never" ||
		text(pod, "status", "phase") != "Running" || !conditionsTrue(valueObject(pod, "status"), "Ready") {
		return fail()
	}
	digest := sha512.Sum512([]byte(strings.NewReplacer(":", "-", "/", "-").Replace(image)))
	labels := object{"longhorn.io/managed-by": "longhorn-manager", "longhorn.io/component": "instance-manager", "longhorn.io/node": target.Name,
		"longhorn.io/instance-manager-type": "aio", "longhorn.io/data-engine": "v2", "longhorn.io/instance-manager-image": "imi-" + hex.EncodeToString(digest[:])[:8]}
	for key, want := range labels {
		if value(manager, "metadata", "labels", key) != want || value(pod, "metadata", "labels", key) != want {
			return fail()
		}
	}
	for _, key := range []string{"hostPID", "hostIPC", "hostNetwork", "shareProcessNamespace"} {
		if actual := value(pod, "spec", key); actual != nil && actual != false {
			return fail()
		}
	}
	for _, key := range []string{"initContainers", "ephemeralContainers"} {
		if !nativeRestoreV2EmptyList(value(pod, "spec", key)) {
			return fail()
		}
	}
	for _, key := range []string{"initContainerStatuses", "ephemeralContainerStatuses"} {
		if !nativeRestoreV2EmptyList(value(pod, "status", key)) {
			return fail()
		}
	}
	containers, _ := value(pod, "spec", "containers").([]any)
	statuses, _ := value(pod, "status", "containerStatuses").([]any)
	if len(containers) != 1 || len(statuses) != 1 {
		return fail()
	}
	container, _ := containers[0].(object)
	process, _ := statuses[0].(object)
	if text(container, "name") != "instance-manager" || !nativeRestoreStockImage(text(container, "image")) || value(container, "securityContext", "privileged") != true ||
		!nativeRestoreV2EmptyList(value(container, "command")) || text(process, "name") != "instance-manager" ||
		value(process, "ready") != true || value(process, "started") != true || value(process, "restartCount") != float64(0) ||
		strings.TrimPrefix(text(process, "imageID"), "docker-pullable://") != nativeRestoreStockImageID ||
		!benchmarkContainerID.MatchString(text(process, "containerID")) || value(process, "state", "waiting") != nil || value(process, "state", "terminated") != nil {
		return fail()
	}
	started, err := time.Parse(time.RFC3339Nano, text(process, "state", "running", "startedAt"))
	if err != nil || started.IsZero() || started.After(time.Now()) {
		return fail()
	}
	return nativeRestoreV2ManagerIdentity{Pod: text(pod, "metadata", "name"), PodUID: text(pod, "metadata", "uid"), ManagerUID: text(manager, "metadata", "uid"), Node: target.Name,
		ContainerID: text(process, "containerID"), StartedAt: started}, nil
}

func nativeRestoreStockImage(image string) bool {
	return image == runtimeStockImage || image == strings.TrimPrefix(runtimeStockImage, "docker.io/")
}

func nativeRestoreV2EmptyList(raw any) bool {
	if raw == nil {
		return true
	}
	list, ok := raw.([]any)
	return ok && len(list) == 0
}
