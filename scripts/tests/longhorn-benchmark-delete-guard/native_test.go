package benchmarkguard_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeBenchmarkDeletionGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	render := exec.CommandContext(ctx, "kubectl", "kustomize", "../../../k8s/providers/hetzner/infrastructure/controllers/longhorn")
	raw, err := render.Output()
	if err != nil {
		t.Fatal("production Longhorn controller profile could not be rendered")
	}
	profile := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(profile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-count=1", "-timeout=2m", ".")
	command.Dir = "testdata/native-proof"
	command.Env = append(os.Environ(), "LONGHORN_GUARD_RENDER_PATH="+profile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native Kubernetes admission proof failed: %v: %s", err, output)
	}
}
