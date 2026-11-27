package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// recoveryPlanningFixture combines the recorded archive with healthy native-shaped processes.
func recoveryPlanningFixture(t *testing.T) (client, cnpgRecoveryRequest, *object) {
	t.Helper()
	observation := cnpgObservationFixture(t)
	cluster, store, backup := recoverySourceFixture("umami-db")
	cluster["spec"].(object)["storage"].(object)["storageClass"] = "longhorn-wffc"
	cluster["status"] = observation.Cluster["status"]
	observation.Cluster = cluster
	backup["metadata"].(object)["name"] = "longhorn-v2-proof-umami-db-1234"
	backup["metadata"].(object)["labels"] = object{"storage.platform.devantler.tech/recovery-run": "1234", "app.kubernetes.io/managed-by": "github-actions"}
	backup["status"].(object)["beginLSN"] = "2/80"
	backup["status"].(object)["endLSN"] = "2/FF"
	value(backup, "status", "pluginMetadata").(object)["timeline"] = "7"
	r := cnpgRecoveryRequest{Target: "umami-db", Source: strings.Repeat("a", 40), Run: "1234", Hostname: "qualified-worker", Archive: archiveBindingFixture(cluster, store), Backup: benchmarkIdentity(backup)}
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if len(input) != 0 {
			t.Fatal("recovery planning attempted a write")
		}
		if program == "git" {
			return []byte(r.Source + "\trefs/heads/main\n"), nil
		}
		if program != "kubectl" || len(args) < 3 {
			t.Fatal("unexpected executable")
		}
		if args[2] == "exec" {
			return json.Marshal(observation.States[args[3]])
		}
		if args[2] != "get" {
			t.Fatal("recovery planning attempted a mutation")
		}
		switch args[3] {
		case "backups.postgresql.cnpg.io":
			return json.Marshal(backup)
		case "objectstores.barmancloud.cnpg.io":
			return json.Marshal(store)
		case "cluster.postgresql.cnpg.io":
			return json.Marshal(cluster)
		case "pods":
			return json.Marshal(object{"metadata": object{"resourceVersion": "10"}, "items": observation.Pods})
		case "pod":
			for _, pod := range observation.Pods {
				if text(pod, "metadata", "name") == args[4] {
					return json.Marshal(pod)
				}
			}
		}
		return nil, errors.New("unexpected native read")
	}}
	return c, r, &backup
}

// TestRecoveryPlanningBindsTheFreshBackupBeforeRenderingAV2Restore exercises actual client reads.
func TestRecoveryPlanningBindsTheFreshBackupBeforeRenderingAV2Restore(t *testing.T) {
	c, r, _ := recoveryPlanningFixture(t)
	plan, err := planCNPGRecovery(context.Background(), c, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Resources) != 4 || plan.Source.BackupUID != r.Backup.UID || text(plan.Resources[3], "spec", "storage", "storageClass") != "longhorn-v2" || text(plan.Resources[3], "spec", "bootstrap", "recovery", "recoveryTarget", "targetLSN") != plan.Barrier.FlushLSN {
		t.Fatal("restore lost its native backup, engine or post-backup WAL binding")
	}
}

// TestRecoveryPlanningRefusesAReplacedOrIncompleteBackup preserves the recorded witness.
func TestRecoveryPlanningRefusesAReplacedOrIncompleteBackup(t *testing.T) {
	for name, change := range map[string]func(object){
		"replaced":   func(b object) { b["metadata"].(object)["uid"] = "replacement" },
		"unfinished": func(b object) { b["status"].(object)["phase"] = "running" },
		"another owner": func(b object) {
			value(b, "metadata", "labels").(object)["storage.platform.devantler.tech/recovery-run"] = "9999"
		},
		"target before backup end": func(b object) { b["status"].(object)["endLSN"] = "2/200" },
	} {
		t.Run(name, func(t *testing.T) {
			c, r, b := recoveryPlanningFixture(t)
			change(*b)
			if _, err := planCNPGRecovery(context.Background(), c, r); err == nil {
				t.Fatal("unbound recovery plan accepted")
			}
		})
	}
}
