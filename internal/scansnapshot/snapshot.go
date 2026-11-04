// Package scansnapshot binds native source analysis and scanner inputs to one
// private checkout of the exact immutable Git commit.
package scansnapshot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Snapshot records the complete checked-out source inventory used by a scan.
type Snapshot struct {
	Root, Head string
	Files      map[string]string
}

func environment() []string {
	var env []string
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "GIT_") {
			env = append(env, item)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
}

func command(ctx context.Context, root string, args ...string) ([]byte, error) {
	args = append([]string{"-c", "safe.directory=" + root, "-c", "core.hooksPath=/dev/null", "-c", "init.templateDir=", "-c", "submodule.recurse=false", "-C", root}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = environment()
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("source snapshot Git operation failed: %w", err)
	}
	return data, nil
}

// Clone creates a private checkout with no global filters, hooks, template or
// submodule execution. A linked or nested Git object is never a source input.
func Clone(ctx context.Context, source string) (_ Snapshot, cleanup func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	root, err := filepath.Abs(source)
	if err != nil {
		return Snapshot{}, nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Snapshot{}, nil, err
	}
	head, err := command(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Snapshot{}, nil, err
	}
	ref := strings.TrimSpace(string(head))
	listing, err := command(ctx, root, "ls-tree", "-r", "--full-tree", "-z", ref)
	if err != nil {
		return Snapshot{}, nil, err
	}
	var paths []string
	for _, entry := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		metadata, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || path == "." || filepath.Clean(path) != path || filepath.IsAbs(path) || strings.HasPrefix(path, "../") || strings.HasPrefix(path, ".git/") {
			return Snapshot{}, nil, fmt.Errorf("source snapshot contains a linked, nested or unsupported Git input")
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return Snapshot{}, nil, fmt.Errorf("source snapshot has no tracked inputs")
	}
	work, err := os.MkdirTemp("", "platform-source-scan-")
	if err != nil {
		return Snapshot{}, nil, err
	}
	cleanup = func() { _ = os.RemoveAll(work) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	destination := filepath.Join(work, "source")
	if _, err = command(ctx, root, "clone", "--quiet", "--shared", "--no-hardlinks", "--no-checkout", "--", root, destination); err != nil {
		return Snapshot{}, cleanup, err
	}
	if _, err = command(ctx, destination, "checkout", "--quiet", "--detach", ref); err != nil {
		return Snapshot{}, cleanup, err
	}
	destination, err = filepath.EvalSymlinks(destination)
	if err != nil {
		return Snapshot{}, cleanup, err
	}
	result := Snapshot{Root: destination, Head: ref, Files: make(map[string]string, len(paths))}
	for _, path := range paths {
		info, statErr := os.Lstat(filepath.Join(destination, path))
		if statErr != nil || !info.Mode().IsRegular() {
			return Snapshot{}, cleanup, fmt.Errorf("snapshot source is missing or not a regular file")
		}
		data, readErr := os.ReadFile(filepath.Join(destination, path))
		if readErr != nil {
			return Snapshot{}, cleanup, readErr
		}
		result.Files[path] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	return result, cleanup, nil
}

// Verify refuses a scan whose tracked source changed after its proof.
func (s Snapshot) Verify() error {
	if len(s.Files) == 0 {
		return fmt.Errorf("empty source snapshot inventory")
	}
	for path, expected := range s.Files {
		info, err := os.Lstat(filepath.Join(s.Root, path))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot source is missing or linked")
		}
		data, err := os.ReadFile(filepath.Join(s.Root, path))
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != expected {
			return fmt.Errorf("snapshot source changed after applicability proof")
		}
	}
	return nil
}
