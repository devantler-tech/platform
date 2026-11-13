package main

import (
	"errors"
	"path"
	"reflect"
	"sort"
	"strings"
)

const cacheAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes"

func controllerOwner(value object, kind string) string {
	refs, ok := at(value, "metadata", "ownerReferences").([]any)
	if !ok {
		return ""
	}
	uid := ""
	for _, ref := range refs {
		if at(ref, "controller") == true {
			if uid != "" || text(ref, "kind") != kind {
				return ""
			}
			uid = text(ref, "uid")
		}
	}
	return uid
}

func cacheIsEphemeral(spec any) bool {
	volumes, ok := at(spec, "volumes").([]any)
	if !ok {
		return false
	}
	cacheVolumes := 0
	for _, volume := range volumes {
		if text(volume, "name") != "sigstore" {
			continue
		}
		v, ok := volume.(map[string]any)
		if !ok || len(v) != 2 {
			return false
		}
		if _, ok = v["emptyDir"].(map[string]any); !ok {
			return false
		}
		cacheVolumes++
	}
	containers, ok := at(spec, "containers").([]any)
	if !ok || len(containers) != 1 || text(containers[0], "name") != "kyverno" || cacheVolumes != 1 {
		return false
	}
	env, ok := at(containers[0], "env").([]any)
	if !ok {
		return false
	}
	roots := 0
	for _, e := range env {
		if text(e, "name") == "TUF_ROOT" {
			if text(e, "value") != "/.sigstore" || at(e, "valueFrom") != nil {
				return false
			}
			roots++
		}
	}
	mounts, ok := at(containers[0], "volumeMounts").([]any)
	if !ok {
		return false
	}
	cacheMounts := 0
	for _, mount := range mounts {
		mountPath := path.Clean(text(mount, "mountPath"))
		if text(mount, "name") == "sigstore" {
			if text(mount, "mountPath") != "/.sigstore" || text(mount, "subPath") != "" || text(mount, "subPathExpr") != "" || at(mount, "readOnly") == true {
				return false
			}
			cacheMounts++
		} else if mountPath == "/" || mountPath == "/.sigstore" || strings.HasPrefix(mountPath, "/.sigstore/") {
			return false
		}
	}
	return roots == 1 && cacheMounts == 1
}

func sameExecution(left, right any) bool {
	a, aOK := at(left, "containers").([]any)
	b, bOK := at(right, "containers").([]any)
	if !aOK || !bOK || len(a) != 1 || len(b) != 1 {
		return false
	}
	for _, key := range []string{"name", "image", "command", "args", "env", "envFrom", "workingDir"} {
		if !reflect.DeepEqual(at(a[0], key), at(b[0], key)) {
			return false
		}
	}
	return true
}

func qualifyControllers(deploymentRaw, pdbRaw, replicaSetsRaw, podsRaw []byte) ([]object, error) {
	fail := errors.New("admission replicas, disruption floor or ephemeral cache is unqualified")
	d, err := nativeObject(deploymentRaw, "apps/v1", "Deployment", "kyverno-admission-controller", "kyverno")
	if err != nil {
		return nil, err
	}
	generation, ok := at(d, "metadata", "generation").(float64)
	if !ok || generation <= 0 || at(d, "spec", "replicas") != float64(2) || at(d, "status", "readyReplicas") != float64(2) || at(d, "status", "availableReplicas") != float64(2) || at(d, "status", "updatedReplicas") != float64(2) || at(d, "status", "observedGeneration") != generation || text(d, "spec", "template", "metadata", "annotations", cacheAnnotation) != "sigstore" || !cacheIsEphemeral(at(d, "spec", "template", "spec")) {
		return nil, fail
	}
	pdb, err := nativeObject(pdbRaw, "policy/v1", "PodDisruptionBudget", "kyverno-admission-controller", "kyverno")
	if err != nil {
		return nil, err
	}
	if at(pdb, "spec", "minAvailable") != float64(1) || at(pdb, "spec", "maxUnavailable") != nil || !reflect.DeepEqual(at(pdb, "spec", "selector"), at(d, "spec", "selector")) || at(pdb, "status", "currentHealthy") != float64(2) || at(pdb, "status", "desiredHealthy") != float64(1) || at(pdb, "status", "disruptionsAllowed") != float64(1) {
		return nil, fail
	}
	sets, err := completeList(replicaSetsRaw, "apps/v1", "ReplicaSetList")
	if err != nil {
		return nil, err
	}
	owned := map[string]object{}
	for _, rs := range sets {
		if controllerOwner(rs, "Deployment") == text(d, "metadata", "uid") && text(rs, "metadata", "namespace") == "kyverno" {
			owned[text(rs, "metadata", "uid")] = rs
		}
	}
	pods, err := completeList(podsRaw, "v1", "PodList")
	if err != nil {
		return nil, err
	}
	bindings := []object{d, pdb}
	usedSets := map[string]bool{}
	count := 0
	for _, pod := range pods {
		rsUID := controllerOwner(pod, "ReplicaSet")
		rs, ownedPod := owned[rsUID]
		component := text(pod, "metadata", "labels", "app.kubernetes.io/component")
		if !ownedPod && component != "admission-controller" {
			continue
		}
		if !ownedPod || component != "admission-controller" || text(pod, "metadata", "namespace") != "kyverno" || text(pod, "metadata", "annotations", cacheAnnotation) != "sigstore" || text(pod, "spec", "nodeName") == "" || text(pod, "status", "phase") != "Running" || !cacheIsEphemeral(pod["spec"]) || text(rs, "spec", "template", "metadata", "annotations", cacheAnnotation) != "sigstore" || !cacheIsEphemeral(at(rs, "spec", "template", "spec")) {
			return nil, fail
		}
		if !sameExecution(pod["spec"], at(d, "spec", "template", "spec")) || !sameExecution(pod["spec"], at(rs, "spec", "template", "spec")) {
			return nil, fail
		}
		labels, ok := at(d, "spec", "selector", "matchLabels").(map[string]any)
		if !ok || len(labels) == 0 {
			return nil, fail
		}
		for key, value := range labels {
			wanted, ok := value.(string)
			if !ok || text(pod, "metadata", "labels", key) != wanted {
				return nil, fail
			}
		}
		conditions, ok := at(pod, "status", "conditions").([]any)
		if !ok {
			return nil, fail
		}
		ready := 0
		for _, condition := range conditions {
			if text(condition, "type") == "Ready" {
				if text(condition, "status") != "True" {
					return nil, fail
				}
				ready++
			}
		}
		statuses, ok := at(pod, "status", "containerStatuses").([]any)
		if ready != 1 || !ok || len(statuses) != 1 || text(statuses[0], "name") != "kyverno" || at(statuses[0], "ready") != true || at(statuses[0], "started") != true || text(statuses[0], "containerID") == "" || text(statuses[0], "imageID") == "" || text(statuses[0], "state", "running", "startedAt") == "" {
			return nil, fail
		}
		bindings = append(bindings, pod)
		usedSets[rsUID] = true
		count++
	}
	if count != 2 {
		return nil, fail
	}
	for uid := range usedSets {
		bindings = append(bindings, owned[uid])
	}
	sort.Slice(bindings, func(i, j int) bool {
		return text(bindings[i], "metadata", "uid") < text(bindings[j], "metadata", "uid")
	})
	return bindings, nil
}
