package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDatabaseProofRejectsReadOnlySourcesWithoutAnArchive(t *testing.T) {
	for _, name := range []string{"kagent-db", "substrate-db"} {
		t.Run(name, func(t *testing.T) {
			c := client{run: func(context.Context, []byte, string, ...string) ([]byte, error) {
				t.Fatal("an unarchived database reached a recovery command")
				return nil, nil
			}}
			if _, err := beginCNPGProof(context.Background(), c, strings.Repeat("a", 40), "1234", name); err == nil {
				t.Fatal("an unarchived database was admitted to archive recovery")
			}
		})
	}
}

// TestDatabaseProofInitializationPersistsTheArchiveBeforeCreatingAnyBackup verifies the first write.
func TestDatabaseProofInitializationPersistsTheArchiveBeforeCreatingAnyBackup(t *testing.T) {
	c, r, _ := recoveryPlanningFixture(t)
	delegate := c.run
	createdCount := 0
	var liveCM object
	c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "configmap" {
			if liveCM == nil {
				return nil, nil
			}
			return json.Marshal(liveCM)
		}
		if program == "kubectl" && len(args) > 2 && args[2] == "create" {
			createdCount++
			var cm object
			if json.Unmarshal(input, &cm) != nil || text(cm, "kind") != "ConfigMap" || text(cm, "metadata", "namespace") != "longhorn-system" {
				t.Fatal("unexpected initial proof write")
			}
			var record cnpgProofRecord
			if json.Unmarshal([]byte(text(cm, "data", "record")), &record) != nil || record.Phase != "planned" || record.Archive != r.Archive || record.Backup.UID != "" {
				t.Fatal("initial archive witness was not persisted")
			}
			cm["metadata"].(object)["uid"] = "journal-uid"
			cm["metadata"].(object)["resourceVersion"] = "10"
			liveCM = cm
			return json.Marshal(cm)
		}
		return delegate(ctx, input, program, args...)
	}
	p, err := beginCNPGProof(context.Background(), c, strings.Repeat("a", 40), "1234", "umami-db")
	if err != nil {
		t.Fatal(err)
	}
	if p.journalUID != "journal-uid" || createdCount != 1 {
		t.Fatal("initial native journal identity was lost")
	}
}
