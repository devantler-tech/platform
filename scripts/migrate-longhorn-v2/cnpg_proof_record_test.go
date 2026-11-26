package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// TestFreshBackupRevalidatesTheCompleteArchiveBeforeIntent refuses changed plugin configuration.
func TestFreshBackupRevalidatesTheCompleteArchiveBeforeIntent(t *testing.T) {
	for name, mutate := range map[string]func(object, object){
		"credential": func(_, s object) {
			value(s, "spec", "configuration", "s3Credentials", "accessKeyId").(object)["name"] = "foreign"
		},
		"database":    func(c, _ object) { value(c, "spec", "bootstrap", "initdb").(object)["database"] = "foreign" },
		"archiver":    func(c, _ object) { value(c, "spec", "plugins").([]any)[0].(object)["isWALArchiver"] = false },
		"compression": func(_, s object) { value(s, "spec", "configuration", "wal").(object)["compression"] = "none" },
	} {
		t.Run(name, func(t *testing.T) {
			cluster, store, _ := recoverySourceFixture("umami-db")
			record := cnpgProofRecord{Version: 1, Source: strings.Repeat("a", 40), Run: "1234", Target: "umami-db", Phase: "planned", Archive: archiveBindingFixture(cluster, store)}
			mutate(cluster, store)
			reachedIntent := false
			p := cnpgProofRun{record: record, c: client{run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				if program == "kubectl" && len(args) > 3 && args[2] == "get" {
					switch args[3] {
					case "cluster.postgresql.cnpg.io":
						return json.Marshal(cluster)
					case "objectstores.barmancloud.cnpg.io":
						return json.Marshal(store)
					}
				}
				reachedIntent = true
				return nil, errors.New("command after invalid archive")
			}}}
			if p.freshBackup(context.Background()) == nil || reachedIntent {
				t.Fatal("changed archive reached backup intent")
			}
		})
	}
}

// TestFreshDatabaseBackupPersistsIntentBeforeCreationAndTheReturnedUIDAfterward checks ordering.
func TestFreshDatabaseBackupPersistsIntentBeforeCreationAndTheReturnedUIDAfterward(t *testing.T) {
	cluster, store, _ := recoverySourceFixture("umami-db")
	record := cnpgProofRecord{Version: 1, Source: strings.Repeat("a", 40), Run: "1234", Target: "umami-db", Phase: "planned", Archive: archiveBindingFixture(cluster, store), Resources: map[string]benchmarkResource{}}
	cm := ownedCNPGJournalFixture(t, record)
	sequence := []string{}
	version := 1
	p := cnpgProofRun{record: record, journalUID: text(cm, "metadata", "uid")}
	p.c = client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "git" {
			return []byte(record.Source + "\trefs/heads/main\n"), nil
		}
		if program != "kubectl" || len(args) < 3 {
			t.Fatal("unexpected executable")
		}
		switch args[2] {
		case "get":
			switch args[3] {
			case "configmap":
				return json.Marshal(cm)
			case "cluster.postgresql.cnpg.io":
				return json.Marshal(cluster)
			case "objectstores.barmancloud.cnpg.io":
				return json.Marshal(store)
			default:
				t.Fatal("unexpected recovery record read")
			}
		case "patch":
			var operations []object
			if json.Unmarshal(input, &operations) != nil || len(operations) != 3 || text(operations[0], "path") != "/metadata/uid" || operations[0]["value"] != p.journalUID || text(operations[1], "path") != "/metadata/resourceVersion" || operations[1]["value"] != text(cm, "metadata", "resourceVersion") || text(operations[2], "path") != "/data/record" {
				t.Fatal("journal update lacks native CAS")
			}
			var next cnpgProofRecord
			if json.Unmarshal([]byte(text(operations[2], "value")), &next) != nil {
				t.Fatal("invalid journal payload")
			}
			sequence = append(sequence, next.Phase)
			cm["data"].(object)["record"] = text(operations[2], "value")
			version++
			cm["metadata"].(object)["resourceVersion"] = strconv.Itoa(version)
			return []byte("configmap/proof\n"), nil
		case "create":
			var current cnpgProofRecord
			if json.Unmarshal([]byte(text(cm, "data", "record")), &current) != nil || current.Phase != "backup-requested" || current.Backup.UID != "" {
				t.Fatal("backup created before durable intent")
			}
			var backup object
			if json.Unmarshal(input, &backup) != nil || text(backup, "kind") != "Backup" || text(backup, "spec", "cluster", "name") != "umami-db" || text(backup, "spec", "method") != "plugin" || text(backup, "spec", "target") != "primary" {
				t.Fatal("unexpected backup mutation")
			}
			sequence = append(sequence, "create")
			backup["metadata"].(object)["uid"] = "recorded-backup-uid"
			backup["metadata"].(object)["resourceVersion"] = "10"
			return json.Marshal(backup)
		default:
			t.Fatal("unexpected production mutation")
		}
		return nil, nil
	}}
	if err := p.freshBackup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sequence, ",") != "backup-requested,create,backup-created" || p.record.Backup.UID != "recorded-backup-uid" {
		t.Fatalf("backup witness ordering lost: %v", sequence)
	}
}

// TestFreshDatabaseBackupCannotReplayAnUncertainCreation stops before any second command.
func TestFreshDatabaseBackupCannotReplayAnUncertainCreation(t *testing.T) {
	cluster, store, _ := recoverySourceFixture("umami-db")
	p := cnpgProofRun{record: cnpgProofRecord{Version: 1, Source: strings.Repeat("a", 40), Run: "1234", Target: "umami-db", Phase: "backup-requested", Archive: archiveBindingFixture(cluster, store)}, c: client{run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("uncertain creation reached another command")
		return nil, nil
	}}}
	if p.freshBackup(context.Background()) == nil {
		t.Fatal("uncertain backup creation was replayed")
	}
}

// TestDatabaseProofJournalCannotChangeAnEstablishedWitness protects recovery ownership.
func TestDatabaseProofJournalCannotChangeAnEstablishedWitness(t *testing.T) {
	cluster, store, _ := recoverySourceFixture("umami-db")
	before := cnpgProofRecord{Version: 1, Source: strings.Repeat("a", 40), Run: "1234", Target: "umami-db", Archive: archiveBindingFixture(cluster, store), Backup: benchmarkResource{Name: "backup", Namespace: "umami", UID: "backup-uid"}, Resources: map[string]benchmarkResource{"Namespace": {Name: "proof", UID: "namespace-uid"}}}
	for name, change := range map[string]func(*cnpgProofRecord){
		"source":    func(r *cnpgProofRecord) { r.Source = strings.Repeat("b", 40) },
		"run":       func(r *cnpgProofRecord) { r.Run = "5678" },
		"target":    func(r *cnpgProofRecord) { r.Target = "coroot-db" },
		"archive":   func(r *cnpgProofRecord) { r.Archive.StoreUID = "another-store" },
		"backup":    func(r *cnpgProofRecord) { r.Backup.UID = "replacement" },
		"namespace": func(r *cnpgProofRecord) { r.Resources = map[string]benchmarkResource{} },
	} {
		t.Run(name, func(t *testing.T) {
			after := before
			change(&after)
			if unchangedCNPGProofInputs(before, after) {
				t.Fatal("established proof input changed")
			}
		})
	}
}
