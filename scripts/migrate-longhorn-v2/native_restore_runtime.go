package main

import (
	"context"
	"reflect"
	"strings"
	"time"
)

type nativeRestoreVeleroEpoch struct {
	Deployment, ReplicaSet, Pod benchmarkResource
	Generation                  float64
	ContainerID, Digest         string
	StartedAt                   time.Time
}

// Bind the restore helper's image to the original live server process, rather
// than resolving its mutable tag independently. Rebind this complete epoch
// before and after the native restore; an unrelated Ready Pod is not proof.
func qualifyNativeRestoreVelero(deployment object, replicas, pods []object) (nativeRestoreVeleroEpoch, error) {
	fail := func() (nativeRestoreVeleroEpoch, error) { return nativeRestoreVeleroEpoch{}, errNativeRestoreEvidence }
	if !nativeVeleroRuntimeIdentity(deployment, "apps/v1", "Deployment") || text(deployment, "metadata", "name") != "velero" ||
		text(deployment, "metadata", "annotations", "meta.helm.sh/release-name") != "velero" ||
		text(deployment, "metadata", "annotations", "meta.helm.sh/release-namespace") != "velero" || value(deployment, "spec", "replicas") != float64(1) {
		return fail()
	}
	generation, ok := value(deployment, "metadata", "generation").(float64)
	if !ok || generation < 1 || value(deployment, "status", "observedGeneration") != generation {
		return fail()
	}
	for _, field := range []string{"replicas", "updatedReplicas", "readyReplicas", "availableReplicas"} {
		if value(deployment, "status", field) != float64(1) {
			return fail()
		}
	}
	if n := value(deployment, "status", "unavailableReplicas"); n != nil && n != float64(0) {
		return fail()
	}
	selector, ok := value(deployment, "spec", "selector", "matchLabels").(object)
	if !ok || len(selector) == 0 || value(deployment, "spec", "selector", "matchExpressions") != nil || !nativeVeleroRuntimeContainer(valueObject(deployment, "spec", "template", "spec")) {
		return fail()
	}
	seen := map[string]bool{text(deployment, "metadata", "uid"): true}
	names := map[string]bool{}
	var original object
	for _, replica := range replicas {
		id := benchmarkIdentity(replica)
		if !nativeVeleroRuntimeIdentity(replica, "apps/v1", "ReplicaSet") || seen[id.UID] || names[id.Name] {
			return fail()
		}
		seen[id.UID], names[id.Name] = true, true
		if !nativeVeleroRuntimeOwner(replica, deployment, "Deployment") {
			continue
		}
		if value(replica, "spec", "replicas") == float64(0) {
			if n := value(replica, "status", "replicas"); n != nil && n != float64(0) {
				return fail()
			}
			continue
		}
		if original != nil || value(replica, "spec", "replicas") != float64(1) || !nativeVeleroRuntimeContainer(valueObject(replica, "spec", "template", "spec")) {
			return fail()
		}
		for _, field := range []string{"replicas", "readyReplicas", "availableReplicas"} {
			if value(replica, "status", field) != float64(1) {
				return fail()
			}
		}
		original = replica
	}
	if original == nil {
		return fail()
	}
	names = map[string]bool{}
	var consumer object
	for _, pod := range pods {
		id := benchmarkIdentity(pod)
		if !nativeVeleroRuntimeIdentity(pod, "v1", "Pod") || seen[id.UID] || names[id.Name] {
			return fail()
		}
		seen[id.UID], names[id.Name] = true, true
		if !nativeVeleroRuntimeOwner(pod, original, "ReplicaSet") {
			continue
		}
		if consumer != nil || text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) || text(pod, "spec", "nodeName") == "" ||
			!nativeVeleroRuntimeContainer(valueObject(pod, "spec")) {
			return fail()
		}
		for key, label := range selector {
			if !reflect.DeepEqual(value(pod, "metadata", "labels", key), label) {
				return fail()
			}
		}
		consumer = pod
	}
	statuses, valid := value(consumer, "status", "containerStatuses").([]any)
	if consumer == nil || !valid || len(statuses) != 1 {
		return fail()
	}
	process, valid := statuses[0].(object)
	if !valid || text(process, "name") != "velero" || value(process, "ready") != true || value(process, "restartCount") != float64(0) || !benchmarkContainerID.MatchString(text(process, "containerID")) {
		return fail()
	}
	started, err := time.Parse(time.RFC3339Nano, text(process, "state", "running", "startedAt"))
	image := text(process, "imageID")
	digest := strings.TrimPrefix(image, "sha256:")
	if _, suffix, found := strings.Cut(image, "@sha256:"); found {
		digest = suffix
	}
	if err != nil || started.IsZero() || started.After(time.Now()) || !seedSHA256.MatchString(digest) {
		return fail()
	}
	return nativeRestoreVeleroEpoch{Deployment: benchmarkIdentity(deployment), ReplicaSet: benchmarkIdentity(original), Pod: benchmarkIdentity(consumer), Generation: generation,
		ContainerID: text(process, "containerID"), Digest: digest, StartedAt: started}, nil
}

func nativeVeleroRuntimeIdentity(o object, api, kind string) bool {
	return seedVeleroIdentity(o) && (text(o, "apiVersion") == "" || text(o, "apiVersion") == api) && (text(o, "kind") == "" || text(o, "kind") == kind)
}

func nativeVeleroRuntimeContainer(spec object) bool {
	containers, ok := value(spec, "containers").([]any)
	if !ok || len(containers) != 1 || value(spec, "ephemeralContainers") != nil {
		return false
	}
	container, ok := containers[0].(object)
	return ok && text(container, "name") == "velero" && text(container, "image") == "velero/velero:v1.18.4"
}

func nativeVeleroRuntimeOwner(child, parent object, kind string) bool {
	refs, ok := value(child, "metadata", "ownerReferences").([]any)
	if !ok || len(refs) != 1 {
		return false
	}
	ref, ok := refs[0].(object)
	return ok && text(ref, "apiVersion") == "apps/v1" && text(ref, "kind") == kind && value(ref, "controller") == true &&
		text(ref, "name") == text(parent, "metadata", "name") && text(ref, "uid") == text(parent, "metadata", "uid")
}

func captureNativeRestoreVelero(ctx context.Context, c client, source string) (nativeRestoreVeleroEpoch, error) {
	if c.context != "admin@prod" || sourceCurrent(ctx, c.run, source) != nil {
		return nativeRestoreVeleroEpoch{}, errNativeRestoreEvidence
	}
	read := func() (nativeRestoreVeleroEpoch, error) {
		deployment, err := c.get(ctx, "deployment", "velero", "velero", false)
		if err != nil {
			return nativeRestoreVeleroEpoch{}, errNativeRestoreEvidence
		}
		replicas, err := c.list(ctx, "replicasets", "velero")
		if err != nil {
			return nativeRestoreVeleroEpoch{}, errNativeRestoreEvidence
		}
		pods, err := c.list(ctx, "pods", "velero")
		if err != nil {
			return nativeRestoreVeleroEpoch{}, errNativeRestoreEvidence
		}
		return qualifyNativeRestoreVelero(deployment, replicas, pods)
	}
	before, err := read()
	if err != nil {
		return nativeRestoreVeleroEpoch{}, err
	}
	after, err := read()
	if err != nil || before != after || sourceCurrent(ctx, c.run, source) != nil || !commandContextActive(ctx) {
		return nativeRestoreVeleroEpoch{}, errNativeRestoreEvidence
	}
	return after, nil
}
