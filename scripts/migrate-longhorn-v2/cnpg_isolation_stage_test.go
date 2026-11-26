package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func recoveryIsolationStageFixture(t *testing.T, mutation string) (*cnpgProofRun, *int) {
	t.Helper()
	return recoveryIsolationOnHostFixture(t, mutation, nil)
}

func recoveryIsolationOnHostFixture(t *testing.T, mutation string, host object) (*cnpgProofRun, *int) {
	t.Helper()
	c, input, _ := recoveryObservationFixture(t, nil)
	hostname := "prod-worker-3"
	if host != nil {
		hostname = text(host, "metadata", "name")
		valueObject(input.Node, "metadata")["uid"] = text(host, "metadata", "uid")
		valueObject(input.Node, "status", "nodeInfo")["bootID"] = text(host, "status", "nodeInfo", "bootID")
	}
	input.Agent["spec"].(object)["nodeName"] = hostname
	input.Node["metadata"].(object)["name"] = hostname
	input.Node["metadata"].(object)["labels"] = object{"kubernetes.io/hostname": hostname}
	pc, req, _ := recoveryPlanningFixture(t)
	req.Hostname = hostname
	plan, err := planCNPGRecovery(context.Background(), pc, req)
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := cloneRecoverySpec(plan.Resources[0])
	if err != nil {
		t.Fatal(err)
	}
	namespace["metadata"].(object)["uid"] = "88888888-8888-4888-8888-888888888888"
	namespace["metadata"].(object)["resourceVersion"] = "10"
	r := cnpgProofRecord{Version: 1, Source: req.Source, Run: req.Run, Target: req.Target, Phase: "policy-created", Archive: req.Archive, Backup: req.Backup, Plan: &plan, Resources: map[string]benchmarkResource{"Namespace": benchmarkIdentity(namespace), "Policy": benchmarkIdentity(input.Policy)}}
	cm := ownedCNPGJournalFixture(t, r)
	delegate := c.run
	patches, lists, policyReads := 0, 0, 0
	p := &cnpgProofRun{c: c, record: r, journalUID: text(cm, "metadata", "uid")}
	p.c.run = func(ctx context.Context, stdin []byte, command string, args ...string) ([]byte, error) {
		if command == "git" {
			if mutation == "main advanced" {
				return []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\trefs/heads/main\n"), nil
			}
			return []byte(req.Source + "\trefs/heads/main\n"), nil
		}
		if command != "kubectl" || len(args) < 4 {
			return nil, errors.New("unexpected command")
		}
		if args[2] == "patch" {
			patches++
			var ops []object
			if json.Unmarshal(stdin, &ops) != nil || len(ops) != 3 || ops[0]["value"] != p.journalUID || ops[1]["value"] != text(cm, "metadata", "resourceVersion") {
				t.Fatal("missing journal CAS")
			}
			cm["data"].(object)["record"] = text(ops[2], "value")
			cm["metadata"].(object)["resourceVersion"] = "11"
			return []byte("patched"), nil
		}
		if args[2] == "get" {
			if args[3] == "configmap" {
				return json.Marshal(cm)
			}
			if args[3] == "namespace" {
				if mutation == "namespace replaced" {
					namespace["metadata"].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
				}
				return json.Marshal(namespace)
			}
			if len(args) > 4 && args[4] == "--output=json" {
				lists++
				items := []object{}
				switch args[3] {
				case "nodes":
					items = []object{input.Node}
					if mutation == "duplicate hostname" {
						duplicateNode, _ := cloneRecoverySpec(input.Node)
						duplicateNode["metadata"].(object)["name"] = "foreign-worker"
						items = append(items, duplicateNode)
					}
				case "pods":
					if args[len(args)-1] == "--namespace=kube-system" {
						items = []object{input.Agent}
						if mutation == "duplicate agent" {
							items = append(items, input.Agent)
						}
					} else if mutation == "unexpected Pod" {
						items = []object{resource("foreign")}
					}
				case "secrets":
					if mutation == "unexpected credential" {
						items = []object{resource("foreign")}
					}
				}
				if mutation == "failed complete inventory" && lists == 2 {
					return nil, errors.New("unavailable")
				}
				return json.Marshal(object{"apiVersion": "v1", "kind": "List", "items": items})
			}
		}
		if args[2] == "get" && args[3] == "ciliumnetworkpolicies.cilium.io" {
			policyReads++
			if mutation == "policy label changed" || mutation == "policy terminating after import" && policyReads == 4 {
				raw, err := delegate(ctx, stdin, command, args...)
				if err != nil {
					return nil, err
				}
				var changed object
				if json.Unmarshal(raw, &changed) != nil {
					t.Fatal("bad policy fixture")
				}
				if mutation == "policy label changed" {
					value(changed, "metadata", "labels").(object)["storage.platform.devantler.tech/recovery-run"] = "5678"
				} else {
					changed["metadata"].(object)["deletionTimestamp"] = "2026-11-03T22:00:00Z"
				}
				return json.Marshal(changed)
			}
		}
		return delegate(ctx, stdin, command, args...)
	}
	return p, &patches
}

// TestRecoveryIsolationStageCommitsOnlyAfterAnEmptyOwnedNamespaceAndNativeImport binds the whole precreation stage.
func TestRecoveryIsolationStageCommitsOnlyAfterAnEmptyOwnedNamespaceAndNativeImport(t *testing.T) {
	p, writes := recoveryIsolationStageFixture(t, "")
	if err := p.prepareIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *writes != 1 || p.record.Phase != "isolation-prepared" || p.record.Preparation == nil || p.record.Preparation.Revision != 817 {
		t.Fatal("missing native isolation checkpoint")
	}
}

// TestRecoveryIsolationStageCannotAdoptForeignDataOrAmbiguousProcesses guards the complete inventory boundary.
func TestRecoveryIsolationStageCannotAdoptForeignDataOrAmbiguousProcesses(t *testing.T) {
	for _, mutation := range []string{"namespace replaced", "duplicate hostname", "duplicate agent", "unexpected Pod", "unexpected credential", "failed complete inventory", "main advanced", "policy label changed", "policy terminating after import"} {
		t.Run(mutation, func(t *testing.T) {
			p, writes := recoveryIsolationStageFixture(t, mutation)
			if p.prepareIsolation(context.Background()) == nil || *writes != 0 || p.record.Preparation != nil {
				t.Fatal("incomplete isolation checkpointed")
			}
		})
	}
}
