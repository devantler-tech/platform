package main

import (
	"reflect"
	"strings"
)

type nativeRestoreInitProof struct{ RestoreUID, ImageDigest string }

// The first consumer may have exactly Velero's completed restore-wait init
// process. The reattachment consumer has none and cannot initiate another
// restore. The caller must bind helperDigest to the live reviewed Velero server
// epoch; a caller-supplied string alone is not runtime provenance.
func verifyNativeRestoreConsumer(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, pod object, helperDigest string) error {
	if state.validate(record, seed) != nil || text(pod, "metadata", "resourceVersion") == "" ||
		text(pod, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != state.Run {
		return errNativeRestoreEvidence
	}
	rank := nativeRestorePhaseIndex(state.Phase)
	kind, node, script := "FirstPod", state.FirstNode, nativeRestoreReadScript
	var init *nativeRestoreInitProof
	switch {
	case rank >= nativeRestorePhaseIndex("restore-verified") && rank <= nativeRestorePhaseIndex("write-verified"):
		if node != state.Targets[0].Name || !seedSHA256.MatchString(helperDigest) ||
			text(pod, "metadata", "labels", "velero.io/restore-name") != state.Resources["Restore"].Name {
			return errNativeRestoreEvidence
		}
		init = &nativeRestoreInitProof{RestoreUID: state.Resources["Restore"].UID, ImageDigest: helperDigest}
	case rank >= nativeRestorePhaseIndex("reattach-owned"):
		kind, node = "SecondPod", text(pod, "spec", "nodeName")
		known := false
		for _, target := range state.Targets {
			known = known || target.Name == node
		}
		if !known || node == state.FirstNode || state.SecondNode != "" && state.SecondNode != node {
			return errNativeRestoreEvidence
		}
		var err error
		script, err = nativeRestoreVerifyScript(seed.PayloadSHA256, state.Token)
		if err != nil {
			return errNativeRestoreEvidence
		}
		script += nativeReattachWait
	default:
		return errNativeRestoreEvidence
	}
	identity := benchmarkIdentity(pod)
	normalized, err := nativeRestorePodWithoutUIDEnvironment(pod)
	if identity != state.Resources[kind] || !nativeRestoreChildIdentity(record, seed, state, kind, identity) ||
		err != nil || verifyNativeCanaryPod(normalized, identity.UID, node, script, init) != nil {
		return errNativeRestoreEvidence
	}
	return nil
}

func verifyNativeCanaryInit(pod object, proof *nativeRestoreInitProof) error {
	if proof == nil {
		if value(pod, "spec", "initContainers") != nil || value(pod, "status", "initContainerStatuses") != nil {
			return errSeedEvidence
		}
		return nil
	}
	containers, _ := value(pod, "spec", "initContainers").([]any)
	statuses, _ := value(pod, "status", "initContainerStatuses").([]any)
	main, _ := value(pod, "spec", "containers").([]any)
	if !nativeRestoreUID(proof.RestoreUID) || !seedSHA256.MatchString(proof.ImageDigest) || len(containers) != 1 || len(statuses) != 1 || len(main) != 1 {
		return errSeedEvidence
	}
	container, _ := containers[0].(object)
	status, _ := statuses[0].(object)
	primary, _ := main[0].(object)
	// The native helper is an ordinary init container; restartable init,
	// lifecycle and probe injections would change its execution and peak budget.
	if !runtimeOnlyKeys(container, "name", "image", "imagePullPolicy", "command", "args", "securityContext", "volumeMounts", "env", "resources", "terminationMessagePath", "terminationMessagePolicy") {
		return errSeedEvidence
	}
	for key, expected := range map[string]string{"imagePullPolicy": "IfNotPresent", "terminationMessagePath": "/dev/termination-log", "terminationMessagePolicy": "File"} {
		if actual, present := container[key]; present && actual != expected {
			return errSeedEvidence
		}
	}
	imageID := text(status, "imageID")
	imageMatches := imageID == "sha256:"+proof.ImageDigest || strings.HasSuffix(imageID, "@sha256:"+proof.ImageDigest)
	if text(container, "name") != "restore-wait" || text(container, "image") != "velero/velero:v1.18.4" ||
		!reflect.DeepEqual(value(container, "command"), []any{"/velero-restore-helper"}) || !reflect.DeepEqual(value(container, "args"), []any{proof.RestoreUID}) ||
		!nativeRestoreWaitEnvironment(value(container, "env")) || value(container, "envFrom") != nil || value(container, "volumeDevices") != nil ||
		!reflect.DeepEqual(value(container, "securityContext"), value(primary, "securityContext")) ||
		text(status, "name") != "restore-wait" || text(status, "containerID") == "" || value(status, "restartCount") != float64(0) ||
		!imageMatches || value(status, "state", "terminated", "exitCode") != float64(0) {
		return errSeedEvidence
	}
	resources, resourceShape := value(container, "resources").(object)
	profile := object{"cpu": "100m", "memory": "128Mi"}
	if !resourceShape || !runtimeOnlyKeys(resources, "requests", "limits") ||
		!nativeRestoreV2ResourceEqual(resources["requests"], profile) || !nativeRestoreV2ResourceEqual(resources["limits"], profile) ||
		!nativeRestoreWaitAllocation(status, profile) {
		return errSeedEvidence
	}
	mounts, _ := value(container, "volumeMounts").([]any)
	if len(mounts) != 1 {
		return errSeedEvidence
	}
	mount, _ := mounts[0].(object)
	if text(mount, "name") != "data" || text(mount, "mountPath") != "/restores/data" || value(mount, "readOnly") == true ||
		!absentOrEmptyString(value(mount, "subPath")) || !absentOrEmptyString(value(mount, "subPathExpr")) ||
		text(mount, "mountPropagation") != "" && text(mount, "mountPropagation") != "None" {
		return errSeedEvidence
	}
	return nil
}

// The pinned Velero helper uses these two identity fields. This grants no
// environment access to ordinary consumers, credentials or other Pod fields.
func nativeRestoreWaitEnvironment(raw any) bool {
	entries, ok := raw.([]any)
	if !ok || len(entries) != 2 {
		return false
	}
	expected := map[string]string{"POD_NAMESPACE": "metadata.namespace", "POD_NAME": "metadata.name"}
	seen := map[string]bool{}
	for _, item := range entries {
		entry, valid := item.(object)
		name := text(entry, "name")
		if !valid || !runtimeOnlyKeys(entry, "name", "valueFrom") || expected[name] == "" || seen[name] {
			return false
		}
		from, valid := value(entry, "valueFrom").(object)
		if !valid || len(from) != 1 {
			return false
		}
		field, valid := from["fieldRef"].(object)
		if !valid || !runtimeOnlyKeys(field, "apiVersion", "fieldPath") || text(field, "fieldPath") != expected[name] {
			return false
		}
		if version, present := field["apiVersion"]; present && version != "v1" {
			return false
		}
		seen[name] = true
	}
	return true
}

func nativeRestoreWaitAllocation(status, profile object) bool {
	if raw, present := status["allocatedResources"]; present {
		resources, valid := raw.(object)
		if !valid || resources == nil || !nativeRestoreV2ResourceBound(resources, profile) {
			return false
		}
	}
	if raw, present := status["resources"]; present {
		resources, valid := raw.(object)
		if !valid || resources == nil || !runtimeOnlyKeys(resources, "requests", "limits") {
			return false
		}
		for kind, raw := range resources {
			values, valid := raw.(object)
			if !valid || values == nil || !nativeRestoreV2ResourceBound(values, profile) ||
				(kind == "limits" && !nativeRestoreV2ObservedLimits(values, profile)) {
				return false
			}
		}
	}
	return true
}
