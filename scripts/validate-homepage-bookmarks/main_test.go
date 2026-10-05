package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `data:
  bookmarks.yaml: |
    - Links:
      - Docs:
        - icon: mdi-book
          href: https://example.com
  settings.yaml: |
    layout:
      Links: {}
  services.yaml: |
    - Apps: []
`

func TestPartialAnnotationReadNeverPasses(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.yaml"), []byte("gethomepage.dev/group: Other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "z.yaml")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if status := run([]string{path, root}, &out); status != 2 || strings.Contains(out.String(), " valid.") {
		t.Fatalf("partial observation must be unknown: status=%d output=%s", status, out.String())
	}
}

func TestAmbiguousYAMLIsUnknown(t *testing.T) {
	for _, suffix := range []string{"---\ndata: {}\n", "  bookmarks.yaml: '[]'\n"} {
		t.Run(suffix, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(validConfig+suffix), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if status := run([]string{path, root}, &out); status != 2 || strings.Contains(out.String(), " valid.") {
				t.Fatalf("ambiguous input must be unknown: status=%d output=%s", status, out.String())
			}
		})
	}
}

func TestAnnotationCollisionIsIndependentOfServicesYAML(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "service.yaml"), []byte("metadata:\n  annotations:\n    gethomepage.dev/group: \"Links\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if status := run([]string{path, root}, &out); status != 1 || !strings.Contains(out.String(), "Links: bookmark group reuses a service group name") {
		t.Fatalf("annotation must independently constrain groups: status=%d output=%s", status, out.String())
	}
}
