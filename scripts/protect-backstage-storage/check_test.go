package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"
)

// checkStorage exercises the read-only entrypoint using the normal protected caller facts.
func checkStorage(t *testing.T, a *storageAPI, env map[string]string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), []string{"--check"}, func(k string) string { return env[k] }, a.execute, a.wait, &out)
	return out.String(), err
}

// TestStorageCheckRequiresEveryProtectedCallerFact denies unbound read-only invocations before tools.
func TestStorageCheckRequiresEveryProtectedCallerFact(t *testing.T) {
	for _, key := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_WORKFLOW_REF", "GITHUB_RUN_ATTEMPT", "GITHUB_SHA", "GITHUB_RUN_ID", "PORTAL_STORAGE_OPERATION", "PORTAL_STORAGE_CONFIRM", "PORTAL_STORAGE_REVISION", "PORTAL_STORAGE_DEPLOYMENT_RUN"} {
		t.Run(key, func(t *testing.T) {
			a := storageFixture(t)
			env := testEnvironment()
			env["PORTAL_STORAGE_OPERATION"] = "check"
			env["PORTAL_STORAGE_CONFIRM"] = "CHECK BACKSTAGE STORAGE ONLY"
			env[key] = "wrong"
			if _, err := checkStorage(t, a, env); err == nil || len(a.commands) != 0 {
				t.Fatal("unbound check reached an external tool")
			}
		})
	}
}

// TestStorageCheckNeverChangesDeleteOrRetainVolumes verifies the real command boundary and its bounded receipt.
func TestStorageCheckNeverChangesDeleteOrRetainVolumes(t *testing.T) {
	for _, policy := range []string{"Delete", "Retain"} {
		t.Run(policy, func(t *testing.T) {
			a := storageFixture(t)
			for i := range 2 {
				mchild(mitem(a.volumes, i), "spec")["persistentVolumeReclaimPolicy"] = policy
			}
			before := mustJSON(t, a.volumes)
			env := testEnvironment()
			env["PORTAL_STORAGE_OPERATION"] = "check"
			env["PORTAL_STORAGE_CONFIRM"] = "CHECK BACKSTAGE STORAGE ONLY"
			out, err := checkStorage(t, a, env)
			if err != nil {
				t.Fatal(err)
			}
			wantChanges, retained := 2, 0
			if policy == "Retain" {
				wantChanges, retained = 0, 2
			}
			want := map[string]any{"result": "CHECKED", "claims": float64(2), "changed": float64(0), "wouldChange": float64(wantChanges), "alreadyRetained": float64(retained), "separatedSamples": true, "retirement": "NOT_EXECUTED"}
			var got map[string]any
			if json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, want) || a.patches != 0 || a.pauses != 1 || a.collections != 2 || !bytes.Equal(before, mustJSON(t, a.volumes)) {
				t.Fatalf("check mutated, omitted separated evidence or emitted an incorrect receipt: patches=%d output=%s", a.patches, out)
			}
		})
	}
}

// TestStorageCheckRejectsUnsafeEvidenceWithoutWriting preserves the production failure boundaries.
func TestStorageCheckRejectsUnsafeEvidenceWithoutWriting(t *testing.T) {
	for _, failure := range []string{"invocation", "source", "provenance", "binding", "drift", "output"} {
		t.Run(failure, func(t *testing.T) {
			a := storageFixture(t)
			env := testEnvironment()
			env["PORTAL_STORAGE_OPERATION"] = "check"
			env["PORTAL_STORAGE_CONFIRM"] = "CHECK BACKSTAGE STORAGE ONLY"
			var out bytes.Buffer
			var writer io.Writer = &out
			switch failure {
			case "invocation":
				env["PORTAL_STORAGE_CONFIRM"] = "PROTECT BACKSTAGE STORAGE ONLY"
			case "source":
				a.staleSource = true
			case "provenance":
				a.provenanceFailure = true
			case "binding":
				mchild(mchild(mitem(a.volumes, 0), "spec"), "claimRef")["uid"] = "foreign"
			case "drift":
				a.onRead = func(a *storageAPI, s string) {
					if a.pauses > 0 {
						mchild(a.db, "spec")["instances"] = float64(3)
					}
				}
			case "output":
				writer = failedResultWriter{short: true}
			}
			if err := run(context.Background(), []string{"--check"}, func(k string) string { return env[k] }, a.execute, a.wait, writer); err == nil || a.patches != 0 {
				t.Fatal("unsafe check succeeded or attempted a write")
			}
		})
	}
}

// TestStorageOperationCannotCrossItsConfirmation blocks mode confusion before any external read.
func TestStorageOperationCannotCrossItsConfirmation(t *testing.T) {
	for _, mode := range []string{"check", "protect", "", "CHECK", "check;protect"} {
		t.Run(mode, func(t *testing.T) {
			a := storageFixture(t)
			env := testEnvironment()
			env["PORTAL_STORAGE_OPERATION"] = mode
			// The execute flag is always invalid with the check confirmation,
			// even if someone binds the operation to protect.
			env["PORTAL_STORAGE_CONFIRM"] = "CHECK BACKSTAGE STORAGE ONLY"
			if _, err := executeStorage(t, a, env); err == nil || len(a.commands) != 0 {
				t.Fatal("cross-mode invocation reached a tool")
			}
		})
	}
}
