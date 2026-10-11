package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func privateSnapshotFixture(t *testing.T) object {
	t.Helper()
	s, err := testEngine(fakeFixture()).collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range s.backups {
		b["status"].(object)["stoppedAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	}
	o := object{"release": s.release, "deployment": s.deployment, "backupRelease": s.backupRelease}
	for name, list := range map[string][]object{"clusters": s.clusters, "pods": s.pods, "claims": s.claims, "pvs": s.pvs, "volumes": s.volumes, "backups": s.backups, "replicasets": s.replicasets, "schedules": s.schedules} {
		o[name] = object{"items": itemsAsAny(list)}
	}
	extra := []any{}
	for _, w := range s.extra {
		extra = append(extra, object{"release": w.Release, "deployment": w.Deployment})
	}
	o["extra"] = extra
	return o
}

func TestCompletePrivateSnapshotReachesReadOnlyPlan(t *testing.T) {
	b, err := json.Marshal(privateSnapshotFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LONGHORN_PRIVATE_PREFLIGHT_SNAPSHOT", path)
	TestPrivateReadOnlyPreflightSnapshot(t)
}

func TestPrivateSnapshotRetainsBackupSafetyChecks(t *testing.T) {
	for _, variation := range []string{"paused-schedule", "foreign-backup-owner", "missing-backup-release", "malformed-release", "missing-schedules", "malformed-schedules"} {
		t.Run(variation, func(t *testing.T) {
			o := privateSnapshotFixture(t)
			switch variation {
			case "paused-schedule":
				items(o, "schedules", "items")[0]["spec"].(object)["suspend"] = true
			case "foreign-backup-owner":
				val(o, "backupRelease", "metadata").(object)["labels"] = object{}
			case "missing-backup-release":
				delete(o, "backupRelease")
			case "malformed-release":
				o["release"] = "not an object"
			case "missing-schedules":
				delete(o, "schedules")
			case "malformed-schedules":
				o["schedules"] = object{"items": []any{"not an object"}}
			}
			b, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			s, err := decodePrivateSnapshot(b)
			if err == nil {
				_, err = buildPlan(s, time.Now())
			}
			if err == nil {
				t.Fatal("unsafe or incomplete private snapshot admitted")
			}
		})
	}
}

// Explicit, offline replay of a privately captured OIDC read-only inventory.
// This test performs no API call or write, and publishes no resource payload.
func TestPrivateReadOnlyPreflightSnapshot(t *testing.T) {
	path := os.Getenv("LONGHORN_PRIVATE_PREFLIGHT_SNAPSHOT")
	if path == "" {
		t.Skip("no private OIDC snapshot supplied")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := decodePrivateSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	p, err := buildPlan(s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read-only preflight: %d clusters, %d retained claims, %d backing volumes", len(p.clusters), len(p.claims), len(p.volumes))
}

func decodePrivateSnapshot(b []byte) (inventory, error) {
	var o object
	if err := json.Unmarshal(b, &o); err != nil {
		return inventory{}, err
	}
	s := inventory{}
	for name, dst := range map[string]*[]object{
		"clusters": &s.clusters, "pods": &s.pods, "claims": &s.claims, "pvs": &s.pvs,
		"volumes": &s.volumes, "backups": &s.backups, "replicasets": &s.replicasets,
		"schedules": &s.schedules,
	} {
		raw, ok := val(o, name, "items").([]any)
		list := items(o, name, "items")
		if !ok || len(list) != len(raw) {
			return inventory{}, fmt.Errorf("%s inventory is missing or malformed", name)
		}
		*dst = list
	}
	s.release, _ = val(o, "release").(map[string]any)
	s.deployment, _ = val(o, "deployment").(map[string]any)
	s.backupRelease, _ = val(o, "backupRelease").(map[string]any)
	if s.release == nil || s.deployment == nil || s.backupRelease == nil {
		return inventory{}, errors.New("consumer or backup release coverage is malformed")
	}
	for _, w := range items(o, "extra") {
		r, rok := val(w, "release").(map[string]any)
		d, dok := val(w, "deployment").(map[string]any)
		if !rok || !dok {
			return inventory{}, errors.New("additional consumer coverage is malformed")
		}
		s.extra = append(s.extra, workload{r, d})
	}
	return s, nil
}
