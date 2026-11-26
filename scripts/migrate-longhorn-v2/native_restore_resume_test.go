package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestNativeRestoreResumeBindsNewExecutionToOriginalIdentity(t *testing.T) {
	_, _, state := nativeRestoreStateFixture(t, "restore-owned")
	for _, tc := range []struct {
		name, key, value string
		allow            bool
	}{
		{name: "confirmed", allow: true},
		{name: "foreign original", key: "LONGHORN_V2_CONFIRM", value: "resume-longhorn-native-v2-seed:999"},
		{name: "same execution", key: "GITHUB_RUN_ID", value: state.Run},
		{name: "same source", key: "GITHUB_SHA", value: state.Source},
		{name: "repeat attempt", key: "GITHUB_RUN_ATTEMPT", value: "2"},
		{name: "selected worker", key: "LONGHORN_V2_NODE", value: "prod-worker-1"},
		{name: "selected database", key: "LONGHORN_V2_DATABASE", value: "umami-db"},
		{name: "branch", key: "GITHUB_REF", value: "refs/heads/other"},
		{name: "oversized executor", key: "GITHUB_RUN_ID", value: "9999999999999999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := nativeRestoreDispatch(state)
			env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = "resume-native-v2-seed-restore", "resume-longhorn-native-v2-seed:"+state.Run
			env["GITHUB_RUN_ID"], env["GITHUB_SHA"] = "9876", strings.Repeat("a", 40)
			env[tc.key] = tc.value
			source, marker, err := nativeRestoreGrant(func(k string) string { return env[k] }, state)
			if (err == nil) != tc.allow {
				t.Fatalf("unexpected resume authorization: %v", err)
			}
			if tc.allow && (source != env["GITHUB_SHA"] || marker != source+":"+env["GITHUB_RUN_ID"]) {
				t.Fatal("resume authorization did not bind the replacement execution")
			}
		})
	}
}

func TestNativeRestoreResumeRequiresItsOwnWorkflowConfirmation(t *testing.T) {
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	gate := migrationDispatchStep(t, valueObject(workflow, "jobs", "prepare"), "Require confirmed first attempt on main")
	want := nativeRestoreResumeOperation +
		`) [[ -z "${LONGHORN_V2_NODE}" && -z "${LONGHORN_V2_DATABASE}" && "${LONGHORN_V2_CONFIRM}" =~ ^` +
		nativeRestoreResumeConfirmation + `[1-9][0-9]{0,18}$ ]] ;;`
	if !slices.Contains(choices, nativeRestoreResumeOperation) || !migrationDispatchMustRun(gate) ||
		!strings.Contains(strings.Join(strings.Fields(text(gate, "run")), " "), want) {
		t.Fatal("resume requires its explicit first-attempt main workflow and original run ID")
	}
}

func TestNativeRestoreResumeWrongGrantNeverReadsTheCluster(t *testing.T) {
	_, _, state := nativeRestoreStateFixture(t, "restore-owned")
	env := nativeRestoreDispatch(state)
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = nativeRestoreResumeOperation, nativeRestoreResumeConfirmation+"malformed"
	for key, value := range env {
		t.Setenv(key, value)
	}
	c := client{context: "admin@prod", run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("an unconfirmed resume reached an external command")
		return nil, nil
	}}
	if err := resumeNativeRestore(t.Context(), c, poolConfigFixture()); err == nil {
		t.Fatal("an unconfirmed resume was admitted")
	}
}

func TestNativeRestoreResumeRequiresTheLatestFirstAttemptToHaveFailed(t *testing.T) {
	_, _, state := nativeRestoreStateFixture(t, "restore-owned")
	for _, tc := range []struct {
		name, key string
		value     any
		allow     bool
	}{
		{name: "failed dispatch", allow: true},
		{name: "live", key: "status", value: "in_progress"},
		{name: "passed", key: "conclusion", value: "success"},
		{name: "canceled", key: "conclusion", value: "cancelled"},
		{name: "rerun", key: "run_attempt", value: 2},
		{name: "wrong run", key: "id", value: 5679},
		{name: "wrong source", key: "head_sha", value: strings.Repeat("f", 40)},
		{name: "wrong workflow", key: "path", value: ".github/workflows/ci.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client{run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || program != "gh" || strings.Join(args, " ") != "api repos/devantler-tech/platform/actions/runs/"+state.Run {
					t.Fatal("resume checked an endpoint other than the latest original attempt")
				}
				run := object{"id": 5678, "status": "completed", "conclusion": "failure", "head_sha": state.Source,
					"head_branch": "main", "event": "workflow_dispatch", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"}
				if tc.key != "" {
					run[tc.key] = tc.value
				}
				return json.Marshal(run)
			}}
			if err := verifyNativeRestoreResumeOwner(t.Context(), c, state); (err == nil) != tc.allow {
				t.Fatalf("unexpected original workflow verdict: %v", err)
			}
		})
	}
}

