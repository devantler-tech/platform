package main

import (
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeSource(t *testing.T, root, path, source string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCompletePackageContextRetainsForeignPlatformAndTaggedFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"doc.go", "source.go", "only_windows.go", "tagged.go", "nested/source.go", "nested/source_test.go"}
	for _, path := range paths {
		writeSource(t, root, path, "// Package fixture is an offline source.\npackage fixture\n")
	}
	writeSource(t, root, "tagged.go", "//go:build unavailable_fixture_tag\n\npackage fixture\n")
	context := build.Default
	context.GOOS, context.GOARCH = "linux", "amd64"
	args, dirs, fallbacks, err := targets(root, paths, context)
	if err != nil {
		t.Fatal(err)
	}
	if dirs != 2 || fallbacks != 2 || !slices.Equal(args, []string{".", "nested", "only_windows.go", "tagged.go"}) {
		t.Fatalf("coverage changed: dirs=%d fallback=%d args=%v", dirs, fallbacks, args)
	}
}

func TestDiscoveryRefusesIncompleteOrAmbiguousSources(t *testing.T) {
	for _, test := range []struct {
		name  string
		paths []string
	}{
		{"empty", nil}, {"empty entry", []string{""}},
		{"missing", []string{"missing.go"}},
		{"duplicate", []string{"source.go", "source.go"}},
		{"escape", []string{"../source.go"}},
		{"linked", []string{"linked.go", "source.go"}},
		{"untracked package member", []string{"source.go"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeSource(t, root, "source.go", "package fixture\n")
			if test.name == "untracked package member" {
				writeSource(t, root, "untracked.go", "package fixture\n")
			}
			if test.name == "linked" {
				if err := os.Symlink(filepath.Join(root, "source.go"), filepath.Join(root, "linked.go")); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, _, err := targets(root, test.paths, build.Default); err == nil {
				t.Fatal("accepted incomplete or ambiguous source discovery")
			}
		})
	}
}
