package scansnapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = environment()
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fixture: %v: %s", err, data)
	}
}

func source(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-q")
	for name, value := range map[string]string{"go.mod": "module snapshot.example/control\n\ngo 1.27.3\n", "main.go": "package main\nfunc main() {}\n", ".gitattributes": "main.go export-ignore\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "add", "--", "go.mod", "main.go", ".gitattributes")
	git(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgSign=false", "commit", "-qm", "fixture")
	return root
}

func TestSnapshotKeepsAllHeadFilesAndIsIndependentOfWorkingChanges(t *testing.T) {
	root := source(t)
	snapshot, cleanup, err := Clone(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(snapshot.Root, "main.go"))
	if err != nil || string(data) != "package main\nfunc main() {}\n" {
		t.Fatalf("snapshot lost HEAD input: %s, %v", data, err)
	}
	if snapshot.Head == "" || len(snapshot.Files) != 3 {
		t.Fatalf("incomplete source receipt: %+v", snapshot)
	}
	git(t, snapshot.Root, "diff", "--exit-code", "HEAD")
}

func TestSnapshotRefusesTrackedLinksAndUnbornOrMissingSources(t *testing.T) {
	root := source(t)
	if err := os.Symlink("/outside-source", filepath.Join(root, "linked.go")); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "--", "linked.go")
	git(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgSign=false", "commit", "-qm", "linked")
	for _, path := range []string{root, t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		_, cleanup, err := Clone(context.Background(), path)
		if cleanup != nil {
			cleanup()
		}
		if err == nil {
			t.Fatalf("accepted unsupported source %s", path)
		}
	}
}
