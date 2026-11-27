package main

import (
	"strings"
	"testing"
)

func TestActualBudgetRestoreRequiresASeparateConfirmedBackup(t *testing.T) {
	const backup = "1001"
	source, digest := strings.Repeat("a", 40), strings.Repeat("c", 64)
	for _, fault := range []string{"", "retry", "source", "target", "database", "backup"} {
		t.Run(fault, func(t *testing.T) {
			env := dispatchFixture()
			env["GITHUB_RUN_ID"] = "1002"
			env["LONGHORN_V2_NODE"] = ""
			env["LONGHORN_V2_OPERATION"] = actualBudgetRestoreOperation
			env["LONGHORN_V2_CONFIRM"] = actualBudgetRestoreOperation + ":" + backup + ":" + source + ":" + digest
			switch fault {
			case "retry":
				env["GITHUB_RUN_ATTEMPT"] = "2"
			case "source":
				env["GITHUB_REF"] = "refs/heads/staging"
			case "target":
				env["LONGHORN_V2_NODE"] = "prod-worker-1"
			case "database":
				env["LONGHORN_V2_DATABASE"] = "umami-db"
			case "backup":
				env["GITHUB_RUN_ID"] = backup
			}
			got, err := authorizeActualBudgetRestore(func(key string) string { return env[key] })
			if fault == "" && (err != nil || got != (actualBudgetRestoreConfirmation{backup, source, digest})) || fault != "" && err == nil {
				t.Fatalf("unexpected restore authorization %v", err)
			}
		})
	}
}

func TestActualBudgetRestoreContentReaderUsesOnlyItsReadOnlyClaim(t *testing.T) {
	const namespace = "actual-budget-recovery-1234"
	for _, fault := range []string{"", "other reader", "other image", "mutating mount", "attached credentials", "added sidecar"} {
		t.Run(fault, func(t *testing.T) {
			desired := actualBudgetProofPod(namespace, "1234")
			pod := actualBudgetProofPod(namespace, "1234")
			valueObject(pod, "metadata")["uid"] = "7db77aa9-0fd6-4343-a799-75e7f0117380"
			valueObject(pod, "metadata")["resourceVersion"] = "12345"
			_, digest, _ := strings.Cut(actualBudgetProofImage, "@")
			status := object{"name": "verify", "ready": true, "restartCount": float64(0),
				"imageID": "docker.io/actualbudget/actual-server@" + digest}
			pod["status"] = object{"phase": "Running", "containerStatuses": []any{status},
				"conditions": []any{object{"type": "Ready", "status": "True"}}}
			initial := object{"metadata": object{"uid": text(pod, "metadata", "uid"), "name": "verify-restored-budget", "namespace": namespace}}
			spec := valueObject(pod, "spec")
			containers := value(spec, "containers").([]any)
			container := containers[0].(object)
			switch fault {
			case "other reader":
				container["command"] = []any{"node", "server.js"}
			case "other image":
				status["imageID"] = "docker.io/actualbudget/actual-server@sha256:" + strings.Repeat("f", 64)
			case "mutating mount":
				value(container, "volumeMounts").([]any)[0].(object)["readOnly"] = false
			case "attached credentials":
				spec["automountServiceAccountToken"] = true
			case "added sidecar":
				spec["containers"] = append(containers, object{"name": "application"})
			}
			if valid := actualBudgetProbeReady(pod, initial, desired); valid != (fault == "") {
				t.Fatalf("unexpected content verifier verdict: %t", valid)
			}
		})
	}
}
