package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func recoveryFixture(t *testing.T) benchmarkRecord {
	t.Helper()
	machines, err := benchmarkMachines(benchmarkInventoryFixture(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	return benchmarkRecord{Version: 2, Phase: "v1", Source: strings.Repeat("1", 40), Run: "1234", Image: fioImage, Workload: benchmarkWorkload(), Namespace: "longhorn-benchmark-v1-1234", Machines: machines}
}

func TestBenchmarkRecoveryClaimsExactJournalWithoutReattributingOriginalRun(t *testing.T) {
	original := recoveryFixture(t)
	stored := original
	reads, patches := 0, 0
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		switch program {
		case "git":
			return []byte(strings.Repeat("2", 40) + "\trefs/heads/main\n"), nil
		case "gh":
			reads++
			if strings.Join(args, " ") != "api repos/devantler-tech/platform/actions/runs/1234" {
				t.Fatal("terminal check did not read the latest original attempt")
			}
			return []byte(`{"id":1234,"status":"completed","head_sha":"` + original.Source + `","head_branch":"main","event":"workflow_dispatch","run_attempt":1,"path":".github/workflows/prepare-longhorn-v2-node.yaml"}`), nil
		case "kubectl":
			if args[2] == "patch" {
				var patch []object
				if json.Unmarshal(input, &patch) != nil || len(patch) != 3 || text(patch[0], "op") != "test" || text(patch[0], "path") != "/metadata/uid" || value(patch[0], "value") != "uid-longhorn-v2-benchmark-v1" || text(patch[1], "path") != "/metadata/resourceVersion" || value(patch[1], "value") != "1" {
					t.Fatal("recovery claim lacks atomic journal UID/version tests")
				}
				if json.Unmarshal([]byte(text(patch[2], "value")), &stored) != nil {
					t.Fatal("invalid journal patch")
				}
				patches++
				return nil, nil
			}
			cm := resource("longhorn-v2-benchmark-v1")
			raw, _ := json.Marshal(stored)
			cm["data"] = object{"record": string(raw)}
			return json.Marshal(cm)
		default:
			t.Fatalf("unexpected command %s", program)
			return nil, errors.New("unexpected command")
		}
	}}
	claimed, err := claimBenchmarkRecovery(context.Background(), c, "v1", strings.Repeat("2", 40), "5678")
	if err != nil || reads != 1 || patches != 1 || claimed.Recovery == nil || claimed.Recovery.Source != strings.Repeat("2", 40) || claimed.Recovery.Run != "5678" {
		t.Fatalf("recovery claim failed: reads=%d patches=%d error=%v", reads, patches, err)
	}
	claimed.Recovery = nil
	if !reflect.DeepEqual(claimed, original) {
		t.Fatal("recovery changed the original source, run, identities or measurements")
	}
}