func TestNativeRestoreClaimResumesOnlyTheOriginalJournalOnce(t *testing.T) {
	for _, tc := range []string{"confirmed", "marker exists", "replacement", "ambiguous patch"} {
		t.Run(tc, func(t *testing.T) {
			record, seed, state := nativeRestoreStateFixture(t, "restore-owned")
			const activationBody = "original activation record"
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(activationBody)))
			for i := range state.Targets {
				state.Targets[i].Preparation.ActivationSHA256 = digest
			}
			source := strings.Repeat("a", 40)
			executor := source + ":9876"
			env := nativeRestoreDispatch(state)
			env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = nativeRestoreResumeOperation, nativeRestoreResumeConfirmation+state.Run
			env["GITHUB_SHA"], env["GITHUB_RUN_ID"] = source, "9876"
			for key, value := range env {
				t.Setenv(key, value)
			}
			journal := func(id benchmarkResource, data object) object {
				return object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{
					"name": id.Name, "namespace": id.Namespace, "uid": id.UID, "resourceVersion": "17",
				}, "data": data}
			}
			encode := func(obj any) string {
				raw, err := json.Marshal(obj)
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			original := benchmarkResource{Name: nativeRestoreJournal, Namespace: "longhorn-system", UID: "restore-journal-uid"}
			restore := journal(original, object{"restore": encode(state)})
			if tc == "marker exists" {
				valueObject(restore, "metadata")["annotations"] = object{nativeRestoreExecutorAnnotation: ""}
			}
			if tc == "replacement" {
				valueObject(restore, "metadata")["uid"] = "replacement-journal"
			}
			seedJournal := journal(state.SeedJournal, object{"record": encode(record), "seed": encode(seed)})
			activation := journal(state.Targets[0].Preparation.Activation, object{"record": activationBody})
			patches := 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				switch {
				case program == "gh" && strings.Join(args, " ") == "api repos/devantler-tech/platform/actions/runs/"+state.Run:
					return json.Marshal(object{"id": 5678, "status": "completed", "conclusion": "failure", "head_branch": "main",
						"head_sha": state.Source, "event": "workflow_dispatch", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"})
				case program == "git" && strings.Join(args, " ") == "ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main":
					return []byte(source + "\trefs/heads/main\n"), nil
				case program == "kubectl" && len(args) >= 7 && args[0] == "--context=admin@prod" && args[3] == "configmap":
					if args[2] == "get" && input == nil {
						switch args[4] {
						case nativeRestoreJournal:
							return json.Marshal(restore)
						case nativeSeedJournal:
							return json.Marshal(seedJournal)
						case temporaryActivationJournal:
							return json.Marshal(activation)
						}
					}
					if args[2] == "patch" && args[4] == nativeRestoreJournal {
						want := []object{
							{"op": "test", "path": "/metadata/uid", "value": original.UID},
							{"op": "test", "path": "/metadata/resourceVersion", "value": "17"},
							{"op": "add", "path": "/metadata/annotations", "value": object{}},
							{"op": "add", "path": "/metadata/annotations/storage.platform.devantler.tech~1native-restore-executor", "value": executor},
						}
						var patch []object
						if json.Unmarshal(input, &patch) != nil || !reflect.DeepEqual(patch, want) {
							t.Fatal("resume patched something other than the original journal's executor")
						}
						patches++
						valueObject(restore, "metadata")["annotations"] = object{nativeRestoreExecutorAnnotation: executor}
						if tc == "ambiguous patch" {
							return nil, errors.New("response lost")
						}
						return []byte("configmap/" + nativeRestoreJournal), nil
					}
				}
				t.Fatalf("unexpected command %s %v", program, args)
				return nil, nil
			}}
			r := nativeRestoreRun{c: c, source: source, state: state, journalUID: original.UID,
				plan: nativeRestoreAdmission{Seed: nativeRestoreSeedReceipt{Journal: state.SeedJournal, Record: record, Seed: seed}}}
			err := r.claimResume(t.Context(), executor)
			wantPatches := 0
			if tc == "confirmed" || tc == "ambiguous patch" {
				wantPatches = 1
			}
			if (err == nil) != (tc == "confirmed") || patches != wantPatches || text(restore, "data", "restore") != encode(state) {
				t.Fatalf("unexpected resume outcome: patches=%d err=%v", patches, err)
			}
			if err := r.claimResume(t.Context(), executor); err == nil || patches != wantPatches {
				t.Fatal("resume reclaimed an existing or uncertain write")
			}
		})
	}
}

func TestTemporaryRegistrationReadAllowsOnlyTheOriginalCanary(t *testing.T) {
	id := benchmarkResource{Name: "pvc-canary", Namespace: "longhorn-system", UID: "canary-uid"}
	canary := object{"metadata": object{"name": id.Name, "namespace": id.Namespace, "uid": id.UID},
		"spec": object{"dataEngine": "v2"}}
	v1 := object{"metadata": object{"name": "pvc-v1", "namespace": "longhorn-system", "uid": "v1-uid"},
		"spec": object{"dataEngine": "v1"}}
	other := object{"metadata": object{"name": id.Name, "namespace": id.Namespace, "uid": "replacement"},
		"spec": object{"dataEngine": "v2"}}
	for _, tc := range []struct {
		name    string
		volumes []object
		want    benchmarkResource
		allow   bool
	}{
		{"pre-activation", []object{v1}, benchmarkResource{}, true},
		{"resume", []object{v1, canary}, id, true},
		{"unrelated V2 during activation", []object{v1, canary}, benchmarkResource{}, false},
		{"replacement canary", []object{v1, other}, id, false},
		{"missing canary", []object{v1}, id, false},
		{"duplicated canary", []object{v1, canary, canary}, id, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyTemporaryRegistrationVolumes(tc.volumes, tc.want); (err == nil) != tc.allow {
				t.Fatalf("unexpected inventory verdict: %v", err)
			}
		})
	}
}
