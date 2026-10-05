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

func TestMalformedManifestShapeIsUnknown(t *testing.T) {
	for _, input := range []string{"null\n", "{}\n", "data: []\n", "data: false\n"} {
		t.Run(input, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if status := run([]string{path, root}, &out); status != 2 {
				t.Fatalf("malformed manifest must be unknown: status=%d output=%s", status, out.String())
			}
		})
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestFailedOutputIsUnknown(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if status := run([]string{path, root}, failedWriter{}); status != 2 {
		t.Fatalf("unpublished verdict must be unknown: status=%d", status)
	}
}

func TestDiscoveryCannotMissSupportedGroups(t *testing.T) {
	cases := []struct {
		name, config, annotation string
		status                   int
		diagnostic               string
	}{
		{"inline-comment", validConfig, "gethomepage.dev/group: \"Links\" # annotation comment\n", 1, "reuses a service group"},
		{"all-service-keys", strings.Replace(validConfig, "    - Apps: []", "    - Apps: []\n      Links: []", 1), "", 1, "reuses a service group"},
		{"partial-services", strings.Replace(validConfig, "    - Apps: []", "    - Apps: []\n    - malformed", 1), "", 2, "unsupported service"},
		{"wrong-service-list", strings.Replace(validConfig, "    - Apps: []", "    - Apps: invalid", 1), "", 2, "unsupported service"},
		{"invalid-annotation", validConfig, "gethomepage.dev/group: []\n", 2, "unsupported service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "annotation.yaml"), []byte(tc.annotation), 0600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if status := run([]string{path, root}, &out); status != tc.status || !strings.Contains(out.String(), tc.diagnostic) {
				t.Fatalf("status=%d output=%s", status, out.String())
			}
		})
	}
}

func TestDuplicateBookmarkGroupsAreRejected(t *testing.T) {
	config := strings.Replace(validConfig, "  settings.yaml:", "    - Links:\n      - Docs:\n        - icon: mdi-book\n          href: https://example.com\n  settings.yaml:", 1)
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if status := run([]string{path, root}, &out); status != 1 || !strings.Contains(out.String(), "duplicate bookmark group") {
		t.Fatalf("status=%d output=%s", status, out.String())
	}
}
