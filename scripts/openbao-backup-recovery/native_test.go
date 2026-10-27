package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestPinnedServerRestoresSnapshotAndRejectsInvalidRecovery exercises actual Raft state.
func TestPinnedServerRestoresSnapshotAndRejectsInvalidRecovery(t *testing.T) {
	if os.Getenv("OPENBAO_BACKUP_RECOVERY_NATIVE_TEST") != "1" {
		t.Skip("actual pinned server proof runs in Linux CI")
	}
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Fatal("native proof requires the non-root Linux CI host")
	}
	if out, err := exec.Command("findmnt", "-n", "-o", "FSTYPE", "--target", "/dev/shm").Output(); err != nil || strings.TrimSpace(string(out)) != "tmpfs" {
		t.Fatal("native fixture must remain in memory")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	binary := filepath.Join(t.TempDir(), "verifier")
	build := exec.Command("go", "build", "-o", binary, "./scripts/openbao-backup-recovery")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("native verifier build failed: %v %s", err, out)
	}
	fixture, err := os.MkdirTemp("/dev/shm", "openbao-recovery-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fixture) })
	run := func(mode, dir string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 270*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", filepath.Join(root, "scripts/run-openbao-backup-sandbox.sh"), mode, dir, binary)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("fixture", fixture); err != nil || strings.TrimSpace(out) != "OPENBAO_BACKUP_FIXTURE=PASS" {
		t.Fatalf("native synthetic snapshot creation failed: %v %s", err, out)
	}
	for _, test := range []string{"matching", "corrupt-snapshot", "different-record", "wrong-unseal-key"} {
		t.Run(test, func(t *testing.T) {
			dir, err := os.MkdirTemp("/dev/shm", "openbao-recovery-case-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			for _, name := range []string{"root-token", "unseal-key", "expected.json", "snapshot.snap"} {
				b, err := privateInput(filepath.Join(fixture, name), maxSnapshotBytes)
				if err != nil || writePrivate(filepath.Join(dir, name), b) != nil {
					t.Fatal("could not prepare private synthetic handoff")
				}
			}
			switch test {
			case "corrupt-snapshot":
				b, err := os.ReadFile(filepath.Join(dir, "snapshot.snap"))
				if err != nil || len(b) == 0 {
					t.Fatal("missing synthetic snapshot")
				}
				b[0] ^= 0xff
				if os.WriteFile(filepath.Join(dir, "snapshot.snap"), b, 0600) != nil {
					t.Fatal("could not corrupt synthetic snapshot")
				}
			case "different-record":
				b, err := os.ReadFile(filepath.Join(dir, "expected.json"))
				var records map[string]json.RawMessage
				if err != nil || json.Unmarshal(b, &records) != nil {
					t.Fatal("invalid synthetic records")
				}
				records[credentialPaths[1]] = json.RawMessage(`{"username":"changed-synthetic-user","password":"changed-synthetic-value","retainedField":"synthetic-fixture"}`)
				b, err = json.Marshal(records)
				if err != nil || os.WriteFile(filepath.Join(dir, "expected.json"), b, 0600) != nil {
					t.Fatal("could not change synthetic expected record")
				}
			case "wrong-unseal-key":
				if os.WriteFile(filepath.Join(dir, "unseal-key"), []byte(strings.Repeat("A", 43)+"="), 0600) != nil {
					t.Fatal("could not replace synthetic recovery key")
				}
			}
			out, err := run("verify", dir)
			if test == "matching" {
				if err != nil || strings.TrimSpace(out) != "OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3" {
					t.Fatalf("actual snapshot restoration not proven: %v %s", err, out)
				}
			} else {
				stage := map[string]string{"corrupt-snapshot": "restore", "different-record": "records", "wrong-unseal-key": "unseal"}[test]
				if err == nil || strings.TrimSpace(out) != "OPENBAO_BACKUP_RESTORE=FAIL stage="+stage {
					t.Fatalf("native refusal did not prove its intended failure: %v %s", err, out)
				}
			}
		})
	}
}