func TestBenchmarkRecoveryRejectsLiveUnknownOrChangedLatestWorkflow(t *testing.T) {
	const valid = `{"id":1234,"status":"completed","head_sha":"1111111111111111111111111111111111111111","head_branch":"main","event":"workflow_dispatch","run_attempt":1,"path":".github/workflows/prepare-longhorn-v2-node.yaml"}`
	for name, body := range map[string]string{
		"live latest retry": strings.Replace(strings.Replace(valid, `"completed"`, `"in_progress"`, 1), `"run_attempt":1`, `"run_attempt":2`, 1),
		"other run":         strings.Replace(valid, `"id":1234`, `"id":1235`, 1),
		"other source":      strings.Replace(valid, strings.Repeat("1", 40), strings.Repeat("3", 40), 1),
		"other workflow":    strings.Replace(valid, "prepare-longhorn-v2-node.yaml", "other.yaml", 1),
		"duplicate status":  strings.Replace(valid, `"status":"completed"`, `"status":"in_progress","status":"completed"`, 1),
		"case alias":        strings.Replace(valid, `"status"`, `"Status"`, 1),
		"partial response":  valid[:len(valid)-1],
		"extra document":    valid + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := client{run: func(context.Context, []byte, string, ...string) ([]byte, error) { return []byte(body), nil }}
			if benchmarkOwnerFinished(context.Background(), c, "1234", strings.Repeat("1", 40)) == nil {
				t.Fatal("unproven prior owner was accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := client{run: func(context.Context, []byte, string, ...string) ([]byte, error) { return []byte(valid), nil }}
	if benchmarkOwnerFinished(ctx, c, "1234", strings.Repeat("1", 40)) == nil {
		t.Fatal("expired workflow observation accepted")
	}
}

func TestBenchmarkRecoveryRefusesContentionLiveOwnersAndStaleMutationSource(t *testing.T) {
	for _, failure := range []string{"live original", "live previous recovery", "stale source", "claim conflict", "changed readback"} {
		t.Run(failure, func(t *testing.T) {
			record := recoveryFixture(t)
			if failure == "live previous recovery" {
				record.Recovery = &benchmarkRecoveryOwner{Source: strings.Repeat("3", 40), Run: "3456", JournalUID: "uid-longhorn-v2-benchmark-v1"}
			}
			patches := 0
			c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				switch program {
				case "git":
					sha := strings.Repeat("2", 40)
					if failure == "stale source" {
						sha = strings.Repeat("4", 40)
					}
					return []byte(sha + "\trefs/heads/main\n"), nil
				case "gh":
					id, source, status := "1234", record.Source, "completed"
					if strings.HasSuffix(args[1], "/3456") {
						id, source = "3456", strings.Repeat("3", 40)
					}
					if failure == "live original" || failure == "live previous recovery" && id == "3456" {
						status = "in_progress"
					}
					return []byte(`{"id":` + id + `,"status":"` + status + `","head_sha":"` + source + `","head_branch":"main","event":"workflow_dispatch","run_attempt":2,"path":".github/workflows/prepare-longhorn-v2-node.yaml"}`), nil
				case "kubectl":
					if args[2] == "patch" {
						patches++
						if failure == "claim conflict" {
							return nil, errConflict
						}
						return nil, nil
					}
					cm := resource("longhorn-v2-benchmark-v1")
					if patches > 0 && failure == "changed readback" {
						cm["metadata"].(object)["uid"] = "replacement-journal"
					}
					raw, _ := json.Marshal(record)
					cm["data"] = object{"record": string(raw)}
					return json.Marshal(cm)
				default:
					t.Fatal("unexpected command")
					return nil, errors.New("unexpected command")
				}
			}}
			if _, err := claimBenchmarkRecovery(context.Background(), c, "v1", strings.Repeat("2", 40), "5678"); err == nil {
				t.Fatal("unproven claim accepted")
			}
			want := 0
			if failure == "claim conflict" || failure == "changed readback" {
				want = 1
			}
			if patches != want {
				t.Fatalf("unexpected mutation count: %d", patches)
			}
		})
	}
}

func TestAmbiguousRecoveryJournalCannotBeClaimed(t *testing.T) {
	record := recoveryFixture(t)
	raw, _ := json.Marshal(record)
	receipt := recoveryFixture(t)
	receipt.NamespaceUID = "accepted"
	receipt.Resources = map[string]benchmarkResource{"Namespace": {Name: receipt.Namespace, UID: "foreign"}}
	receiptRaw, _ := json.Marshal(receipt)
	for _, bad := range []string{
		strings.Replace(string(raw), `"Run":"1234"`, `"Run":"foreign","Run":"1234"`, 1),
		strings.Replace(string(raw), `"Run":"1234"`, `"run":"1234"`, 1),
		strings.TrimSuffix(string(raw), "}") + `,"Recovery":{"Source":"` + strings.Repeat("2", 40) + `","Run":"5678","JournalUID":"other"}}`,
		strings.Replace(string(receiptRaw), `"UID":"foreign"`, `"UID":"foreign","uid":"accepted"`, 1),
	} {
		c := client{run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
			if program != "kubectl" || args[2] != "get" {
				t.Fatal("ambiguous journal caused owner checks or writes")
			}
			cm := resource("longhorn-v2-benchmark-v1")
			cm["data"] = object{"record": bad}
			return json.Marshal(cm)
		}}
		if _, err := claimBenchmarkRecovery(context.Background(), c, "v1", strings.Repeat("2", 40), "5678"); err == nil {
			t.Fatal("ambiguous journal accepted")
		}
	}
}

func TestSameRecoveryOwnerRetryRevalidatesJournalAfterExternalReads(t *testing.T) {
	for _, change := range []string{"replacement UID", "other recovery owner"} {
		t.Run(change, func(t *testing.T) {
			record := recoveryFixture(t)
			record.Recovery = &benchmarkRecoveryOwner{Source: strings.Repeat("2", 40), Run: "5678", JournalUID: "uid-longhorn-v2-benchmark-v1"}
			drifted := false
			c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				switch program {
				case "git":
					return []byte(strings.Repeat("2", 40) + "\trefs/heads/main\n"), nil
				case "gh":
					drifted = true
					return []byte(`{"id":1234,"status":"completed","head_sha":"` + record.Source + `","head_branch":"main","event":"workflow_dispatch","run_attempt":1,"path":".github/workflows/prepare-longhorn-v2-node.yaml"}`), nil
				case "kubectl":
					if args[2] != "get" {
						t.Fatal("same-owner retry unexpectedly wrote")
					}
					cm := resource("longhorn-v2-benchmark-v1")
					if drifted {
						if change == "replacement UID" {
							cm["metadata"].(object)["uid"] = "other"
						} else {
							record.Recovery.Run = "6789"
						}
					}
					raw, _ := json.Marshal(record)
					cm["data"] = object{"record": string(raw)}
					return json.Marshal(cm)
				default:
					t.Fatal("unexpected command")
					return nil, errors.New("unexpected command")
				}
			}}
			if _, err := claimBenchmarkRecovery(context.Background(), c, "v1", strings.Repeat("2", 40), "5678"); err == nil {
				t.Fatal("stale same-owner claim returned after journal drift")
			}
		})
	}
}

