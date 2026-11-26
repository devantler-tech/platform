package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestRecoveryEnvironmentPersistsIntentBeforeEveryExclusiveCreate exercises the journal, not only a renderer.
func TestRecoveryEnvironmentPersistsIntentBeforeEveryExclusiveCreate(t *testing.T) {
	for _, scenario := range []string{"confirmed", "uncertain", "replaced namespace"} {
		t.Run(scenario, func(t *testing.T) {
			uncertain := scenario == "uncertain"
			c, req, _ := recoveryPlanningFixture(t)
			plan, err := planCNPGRecovery(context.Background(), c, req)
			if err != nil {
				t.Fatal(err)
			}
			r := cnpgProofRecord{Version: 1, Source: req.Source, Run: req.Run, Target: req.Target, Phase: "restore-planned", Archive: req.Archive, Backup: req.Backup, Plan: &plan, Resources: map[string]benchmarkResource{}}
			cm := ownedCNPGJournalFixture(t, r)
			creates := 0
			createdResources := map[string]object{}
			sequence := []string{}
			p := cnpgProofRun{record: r, journalUID: text(cm, "metadata", "uid")}
			p.c = client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(req.Source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 3 {
					t.Fatal("unexpected executable")
				}
				switch args[2] {
				case "get":
					if args[3] != "configmap" {
						return json.Marshal(createdResources[args[3]])
					}
					return json.Marshal(cm)
				case "patch":
					var ops []object
					if json.Unmarshal(input, &ops) != nil || len(ops) != 3 || ops[0]["value"] != p.journalUID || ops[1]["value"] != text(cm, "metadata", "resourceVersion") {
						t.Fatal("missing journal CAS")
					}
					cm["data"].(object)["record"] = text(ops[2], "value")
					cm["metadata"].(object)["resourceVersion"] = text(cm, "metadata", "resourceVersion") + "1"
					var next cnpgProofRecord
					if json.Unmarshal([]byte(text(ops[2], "value")), &next) != nil {
						t.Fatal("bad checkpoint")
					}
					sequence = append(sequence, next.Phase)
					return []byte("patched"), nil
				case "create":
					creates++
					var persisted cnpgProofRecord
					if json.Unmarshal([]byte(text(cm, "data", "record")), &persisted) != nil {
						t.Fatal("missing durable record")
					}
					var desired object
					if json.Unmarshal(input, &desired) != nil {
						t.Fatal("bad request")
					}
					kind := text(desired, "kind")
					phase := map[string]string{"Namespace": "namespace-requested", "CiliumNetworkPolicy": "policy-requested"}[kind]
					if phase == "" || persisted.Phase != phase || len(persisted.Resources) != creates-1 {
						t.Fatal("create preceded durable intent")
					}
					sequence = append(sequence, "create-"+kind)
					if uncertain {
						return nil, errors.New("unknown API outcome")
					}
					desired["metadata"].(object)["uid"] = "11111111-1111-4111-8111-" + map[int]string{1: "111111111111", 2: "222222222222"}[creates]
					desired["metadata"].(object)["resourceVersion"] = "10"
					createdResources[map[string]string{"Namespace": "namespace", "CiliumNetworkPolicy": "ciliumnetworkpolicies.cilium.io"}[kind]] = desired
					return json.Marshal(desired)
				}
				t.Fatal("unexpected command")
				return nil, nil
			}}
			_, err = p.createEnvironmentResource(context.Background(), "Namespace")
			if uncertain {
				if err == nil || creates != 1 || p.record.Phase != "namespace-requested" {
					t.Fatal("unknown outcome accepted")
				}
				if _, err = p.createEnvironmentResource(context.Background(), "Namespace"); err == nil || creates != 1 {
					t.Fatal("uncertain creation replayed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "replaced namespace" {
				createdResources["namespace"]["metadata"].(object)["uid"] = "33333333-3333-4333-8333-333333333333"
				if _, err = p.createEnvironmentResource(context.Background(), "Policy"); err == nil || creates != 1 {
					t.Fatal("policy creation entered a replacement namespace")
				}
				return
			}
			if _, err = p.createEnvironmentResource(context.Background(), "Policy"); err != nil {
				t.Fatal(err)
			}
			want := "namespace-requested,create-Namespace,namespace-created,policy-requested,create-CiliumNetworkPolicy,policy-created"
			if strings.Join(sequence, ",") != want || creates != 2 || len(p.record.Resources) != 2 {
				t.Fatalf("lost ordering: %v", sequence)
			}
		})
	}
}

// TestRecoveryEnvironmentRejectsSkippingPolicyOrder prevents advancing an incomplete environment.
func TestRecoveryEnvironmentRejectsSkippingPolicyOrder(t *testing.T) {
	p := cnpgProofRun{record: cnpgProofRecord{Phase: "restore-planned"}, c: client{run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("invalid creation reached a command")
		return nil, nil
	}}}
	if _, err := p.createEnvironmentResource(context.Background(), "Policy"); err == nil {
		t.Fatal("policy skipped namespace")
	}
}
