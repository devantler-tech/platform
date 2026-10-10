package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestPauseDiagnosticDispatchCannotReuseRecoveryApproval binds an independent,
// first-attempt diagnostic to reviewed main, not a consumed repair confirmation.
func TestPauseDiagnosticDispatchCannotReuseRecoveryApproval(t *testing.T) {
	good := map[string]string{
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/diagnose-wedding-pause.yaml@refs/heads/main",
		"GITHUB_REPOSITORY":   "devantler-tech/platform", "GITHUB_REF": "refs/heads/main",
		"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1",
		"GITHUB_SHA":                       strings.Repeat("a", 40),
		"WEDDING_PAUSE_DIAGNOSTIC_CONFIRM": "dry-run-retained-wedding-pause",
	}
	if !pauseDiagnosticDispatchAllowed(func(k string) string { return good[k] }) {
		t.Fatal("independent diagnostic dispatch was refused")
	}
	for key, value := range map[string]string{
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/recover-retained-wedding-standby.yaml@refs/heads/main",
		"GITHUB_REPOSITORY":   "other/platform", "GITHUB_REF": "refs/heads/other",
		"GITHUB_EVENT_NAME": "pull_request", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_SHA": strings.Repeat("z", 40),
		"WEDDING_PAUSE_DIAGNOSTIC_CONFIRM": "retain-volume-rebuild-completed-standby",
		"WEDDING_REPAIR_CONFIRM":           "retain-volume-rebuild-completed-standby",
		"WEDDING_REPAIR_RESUME_FENCED":     "true",
	} {
		t.Run(key, func(t *testing.T) {
			for _, replacement := range []string{value, ""} {
				if (key == "WEDDING_REPAIR_CONFIRM" || key == "WEDDING_REPAIR_RESUME_FENCED") && replacement == "" {
					continue
				}
				if pauseDiagnosticDispatchAllowed(func(k string) string {
					if k == key {
						return replacement
					}
					return good[k]
				}) {
					t.Fatal("diagnostic accepted an unbound dispatch")
				}
			}
		})
	}
}

// TestPauseDiagnosticRejectsMixedCLIFlags stops unsafe combinations before any
// observation, even when all five object identities are syntactically valid.
func TestPauseDiagnosticRejectsMixedCLIFlags(t *testing.T) {
	for _, extra := range []string{"--execute", "--prove-fenced", "--resume-fenced", "local caller", "without quarantine"} {
		t.Run(extra, func(t *testing.T) {
			for _, key := range []string{"GITHUB_WORKFLOW_REF", "GITHUB_REPOSITORY", "GITHUB_REF", "GITHUB_EVENT_NAME", "GITHUB_RUN_ATTEMPT", "GITHUB_SHA", "WEDDING_PAUSE_DIAGNOSTIC_CONFIRM"} {
				t.Setenv(key, "")
			}
			args := []string{"diagnose", "--diagnose-pause", "--quarantine-completed-join", "--cluster-uid", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "--pod-uid", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "--job-uid", "cccccccc-cccc-cccc-cccc-cccccccccccc", "--claim-uid", "dddddddd-dddd-dddd-dddd-dddddddddddd", "--volume-uid", "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"}
			if extra == "without quarantine" {
				args = append(args[:2], args[3:]...)
			} else if extra != "local caller" {
				args = append(args, extra)
			}
			previousFlags, previousArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine = previousFlags; os.Args = previousArgs })
			flag.CommandLine = flag.NewFlagSet("diagnose", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = args
			err := run()
			if err == nil || (!strings.Contains(err.Error(), "diagnostic cannot share") && !strings.Contains(err.Error(), "first protected main diagnostic")) {
				t.Fatalf("diagnostic boundary returned %v", err)
			}
		})
	}
}

