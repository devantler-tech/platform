package main

import (
	"encoding/json"
	"errors"
	"regexp"
)

type runMetadata struct{ source, runID, credentialDir string }

var sourcePattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var runPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var credentialPattern = regexp.MustCompile(`^/dev/shm/kyverno-cache-recovery\.[A-Za-z0-9]+$`)

func authorize(get func(string) string) (runMetadata, error) {
	meta := runMetadata{source: get("GITHUB_SHA"), runID: get("GITHUB_RUN_ID"), credentialDir: get("KYVERNO_CACHE_CREDENTIAL_DIR")}
	if get("GITHUB_ACTIONS") != "true" || get("GITHUB_REPOSITORY") != "devantler-tech/platform" || get("GITHUB_REF") != "refs/heads/main" || get("GITHUB_EVENT_NAME") != "workflow_dispatch" || get("GITHUB_RUN_ATTEMPT") != "1" || get("GITHUB_WORKFLOW_REF") != "devantler-tech/platform/.github/workflows/prove-kyverno-cache-recovery.yaml@refs/heads/main" || get("KYVERNO_CACHE_CONFIRM") != "prove-kyverno-cache-recovery" || !sourcePattern.MatchString(meta.source) || !runPattern.MatchString(meta.runID) || !credentialPattern.MatchString(meta.credentialDir) || get("KUBECONFIG") != meta.credentialDir+"/kubeconfig" {
		return runMetadata{}, errors.New("protected admission recovery dispatch is unverified")
	}
	return meta, nil
}

func controlPod(runID, phase string) ([]byte, string, error) {
	if !runPattern.MatchString(runID) || (phase != "signed-before" && phase != "unsigned" && phase != "signed-after") {
		return nil, "", errors.New("admission control identity is unqualified")
	}
	image := signedImage
	if phase == "unsigned" {
		image = unsignedImage
	}
	name := "longhorn-cache-" + phase + "-" + runID
	pod := object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"name": name, "namespace": "default", "labels": object{"app.kubernetes.io/name": "longhorn-cache-admission-probe"}}, "spec": object{
		"automountServiceAccountToken": false, "restartPolicy": "Never", "hostNetwork": false, "hostPID": false, "hostIPC": false, "hostUsers": false,
		"securityContext": object{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "seccompProfile": object{"type": "RuntimeDefault"}},
		"containers":      []any{object{"name": "probe", "image": image, "resources": object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"cpu": "100m", "memory": "128Mi"}}, "securityContext": object{"runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}}}}},
	}}
	raw, err := json.Marshal(pod)
	if err != nil {
		return nil, "", errors.New("fixed admission control could not be encoded")
	}
	return raw, name, nil
}
