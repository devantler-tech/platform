package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ownedCNPGJournalFixture mirrors the ConfigMap created by beginCNPGProof.
func ownedCNPGJournalFixture(t *testing.T, record cnpgProofRecord) object {
	t.Helper()
	body, err := encodeCNPGProofRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	cm := resource("longhorn-v2-db-proof-" + record.Target)
	cm["apiVersion"], cm["kind"] = "v1", "ConfigMap"
	cm["metadata"].(object)["namespace"] = "longhorn-system"
	cm["metadata"].(object)["labels"] = object{"app.kubernetes.io/managed-by": "github-actions", "storage.platform.devantler.tech/recovery-run": record.Run}
	cm["data"] = object{"record": body}
	return cm
}

// TestDatabaseJournalRefusesUnownedMetadataBeforeMutation catches a journal
// transition trusting the payload while ignoring its native ownership envelope.
func TestDatabaseJournalRefusesUnownedMetadataBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"valid", "foreign manager", "foreign run", "missing labels", "deleting", "wrong kind", "wrong API version", "unsupported schema", "invalid run", "unreviewed target"} {
		t.Run(scenario, func(t *testing.T) {
			cluster, store, _ := recoverySourceFixture("umami-db")
			r := cnpgProofRecord{Version: 1, Source: strings.Repeat("a", 40), Run: "1234", Target: "umami-db", Phase: "planned", Archive: archiveBindingFixture(cluster, store), Resources: map[string]benchmarkResource{}}
			switch scenario {
			case "unsupported schema":
				r.Version = 2
			case "invalid run":
				r.Run = "not-a-native-run"
			case "unreviewed target":
				r.Target = "foreign-db"
			}
			cm := ownedCNPGJournalFixture(t, r)
			p := cnpgProofRun{record: r, journalUID: text(cm, "metadata", "uid")}
			p.record.Phase = "backup-requested"
			p.record.Backup = benchmarkResource{Name: "longhorn-v2-proof-" + r.Target + "-" + r.Run, Namespace: "umami"}
			switch scenario {
			case "foreign manager":
				value(cm, "metadata", "labels").(object)["app.kubernetes.io/managed-by"] = "another-controller"
			case "foreign run":
				value(cm, "metadata", "labels").(object)["storage.platform.devantler.tech/recovery-run"] = "5678"
			case "missing labels":
				delete(cm["metadata"].(object), "labels")
			case "deleting":
				cm["metadata"].(object)["deletionTimestamp"] = "2026-11-12T00:00:00Z"
			case "wrong kind":
				cm["kind"] = "Secret"
			case "wrong API version":
				cm["apiVersion"] = "foreign/v1"
			}
			writes := 0
			p.c = client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(r.Source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 4 {
					return nil, errors.New("unexpected journal executable")
				}
				if args[2] == "get" && args[3] == "configmap" {
					return json.Marshal(cm)
				}
				if args[2] == "patch" && args[3] == "configmap" {
					writes++
					var ops []object
					if json.Unmarshal(input, &ops) != nil || len(ops) != 3 || text(ops[2], "path") != "/data/record" {
						return nil, errors.New("unexpected journal patch")
					}
					cm["data"].(object)["record"] = text(ops[2], "value")
					return []byte("patched"), nil
				}
				return nil, errors.New("unexpected journal operation")
			}}
			err := p.save(t.Context())
			if scenario == "valid" {
				if err != nil || writes != 1 {
					t.Fatalf("owned journal refused: err=%v writes=%d", err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatalf("unowned journal reached mutation: err=%v writes=%d", err, writes)
			}
		})
	}
}