// TestPauseDiagnosticStopsAfterOneNonPersistingPatch exercises the full guarded
// path, not just a command builder. Any command after its sole patch is a failure.
func TestPauseDiagnosticStopsAfterOneNonPersistingPatch(t *testing.T) {
	for _, failure := range []string{"", "rejected patch", "source", "unready peer", "mixed execution"} {
		t.Run(failure, func(t *testing.T) {
			s, guards := completedFixture()
			lease, leaderPod := leaderFixture()
			backing := object{"metadata": object{"name": "original-volume", "namespace": "longhorn-system", "uid": "backing-uid", "resourceVersion": "10"}, "status": object{"state": "detached"}}
			if failure == "unready peer" {
				list(s.pods[2], "status", "conditions")[0]["status"] = "False"
			}
			before, snapshotErr := json.Marshal([]any{s.cluster, s.operator, s.pods, s.claims, s.backups, s.jobs, s.volumes})
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			var recorded []json.RawMessage
			if err := json.Unmarshal(before, &recorded); err != nil || len(recorded) != 7 {
				t.Fatal("unchanged-inventory evidence does not cover all seven resource families")
			}
			patches, sourceReads := 0, 0
			c := client{now: func() time.Time { return testNow }, source: func(context.Context) error {
				if patches != 0 {
					t.Fatal("source command continued after diagnostic")
				}
				sourceReads++
				if failure == "source" {
					return errors.New("fixture source changed")
				}
				return nil
			}, command: func(_ context.Context, args []string, body []byte) ([]byte, error) {
				if patches != 0 {
					t.Fatalf("command continued after diagnostic: %v", args)
				}
				switch args[0] {
				case "get":
					switch args[1] {
					case "lease":
						return json.Marshal(lease)
					case "volumes.longhorn.io":
						return json.Marshal(backing)
					case "pod":
						if args[2] == id(leaderPod).name {
							return json.Marshal(leaderPod)
						}
					}
					return fakeRead(s, args[1:])
				case "logs":
					record := pauseRecord()
					record["msg"] = "ordinary reconciliation"
					return json.Marshal(record)
				case "patch":
					patches++
					wantArgs := []string{"patch", "cluster.postgresql.cnpg.io", clusterName, "--type=json", "--patch-file=/dev/stdin", "--field-manager=" + fieldManager, "-n", namespace, "--dry-run=server"}
					if !reflect.DeepEqual(args, wantArgs) || sourceReads < 2 {
						t.Fatalf("diagnostic escaped its command or source guard: %v", args)
					}
					var ops []object
					if err := json.Unmarshal(body, &ops); err != nil || !reflect.DeepEqual(ops, pausePatch(s.cluster, str(s.cluster, "status", "currentPrimary"))) {
						t.Fatal("diagnostic did not use the exact guarded pause patch")
					}
					if failure == "rejected patch" {
						return nil, &exec.ExitError{Stderr: []byte("Error from server (Forbidden): fixture-private-server-error")}
					}
					return []byte("{}"), nil
				default:
					t.Fatalf("unexpected diagnostic command: %v", args)
					return nil, errors.New("unexpected command")
				}
			}}
			o := testOptions()
			o.diagnosePause = true
			err := quarantineCompletedJoin(context.Background(), c, o, guards, failure == "mixed execution")
			wantPatches := 0
			if failure == "" || failure == "rejected patch" {
				wantPatches = 1
			}
			after, snapshotErr := json.Marshal([]any{s.cluster, s.operator, s.pods, s.claims, s.backups, s.jobs, s.volumes})
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			if patches != wantPatches || (err == nil) != (failure == "") || !reflect.DeepEqual(before, after) {
				t.Fatalf("diagnostic patches=%d expected=%d error=%v", patches, wantPatches, err)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-private-server-error") {
				t.Fatal("raw server error escaped diagnostic")
			}
			if failure == "rejected patch" && !strings.Contains(err.Error(), "reason=SERVER_FORBIDDEN") {
				t.Fatal("diagnostic lost the bounded server rejection category")
			}
		})
	}
}

// TestPausePatchPreservesEverySemanticPrecondition independently pins the shared
// request, so using the same builder in two callers cannot hide a weakened test.
func TestPausePatchPreservesEverySemanticPrecondition(t *testing.T) {
	for _, annotations := range []object{nil, {"keep": "unchanged"}} {
		cluster := object{"metadata": object{"uid": "cluster-uid", "resourceVersion": "12"}}
		if annotations != nil {
			at(cluster, "metadata")["annotations"] = annotations
		}
		want := []object{
			{"op": "test", "path": "/metadata/uid", "value": "cluster-uid"},
			{"op": "test", "path": "/metadata/resourceVersion", "value": "12"},
			{"op": "test", "path": "/status/currentPrimary", "value": "stable-primary"},
			{"op": "test", "path": "/status/targetPrimary", "value": "stable-primary"},
			{"op": "test", "path": "/spec/instances", "value": float64(3)},
		}
		if annotations == nil {
			want = append(want, object{"op": "add", "path": "/metadata/annotations", "value": object{pauseKey: "disabled"}})
		} else {
			want = append(want, object{"op": "test", "path": "/metadata/annotations", "value": annotations}, object{"op": "add", "path": "/metadata/annotations/cnpg.io~1reconciliationLoop", "value": "disabled"})
		}
		before, _ := json.Marshal(cluster)
		got := pausePatch(cluster, "stable-primary")
		after, _ := json.Marshal(cluster)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(before, after) {
			t.Fatal("shared pause patch weakened its preconditions or changed the observed Cluster")
		}
	}
}

// TestPauseDiagnosticWorkflowIsSeparatelyProtected checks the effective YAML
// shape, credentials-after-guard ordering, and absence of any execution flag.
func TestPauseDiagnosticWorkflowIsSeparatelyProtected(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/diagnose-wedding-pause.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w object
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	triggers := at(w, "on")
	_, mergeGroup := triggers["merge_group"]
	if len(triggers) != 3 || !mergeGroup || triggers["workflow_dispatch"] == nil || triggers["pull_request"] == nil || value(w, "permissions") == nil || len(at(w, "permissions")) != 0 {
		t.Fatal("diagnostic trigger boundary changed")
	}
	j := at(w, "jobs", "diagnostic")
	if str(j, "environment") != "prod" || str(j, "concurrency", "group") != "prod-deploy" || value(j, "concurrency", "cancel-in-progress") != false || len(at(j, "permissions")) != 1 || str(j, "permissions", "contents") != "read" || str(j, "if") != "${{ github.event_name == 'workflow_dispatch' }}" {
		t.Fatal("diagnostic lost protected dispatch-only least-privilege isolation")
	}
	steps := list(j, "steps")
	if len(steps) < 2 || !strings.Contains(str(steps[0], "run"), "refs/heads/main") || !strings.Contains(str(steps[0], "run"), "dry-run-retained-wedding-pause") || !strings.Contains(str(steps[0], "run"), "GITHUB_RUN_ATTEMPT") || str(steps[1], "with", "ref") != "${{ github.sha }}" || value(steps[1], "with", "persist-credentials") != false {
		t.Fatal("diagnostic credentials precede the reviewed first-main-dispatch guard")
	}
	if !strings.Contains(string(b), "--diagnose-pause --quarantine-completed-join") || strings.Contains(string(b), "--execute") || strings.Contains(string(b), "--resume-fenced") {
		t.Fatal("diagnostic workflow can enter recovery execution")
	}
}
