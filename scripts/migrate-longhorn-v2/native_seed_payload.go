package main

import (
	"reflect"
	"strings"
)

func verifyNativeSeedPod(pod object, uid string) error {
	return verifyNativeCanaryPod(pod, uid, benchmarkClientNode, nativeSeedScript, nil)
}

func verifyNativeCanaryPod(pod object, uid, node, script string, restore *nativeRestoreInitProof) error {
	statuses, _ := value(pod, "status", "containerStatuses").([]any)
	if err := verifyNativeCanaryPodSpec(pod, uid, script, restore); err != nil ||
		text(pod, "spec", "nodeName") != node || text(pod, "status", "phase") != "Running" ||
		!ready(valueObject(pod, "status")) || len(statuses) != 1 {
		return errSeedEvidence
	}
	actual, _ := statuses[0].(object)
	if text(actual, "name") != "fio" || value(actual, "ready") != true || value(actual, "restartCount") != float64(0) ||
		value(actual, "state", "running") == nil || !strings.HasSuffix(text(actual, "imageID"), strings.Split(fioImage, "@")[1]) {
		return errSeedEvidence
	}
	return nil
}

func verifyNativeCanaryPodSpec(pod object, uid, script string, restore *nativeRestoreInitProof) error {
	containers, _ := value(pod, "spec", "containers").([]any)
	if text(pod, "apiVersion") != "v1" || text(pod, "kind") != "Pod" || uid == "" || text(pod, "metadata", "uid") != uid ||
		text(pod, "metadata", "name") != "fio" || text(pod, "metadata", "namespace") != benchmarkNamespace || value(pod, "metadata", "deletionTimestamp") != nil ||
		value(pod, "spec", "automountServiceAccountToken") != false || value(pod, "spec", "hostUsers") != false || value(pod, "spec", "activeDeadlineSeconds") != float64(2700) ||
		value(pod, "spec", "hostNetwork") == true || value(pod, "spec", "hostPID") == true || value(pod, "spec", "hostIPC") == true ||
		len(containers) != 1 || verifyNativeCanaryInit(pod, restore) != nil || value(pod, "spec", "ephemeralContainers") != nil {
		return errSeedEvidence
	}
	container, _ := containers[0].(object)
	if text(container, "name") != "fio" || text(container, "image") != fioImage || !reflect.DeepEqual(value(container, "command"), []any{"/bin/sh", "-ec"}) ||
		!reflect.DeepEqual(value(container, "args"), []any{script}) ||
		value(container, "env") != nil || value(container, "envFrom") != nil ||
		value(container, "securityContext", "runAsNonRoot") != true || value(container, "securityContext", "runAsUser") != float64(65532) ||
		value(container, "securityContext", "allowPrivilegeEscalation") != false || value(container, "securityContext", "readOnlyRootFilesystem") != true ||
		value(container, "securityContext", "privileged") == true || !reflect.DeepEqual(value(container, "securityContext", "capabilities", "drop"), []any{"ALL"}) ||
		text(container, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
		return errSeedEvidence
	}
	volumes, _ := value(pod, "spec", "volumes").([]any)
	mounts, _ := value(container, "volumeMounts").([]any)
	if len(volumes) != 2 || len(mounts) != 2 {
		return errSeedEvidence
	}
	seenVolumes, seenMounts := map[string]bool{}, map[string]bool{}
	for _, raw := range volumes {
		volume, ok := raw.(object)
		name := text(volume, "name")
		if !ok || len(volume) != 2 || seenVolumes[name] {
			return errSeedEvidence
		}
		seenVolumes[name] = true
		switch name {
		case "data":
			if text(volume, "persistentVolumeClaim", "claimName") != "fio-data" || value(volume, "persistentVolumeClaim", "readOnly") == true {
				return errSeedEvidence
			}
		case "scratch":
			if text(volume, "emptyDir", "sizeLimit") != "32Mi" || text(volume, "emptyDir", "medium") != "" {
				return errSeedEvidence
			}
		default:
			return errSeedEvidence
		}
	}
	for _, raw := range mounts {
		mount, ok := raw.(object)
		name := text(mount, "name")
		path := map[string]string{"data": "/data", "scratch": "/tmp"}[name]
		if !ok || path == "" || text(mount, "mountPath") != path || seenMounts[name] || value(mount, "readOnly") == true ||
			text(mount, "subPath") != "" || text(mount, "subPathExpr") != "" || text(mount, "mountPropagation") != "" && text(mount, "mountPropagation") != "None" {
			return errSeedEvidence
		}
		seenMounts[name] = true
	}
	return nil
}

func decodeSeedPayload(output string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 || !seedSHA256.MatchString(fields[0]) || fields[1] != "payload.bin" {
		return "", errSeedEvidence
	}
	return fields[0], nil
}
