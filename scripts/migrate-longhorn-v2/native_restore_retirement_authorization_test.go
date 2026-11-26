package main

import (
	"context"
	"strings"
	"testing"
)

func TestNativeRestoreReclaimRefusesAReplayBeforeAccessingProduction(t *testing.T) {
	_, _, original := nativeRestoreStateFixture(t, "completed")
	for key, value := range nativeRestoreDispatch(original) {
		t.Setenv(key, value)
	}
	t.Setenv("GITHUB_RUN_ID", "6789")
	t.Setenv("GITHUB_RUN_ATTEMPT", "2")
	t.Setenv("LONGHORN_V2_OPERATION", nativeRestoreReclaimOperation)
	t.Setenv("LONGHORN_V2_CONFIRM", nativeRestoreReclaimConfirmation+original.Run)
	c := client{context: "admin@prod", run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("replayed reclaim reached an external command")
		return nil, nil
	}}
	if err := runNativeRestoreReclaim(context.Background(), c, nil); err == nil {
		t.Fatal("replayed reclaim was authorized")
	}
}

func TestNativeRestoreReclaimDispatchSharesProductionSerialization(t *testing.T) {
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	offered := 0
	for _, choice := range choices {
		if choice == nativeRestoreReclaimOperation {
			offered++
		}
	}
	job := valueObject(workflow, "jobs", "prepare")
	require := migrationDispatchStep(t, job, "Require confirmed first attempt on main")
	run := migrationDispatchStep(t, job, "Reclaim the verified disposable V2 restoration")
	ordinary := migrationDispatchStep(t, job, "Run the confirmed migration preparation")
	if offered != 1 || !strings.Contains(text(require, "run"), nativeRestoreReclaimOperation+")") ||
		!strings.Contains(text(require, "run"), nativeRestoreReclaimConfirmation+"[1-9][0-9]{0,18}$") ||
		text(run, "if") != "${{ inputs.operation == 'reclaim-native-v2-seed' }}" ||
		!strings.Contains(text(ordinary, "if"), "inputs.operation != 'reclaim-native-v2-seed'") ||
		!strings.Contains(text(workflow, "concurrency", "group"), "'prod-deploy'") {
		t.Fatal("reclaim lost its fixed-run confirmation, unique runner step or production lock")
	}
}

func TestNativeRestoreReclaimRequiresItsOwnFirstAttemptAndOriginalRun(t *testing.T) {
	for _, fault := range []string{"", "other operation", "replayed", "outside main", "node selected", "database selected", "no run", "leading zero", "other hostname", "self reclaim"} {
		t.Run(fault, func(t *testing.T) {
			_, _, original := nativeRestoreStateFixture(t, "completed")
			env := nativeRestoreDispatch(original)
			env["GITHUB_RUN_ID"] = "6789"
			env["LONGHORN_V2_OPERATION"] = "reclaim-native-v2-seed"
			env["LONGHORN_V2_CONFIRM"] = "reclaim-longhorn-native-v2-seed:" + original.Run
			switch fault {
			case "other operation":
				env["LONGHORN_V2_OPERATION"] = nativeRestoreOperation
			case "replayed":
				env["GITHUB_RUN_ATTEMPT"] = "2"
			case "outside main":
				env["GITHUB_REF"] = "refs/heads/another"
			case "node selected":
				env["LONGHORN_V2_NODE"] = "prod-worker-1"
			case "database selected":
				env["LONGHORN_V2_DATABASE"] = "portal-preview-db"
			case "no run":
				env["LONGHORN_V2_CONFIRM"] = "reclaim-longhorn-native-v2-seed"
			case "leading zero":
				env["LONGHORN_V2_CONFIRM"] = "reclaim-longhorn-native-v2-seed:0" + original.Run
			case "other hostname":
				env["RUNNER_ENVIRONMENT"] = "self-hosted"
			case "self reclaim":
				env["GITHUB_RUN_ID"] = original.Run
			}
			run, err := authorizedNativeRestoreReclaimRun(func(key string) string { return env[key] })
			if fault == "" && (err != nil || run != original.Run) || fault != "" && (err == nil || run != "") {
				t.Fatalf("restore reclaim authorization for %q: run=%q, err=%v", fault, run, err)
			}
		})
	}
}
