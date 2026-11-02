package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func dispatchEnv() map[string]string {
	return map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "123", "GITHUB_SHA": strings.Repeat("a", 40),
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/prove-kyverno-cache-recovery.yaml@refs/heads/main", "KYVERNO_CACHE_CONFIRM": "prove-kyverno-cache-recovery", "KYVERNO_CACHE_CREDENTIAL_DIR": "/dev/shm/kyverno-cache-recovery.abc123", "KUBECONFIG": "/dev/shm/kyverno-cache-recovery.abc123/kubeconfig",
	}
}

func TestOnlyConfirmedFirstMainDispatchCanReachTheCredential(t *testing.T) {
	env := dispatchEnv()
	meta, err := authorize(func(k string) string { return env[k] })
	if err != nil || meta.runID != "123" {
		t.Fatalf("authorized dispatch rejected: %v", err)
	}
	for _, key := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_REF", "GITHUB_EVENT_NAME", "GITHUB_RUN_ATTEMPT", "GITHUB_RUN_ID", "GITHUB_SHA", "GITHUB_WORKFLOW_REF", "KYVERNO_CACHE_CONFIRM", "KYVERNO_CACHE_CREDENTIAL_DIR", "KUBECONFIG"} {
		t.Run(key, func(t *testing.T) {
			env := dispatchEnv()
			env[key] = "foreign"
			if _, err := authorize(func(k string) string { return env[k] }); err == nil {
				t.Fatal("unqualified dispatch accepted")
			}
		})
	}
}

func TestControlPodHasNoHostOrCredentialAccess(t *testing.T) {
	raw, name, err := controlPod("123", "unsigned")
	if err != nil {
		t.Fatal(err)
	}
	var pod object
	if err = json.Unmarshal(raw, &pod); err != nil {
		t.Fatal(err)
	}
	if name != "longhorn-cache-unsigned-123" || text(pod, "metadata", "namespace") != "default" || text(pod, "apiVersion") != "v1" || text(pod, "kind") != "Pod" || at(pod, "spec", "automountServiceAccountToken") != false || text(pod, "spec", "restartPolicy") != "Never" || at(pod, "spec", "hostNetwork") == true || at(pod, "spec", "hostPID") == true || at(pod, "spec", "hostIPC") == true || at(pod, "spec", "volumes") != nil || at(pod, "metadata", "ownerReferences") != nil {
		t.Fatal("control can persist host or credential access")
	}
	containers, _ := at(pod, "spec", "containers").([]any)
	if len(containers) != 1 || text(containers[0], "image") != unsignedImage || text(containers[0], "name") != "probe" || at(containers[0], "env") != nil || at(containers[0], "volumeMounts") != nil || at(containers[0], "securityContext", "allowPrivilegeEscalation") != false || at(containers[0], "securityContext", "readOnlyRootFilesystem") != true || at(containers[0], "securityContext", "runAsNonRoot") != true || text(pod, "spec", "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
		t.Fatal("control is not a restricted single-image Pod")
	}
	if _, _, err := controlPod("1;foreign", "unsigned"); err == nil {
		t.Fatal("untrusted name accepted")
	}
	if _, _, err := controlPod("123", "foreign"); err == nil {
		t.Fatal("unknown control accepted")
	}
}
