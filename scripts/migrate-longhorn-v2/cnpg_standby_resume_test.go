package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRetainedUmamiStandbyStillRequiresFourHealthyProcesses(t *testing.T) {
	before := umamiV2TransitionObservation(t)
	before.retainedStandby = "umami-db-2"
	status := valueObject(before.Cluster, "status")
	status["instances"] = float64(3)
	status["healthyPVC"] = []any{"umami-db-1", "umami-db-3", "umami-db-4"}
	if err := validateCNPGObservation(before, before); err != nil {
		t.Fatalf("a retained claim must not hide the fourth healthy process: %v", err)
	}
	for name, change := range map[string]func(*cnpgObservation){
		"ordinary observation": func(s *cnpgObservation) { s.retainedStandby = "" },
		"incorrect exception":  func(s *cnpgObservation) { s.retainedStandby = "umami-db-3" },
		"missing healthy PVC peer": func(s *cnpgObservation) {
			valueObject(s.Cluster, "status")["healthyPVC"] = []any{"umami-db-1", "umami-db-2", "umami-db-4"}
		},
		"duplicate healthy PVC": func(s *cnpgObservation) {
			valueObject(s.Cluster, "status")["healthyPVC"] = []any{"umami-db-1", "umami-db-3", "umami-db-3"}
		},
		"fewer Ready pods": func(s *cnpgObservation) {
			valueObject(s.Cluster, "status")["readyInstances"] = float64(3)
		},
		"missing physical replay": func(s *cnpgObservation) { delete(s.States, "umami-db-4") },
		"changing writer": func(s *cnpgObservation) {
			valueObject(s.Cluster, "status")["targetPrimary"] = "umami-db-3"
		},
	} {
		t.Run(name, func(t *testing.T) {
			after := cloneCNPGObservation(t, before)
			after.retainedStandby = before.retainedStandby
			change(&after)
			if err := validateCNPGObservation(after, after); err == nil {
				t.Fatal("an unsafe retained-claim observation passed")
			}
		})
	}
}

func TestUmamiStandbyResumeRequiresTheOriginalTerminalAttempt(t *testing.T) {
	const nextSource = "1ff7d913115733d91aa1edae762efae5fa600c52"
	witness, target, claim, pv, volume, pdb := umamiStandbyCutoverFixture(t)
	old, err := bindUmamiStandbyCutover(witness, witness.Pods, text(target, "metadata", "name"), claim, pv, volume, pdb)
	if err != nil {
		t.Fatal(err)
	}
	old.Database.UID = "ac005841-9999-4999-8999-999999999999"
	old.Spare.Cluster = old.Database
	old.Spare.Pod = benchmarkResource{"umami-db-4", "umami", "ac005841-8444-4444-8444-444444444444"}
	prior := umamiStandbyResume{old.Consumer.Name, "45942954918", "23494e7388b3b1a7fc27da52f9ef5799cfb6ef10", "ac005841-7777-4777-8777-777777777777"}
	record := umamiStandbyReplacementRecord{
		Version: 1, Source: prior.source, Run: prior.run, Phase: umamiReplacementClaimRetained,
		Primary: benchmarkResource{"umami-db-1", "umami", "ac005841-6666-4666-8666-666666666666"}, Original: old,
	}
	record.OtherPods = map[string]benchmarkResource{
		record.Primary.Name: record.Primary, old.Spare.Pod.Name: old.Spare.Pod,
		"umami-db-3": {"umami-db-3", "umami", "ac005841-5555-4555-8555-666666666666"},
	}
	owner := umamiReplacementOwner{Source: nextSource, Run: "45953070468"}
	for _, scenario := range []string{"success", "wrong UID", "later phase", "earlier phase", "live run", "rerun", "main drift", "duplicate proof"} {
		t.Run(scenario, func(t *testing.T) {
			current := record
			status, attempt, uid, source := "completed", 1, prior.uid, owner.Source
			switch scenario {
			case "wrong UID":
				uid = "ac005841-1111-4111-8111-111111111111"
			case "earlier phase":
				current.Phase = umamiReplacementRetentionPending
			case "later phase":
				current.Phase = umamiReplacementEvictionPending
			case "live run":
				status = "in_progress"
			case "rerun":
				attempt = 2
			case "main drift":
				source = prior.source
			}
			raw, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "duplicate proof" {
				raw = []byte(strings.Replace(string(raw), `"Version":1`, `"Version":1,"Version":1`, 1))
			}
			cm := object{"apiVersion": "v1", "kind": "ConfigMap", "data": object{"record": string(raw)},
				"metadata": object{"name": umamiStandbyJournalName(record), "namespace": umamiStandbyJournalNamespace,
					"uid": uid, "resourceVersion": "1", "labels": object{
						"app.kubernetes.io/managed-by": "github-actions", "storage.platform.devantler.tech/recovery-run": prior.run,
					}},
			}
			reads := 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil {
					t.Fatal("continuation admission attempted a write")
				}
				switch {
				case program == "git":
					return []byte(source + "\trefs/heads/main\n"), nil
				case program == "gh":
					return json.Marshal(object{"id": 45942954918, "status": status, "conclusion": "failure",
						"head_sha": prior.source, "head_branch": "main", "event": "workflow_dispatch",
						"run_attempt": attempt, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"})
				case program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "configmap":
					reads++
					return json.Marshal(cm)
				default:
					return nil, errors.New("unexpected command")
				}
			}}
			journal, err := readResumableUmamiStandby(t.Context(), c, owner, prior)
			if scenario != "success" {
				if err == nil {
					t.Fatal("an unsafe continuation passed")
				}
				return
			}
			if err != nil || reads != 2 || journal.executor() != owner || journal.record.Source != prior.source ||
				journal.record.Run != prior.run || journal.record.Phase != umamiReplacementClaimRetained {
				t.Fatalf("continuation did not preserve the prior run and its checkpoint: %v", err)
			}
		})
	}
}

func TestUmamiStandbyResumeInputUsesProtectedWorkflow(t *testing.T) {
	const valid = "resume-retained-umami-v2-standby:umami-db-2:45942954918:23494e7388b3b1a7fc27da52f9ef5799cfb6ef10:ac005841-7777-4777-8777-777777777777"
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	if !slices.Contains(choices, resumeUmamiStandbyOperation) {
		t.Fatal("the protected workflow is missing the continuation")
	}
	env := umamiStandbyDispatch()
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = resumeUmamiStandbyOperation, valid
	lookup := func(key string) string { return env[key] }
	if _, err := authorizeUmamiStandbyResume(lookup); err != nil {
		t.Fatal(err)
	}
	env["GITHUB_RUN_ATTEMPT"] = "2"
	if _, err := authorizeUmamiStandbyResume(lookup); err == nil {
		t.Fatal("a rerun authorized a continuation")
	}
}
