package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestDatabaseJournalRefusesAmbiguousEvidenceBeforeMutation catches a typed
// decoder silently accepting duplicate, case-aliased, unknown or oversized data.
func TestDatabaseJournalRefusesAmbiguousEvidenceBeforeMutation(t *testing.T) {
	for name, corrupt := range map[string]func(string) string{
		"valid": func(s string) string { return s },
		"duplicate source": func(s string) string {
			return strings.Replace(s, `"Source":`, `"Source":"`+strings.Repeat("b", 40)+`","Source":`, 1)
		},
		"case alias source": func(s string) string { return strings.Replace(s, `"Source":`, `"source":`, 1) },
		"unknown root":      func(s string) string { return strings.Replace(s, `{`, `{"Unreviewed":true,`, 1) },
		"duplicate archive": func(s string) string {
			return strings.Replace(s, `"ClusterUID":`, `"ClusterUID":"foreign","ClusterUID":`, 1)
		},
		"case alias archive": func(s string) string {
			return strings.Replace(s, `"ClusterUID":`, `"clusteruid":`, 1)
		},
		"unknown archive": func(s string) string { return strings.Replace(s, `"Archive":{`, `"Archive":{"Unreviewed":true,`, 1) },
		"oversized":       func(s string) string { return s + strings.Repeat(" ", 65536) },
		"trailing value":  func(s string) string { return s + `{}` },
	} {
		t.Run(name, func(t *testing.T) {
			cluster, store, _ := recoverySourceFixture("umami-db")
			record := cnpgProofRecord{Version: 1, Source: strings.Repeat("a", 40), Run: "1234", Target: "umami-db", Phase: "planned", Archive: archiveBindingFixture(cluster, store), Resources: map[string]benchmarkResource{}}
			body, err := encodeCNPGProofRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			cm := ownedCNPGJournalFixture(t, record)
			cm["data"] = object{"record": corrupt(body)}
			writes := 0
			p := cnpgProofRun{record: record, journalUID: text(cm, "metadata", "uid")}
			p.record.Phase = "backup-requested"
			p.record.Backup = benchmarkResource{Name: "longhorn-v2-proof-umami-db-1234", Namespace: "umami"}
			p.c = client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(record.Source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 4 {
					return nil, errors.New("unexpected journal command")
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
					return []byte("configmap/proof\n"), nil
				}
				return nil, errors.New("unexpected journal operation")
			}}
			err = p.save(t.Context())
			if name == "valid" {
				if err != nil || writes != 1 {
					t.Fatalf("valid owned transition refused: %v writes=%d", err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatalf("ambiguous recovery evidence reached mutation: err=%v writes=%d", err, writes)
			}
		})
	}
}

// TestDatabaseJournalRefusesAliasedWitnesses prevents Go's case-insensitive
// struct decoding from silently changing the meaning of nested native receipts.
func TestDatabaseJournalRefusesAliasedWitnesses(t *testing.T) {
	p, cnpgPin, barmanPin, _ := databaseCreationFixture(t, "confirmed")
	if _, err := p.createDatabaseResourceWithSchemas(t.Context(), "ObjectStore", cnpgPin, barmanPin); err != nil {
		t.Fatal(err)
	}
	body, err := encodeCNPGProofRecord(p.record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCNPGProofRecord([]byte(body)); err != nil {
		t.Fatal("complete native-shaped journal refused:", err)
	}
	var generic object
	if err := json.Unmarshal([]byte(body), &generic); err != nil {
		t.Fatal(err)
	}
	resources := value(generic, "Plan", "Resources").([]any)
	value(resources[0].(object), "metadata").(object)["annotations"] = object{"Source": "allowed application metadata", "nodeuid": "allowed annotation"}
	withMetadata, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCNPGProofRecord(withMetadata); err != nil {
		t.Fatal("arbitrary Kubernetes metadata was treated as a typed witness:", err)
	}
	for _, path := range [][]any{
		{"Backup", "UID"}, {"Resources", "Namespace", "UID"},
		{"Plan", "Source", "Server"}, {"Plan", "Barrier", "system_id"},
		{"Preparation", "Agent", "NodeUID"}, {"Preparation", "Runtime", "Version"},
		{"Credential", "Source", "Name"},
		{"Admission", "CNPG", "CRD", "UID"},
		{"Admission", "CNPG", "Operators", 0, "Node"},
		{"Admission", "CNPG", "Defaults", "Version"},
		{"Admission", "CNPG", "Startup", 0, "LoadedAt"},
		{"Admission", "CNPG", "Startup", 0, "OperatorPullSecretName"},
		{"Admission", "CNPG", "Startup", 0, "CreateAnyService"},
		{"Admission", "CNPG", "Startup", 0, "Defaults", "Version"},
		{"Admission", "Barman", "CRD", "UID"},
		{"Admission", "Barman", "Operator", "Node"},
		{"Storage", "Activation", "UID"},
		{"Storage", "Targets", "temporary-v1-1", "nodeUID"},
		{"Storage", "Disks", 0, "DiskUUID"},
	} {
		var doc object
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatal(err)
		}
		var at any = doc
		for _, part := range path[:len(path)-1] {
			if index, ok := part.(int); ok {
				at = at.([]any)[index]
			} else {
				at = at.(object)[part.(string)]
			}
		}
		field := path[len(path)-1].(string)
		alias := strings.ToLower(field)
		if alias == field {
			alias = strings.ToUpper(field)
		}
		value, present := at.(object)[field]
		if !present {
			t.Fatalf("missing native-shaped field at %v", path)
		}
		delete(at.(object), field)
		at.(object)[alias] = value
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeCNPGProofRecord(raw); err == nil {
			t.Errorf("case-aliased native witness accepted at %v", path)
		}
	}
	var conflicting object
	if err := json.Unmarshal([]byte(body), &conflicting); err != nil {
		t.Fatal(err)
	}
	startup := value(conflicting, "Admission", "CNPG", "Startup").([]any)[0].(object)
	startup["CreateAnyService"] = false
	startup["createanyservice"] = true
	raw, err := json.Marshal(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCNPGProofRecord(raw); err == nil {
		t.Error("conflicting canonical and case-aliased startup flag accepted")
	}
}

// TestDatabaseJournalRejectsUnreviewedTypedMapKeys catches map entries that a
// struct decoder accepts even with DisallowUnknownFields enabled.
func TestDatabaseJournalRejectsUnreviewedTypedMapKeys(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{"Resources":{"Foreign":{"Name":"other","Namespace":"other","UID":"other"}}}`,
		`{"Storage":{"Targets":{"foreign":{"name":"other"}}}}`,
	} {
		if _, err := decodeCNPGProofRecord([]byte(body)); err == nil {
			t.Errorf("unreviewed journal shape accepted: %s", body)
		}
	}
}
