package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeRestoreReclaimProtectsBothKubernetesReferences(t *testing.T) {
	record, seed, state := nativeRestoreStateFixture(t, "completed")
	policies, err := nativeRestoreReclaimFences(record, seed, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 2 {
		t.Fatal("the original PV and PVC both need reference policies")
	}
	fixture, err := json.Marshal(policies)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "native-restore-reclaim.json")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-count=1", "-timeout=2m", "-run", "^TestNativeRestoreReclaimReferenceDecisions$", ".")
	cmd.Dir = "testdata/retirement-fence-proof"
	cmd.Env = append(os.Environ(), "LONGHORN_RESTORE_RECLAIM_FENCE_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native reference admission proof failed: %v\n%s", err, output)
	}
}