func TestOriginalBenchmarkCannotSaveProgressAfterRecoveryTakesOwnership(t *testing.T) {
	record := recoveryFixture(t)
	owned := record
	owned.Recovery = &benchmarkRecoveryOwner{Source: strings.Repeat("2", 40), Run: "5678", JournalUID: "uid-longhorn-v2-benchmark-v1"}
	c := client{run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
		if program != "kubectl" || args[2] != "get" {
			t.Fatal("original owner attempted a write after recovery takeover")
		}
		cm := resource("longhorn-v2-benchmark-v1")
		raw, _ := json.Marshal(owned)
		cm["data"] = object{"record": string(raw)}
		return json.Marshal(cm)
	}}
	if saveBenchmarkProgress(context.Background(), c, record.Source, record.Run, record) == nil {
		t.Fatal("original benchmark overwrote its recovery owner")
	}
}

func TestScratchCreateIntentPrecedesRequestAndPreservesAmbiguousOutcome(t *testing.T) {
	for _, failure := range []string{"none", "create response lost", "receipt write lost", "intent write failed"} {
		t.Run(failure, func(t *testing.T) {
			record := recoveryFixture(t)
			stored := record
			assets, _ := benchmarkResources(record.Phase, record.Run, record.Namespace)
			creates, saves := 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(record.Source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" {
					t.Fatal("unexpected command")
				}
				switch args[2] {
				case "get":
					cm := resource("longhorn-v2-benchmark-v1")
					raw, _ := json.Marshal(stored)
					cm["data"] = object{"record": string(raw)}
					return json.Marshal(cm)
				case "patch":
					saves++
					if failure == "intent write failed" || failure == "receipt write lost" && saves == 2 {
						return nil, errors.New("journal write failed")
					}
					var patch []object
					stored = benchmarkRecord{}
					if json.Unmarshal(input, &patch) != nil || json.Unmarshal([]byte(text(patch[len(patch)-1], "value")), &stored) != nil {
						t.Fatal("invalid journal save")
					}
					return nil, nil
				case "create":
					creates++
					if saves != 1 || stored.PendingCreate != "Namespace" || len(stored.Resources) != 0 {
						t.Fatal("CREATE occurred without durable unresolved intent")
					}
					if failure == "create response lost" {
						return nil, errors.New("response lost")
					}
					var actual object
					if json.Unmarshal(input, &actual) != nil {
						t.Fatal("invalid create")
					}
					actual["metadata"].(object)["uid"] = "scratch-namespace"
					actual["metadata"].(object)["resourceVersion"] = "2"
					return json.Marshal(actual)
				default:
					t.Fatal("unexpected operation")
					return nil, errors.New("unexpected operation")
				}
			}}
			actual, err := createBenchmarkScratch(context.Background(), c, &record, assets[0])
			if failure == "none" {
				if err != nil || text(actual, "metadata", "uid") != "scratch-namespace" || stored.PendingCreate != "" || stored.Resources["Namespace"].UID != "scratch-namespace" {
					t.Fatalf("successful creation receipt missing: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("ambiguous creation accepted")
				}
				if failure == "intent write failed" {
					if creates != 0 {
						t.Fatal("created scratch after failed intent save")
					}
				} else if stored.PendingCreate != "Namespace" || len(stored.Resources) != 0 {
					t.Fatal("ambiguous CREATE ownership was silently adopted")
				}
			}
		})
	}
}

func TestBenchmarkTagsRefuseAReplacementLonghornNode(t *testing.T) {
	writes := 0
	c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
		if program != "kubectl" || args[2] != "get" {
			writes++
			return nil, errors.New("unexpected write")
		}
		n := resource("prod-worker-1")
		n["metadata"].(object)["uid"] = "replacement"
		n["spec"] = object{"tags": []any{}}
		return json.Marshal(n)
	}}
	if c.setBenchmarkTag(context.Background(), strings.Repeat("1", 40), "longhorn-benchmark-v1-1234", "prod-worker-1", "original", true) == nil || writes != 0 {
		t.Fatal("tag mutation reached a replacement node")
	}
	if _, err := benchmarkTagNodes(benchmarkInventoryFixture().storageNodes); err != nil {
		t.Fatal(err)
	}
	broken := benchmarkInventoryFixture().storageNodes
	broken[0]["metadata"].(object)["uid"] = ""
	if _, err := benchmarkTagNodes(broken); err == nil {
		t.Fatal("missing node UID accepted")
	}
}
