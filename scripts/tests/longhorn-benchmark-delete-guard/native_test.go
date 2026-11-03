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
	// Hosted runners compile the Kubernetes admission dependencies from a cold
	// cache. Keep that setup separate from the native proof's execution budget;
	// both phases together remain below CI's 20-minute per-package deadline.
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancelBuild()
	render := exec.CommandContext(buildCtx, "kubectl", "kustomize", "../../../k8s/providers/hetzner/infrastructure/controllers/longhorn")
	raw, err := render.Output()
	if err != nil {
		t.Fatal("production Longhorn controller profile could not be rendered")
	}
	work := t.TempDir()
	profile := filepath.Join(work, "profile.yaml")
	if err := os.WriteFile(profile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(work, "native-proof.test")
	build := exec.CommandContext(buildCtx, "go", "test", "-c", "-mod=readonly", "-o", binary, ".")
	build.Dir = "testdata/native-proof"
	build.WaitDelay = 10 * time.Second
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("native Kubernetes admission proof build failed: %v: %s", err, output)
	}
	cancelBuild()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.count=1", "-test.timeout=2m")
	command.Dir = build.Dir
	command.WaitDelay = 10 * time.Second
	command.Env = append(os.Environ(), "LONGHORN_GUARD_RENDER_PATH="+profile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native Kubernetes admission proof failed: %v: %s", err, output)
	}
}
