package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRecoveryWorkloadDeletionKeepsUIDIntentAcrossResponseLossAndRetries(t *testing.T) {
	for _, scenario := range []string{"normal", "delete response lost", "missing without intent", "replacement before delete", "replacement during absence check", "main moved", "CAS conflict"} {
		t.Run(scenario, func(t *testing.T) {
			record := recoveryFixture(t)
			record.NamespaceUID = "scratch-namespace"
			record.Resources = map[string]benchmarkResource{
				"Namespace": {Name: record.Namespace, UID: record.NamespaceUID},
				"Pod":       {Name: "fio", Namespace: record.Namespace, UID: "original-pod"},
			}
			record.Recovery = &benchmarkRecoveryOwner{Source: strings.Repeat("2", 40), Run: "5678", JournalUID: "uid-longhorn-v2-benchmark-v1"}
			if scenario == "replacement during absence check" {
				record.Recovery.Deletes = map[string]benchmarkResource{"Pod": record.Resources["Pod"]}
			}
			stored := record
			missing := scenario == "missing without intent" || scenario == "replacement during absence check"
			patches, deletes := 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				switch program {
				case "git":
					sha := strings.Repeat("2", 40)
					if scenario == "main moved" && patches > 0 {
						sha = strings.Repeat("3", 40)
					}
					return []byte(sha + "\trefs/heads/main\n"), nil
				case "gh":
					return []byte(`{"id":1234,"status":"completed","head_sha":"` + record.Source + `","head_branch":"main","event":"workflow_dispatch","run_attempt":1,"path":".github/workflows/prepare-longhorn-v2-node.yaml"}`), nil
				case "kubectl":
					switch args[2] {
					case "patch":
						patches++
						if scenario == "CAS conflict" {
							return nil, errConflict
						}
						var patch []object
						stored = benchmarkRecord{}
						if json.Unmarshal(input, &patch) != nil || json.Unmarshal([]byte(text(patch[len(patch)-1], "value")), &stored) != nil {
							t.Fatal("invalid progress patch")
						}
						return nil, nil
					case "delete":
						if stored.Recovery.Deletes["Pod"] != stored.Resources["Pod"] {
							t.Fatal("delete occurred without durable exact-UID intent")
						}
						var options object
						if json.Unmarshal(input, &options) != nil || text(options, "preconditions", "uid") != "original-pod" || text(options, "preconditions", "resourceVersion") != "9" {
							t.Fatal("delete omitted exact resource preconditions")
						}
						deletes++
						missing = true
						if scenario == "delete response lost" {
							return nil, errors.New("response lost")
						}
						return nil, nil
					case "get":
						if args[3] == "configmap" {
							cm := resource("longhorn-v2-benchmark-v1")
							raw, _ := json.Marshal(stored)
							cm["data"] = object{"record": string(raw)}
							return json.Marshal(cm)
						}
						if missing && (scenario != "replacement during absence check" || patches == 0) {
							return nil, nil
						}
						pod := resource("fio")
						pod["metadata"].(object)["uid"] = "original-pod"
						pod["metadata"].(object)["resourceVersion"] = "9"
						if scenario == "replacement before delete" || scenario == "replacement during absence check" {
							pod["metadata"].(object)["uid"] = "foreign-pod"
						}
						return json.Marshal(pod)
					}
				}
				t.Fatal("unexpected command")
				return nil, errors.New("unexpected command")
			}}
			err := removeRecoveredBenchmarkWorkload(context.Background(), c, &record, "Pod")
			if scenario == "normal" {
				if err != nil || deletes != 1 {
					t.Fatalf("normal cleanup failed: %v deletes=%d", err, deletes)
				}
			} else {
				if err == nil {
					t.Fatal("unproven deletion accepted")
				}
				if scenario == "delete response lost" {
					if deletes != 1 {
						t.Fatal("expected one owned delete")
					}
					if err := removeRecoveredBenchmarkWorkload(context.Background(), c, &record, "Pod"); err != nil || deletes != 1 {
						t.Fatalf("known intent could not resume absence without a second delete: %v", err)
					}
				} else if deletes != 0 {
					t.Fatal("unsafe case reached delete")
				}
			}
		})
	}
}
