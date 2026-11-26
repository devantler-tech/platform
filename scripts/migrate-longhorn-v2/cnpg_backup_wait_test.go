package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestBackupWaitRequiresTheOriginalCompletedNativeObject checks the identity boundary.
func TestBackupWaitRequiresTheOriginalCompletedNativeObject(t *testing.T) {
	for _, mutation := range []string{"completed", "replacement", "failed", "foreign-owner"} {
		t.Run(mutation, func(t *testing.T) {
			_, _, backup := recoverySourceFixture("umami-db")
			backup["metadata"].(object)["name"] = "longhorn-v2-proof-umami-db-1234"
			backup["metadata"].(object)["labels"] = object{"storage.platform.devantler.tech/recovery-run": "1234", "app.kubernetes.io/managed-by": "github-actions"}
			expected := benchmarkIdentity(backup)
			switch mutation {
			case "replacement":
				backup["metadata"].(object)["uid"] = "replacement"
			case "failed":
				backup["status"].(object)["phase"] = "failed"
			case "foreign-owner":
				value(backup, "metadata", "labels").(object)["storage.platform.devantler.tech/recovery-run"] = "9999"
			}
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program != "kubectl" || len(input) != 0 || len(args) < 5 || args[2] != "get" || args[3] != "backups.postgresql.cnpg.io" || args[4] != expected.Name {
					t.Fatal("backup observation escaped its read-only scope")
				}
				return json.Marshal(backup)
			}}
			completed, err := waitCNPGBackup(context.Background(), c, "umami-db", "1234", expected)
			if mutation == "completed" {
				if err != nil || benchmarkIdentity(completed) != expected {
					t.Fatal("native completed backup refused", err)
				}
			} else if err == nil {
				t.Fatal("unbound or failed backup accepted")
			}
		})
	}
}

// TestBackupCompletionTimeoutReturnsNoWitness rejects a late positive callback.
func TestBackupCompletionTimeoutReturnsNoWitness(t *testing.T) {
	_, _, backup := recoverySourceFixture("umami-db")
	backup["metadata"].(object)["name"] = "longhorn-v2-proof-umami-db-1234"
	backup["metadata"].(object)["labels"] = object{"storage.platform.devantler.tech/recovery-run": "1234", "app.kubernetes.io/managed-by": "github-actions"}
	c := client{context: "admin@prod", run: func(ctx context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return json.Marshal(backup)
	}}
	ctx, stop := context.WithTimeout(context.Background(), time.Millisecond)
	defer stop()
	completed, err := waitCNPGBackup(ctx, c, "umami-db", "1234", benchmarkIdentity(backup))
	if err == nil || completed != nil {
		t.Fatal("late completion exposed a usable backup witness")
	}
}
