package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validFlag = `apiVersion: core.openfeature.dev/v1beta1
kind: FeatureFlag
metadata: {name: trial}
spec:
  flagSpec:
    flags:
      enabled:
        state: ENABLED
        defaultVariant: "off"
        variants: {"on": true, "off": false}
        targeting: {if: [{"==": [{var: role}, admin]}, "on", "off"]}
`

func fixture(t *testing.T, data string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "flag.yaml"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestActualGuardAcceptsValidAndRejectsMalformedDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"valid", validFlag, true},
		{"default absent", strings.ReplaceAll(validFlag, "        defaultVariant: \"off\"\n", ""), false},
		{"default missing from variants", strings.ReplaceAll(validFlag, "defaultVariant: \"off\"", "defaultVariant: missing"), false},
		{"unquoted YAML 1.1 boolean default", strings.ReplaceAll(validFlag, "defaultVariant: \"off\"", "defaultVariant: off"), false},
		{"mixed variant types", strings.ReplaceAll(validFlag, "\"off\": false", "\"off\": 42"), false},
		{"unknown targeting operator", strings.ReplaceAll(validFlag, "{var: role}", "{wrong: role}"), false},
		{"duplicate key", strings.ReplaceAll(validFlag, "state: ENABLED", "state: ENABLED\n        state: DISABLED"), false},
		{"unsupported API", strings.ReplaceAll(validFlag, "v1beta1", "v9"), false},
		{"malformed YAML", "kind: [", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			err := validate(fixture(t, tc.data), &output)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t error=%v output=%s", tc.valid, err, &output)
			}
			if tc.valid && !strings.Contains(output.String(), "Validated 1 FeatureFlag") {
				t.Fatalf("missing coverage: %s", &output)
			}
		})
	}
}

func TestNoFlagsReportsEmptyCoverage(t *testing.T) {
	var output bytes.Buffer
	if err := validate(fixture(t, "kind: ConfigMap\ndata: {flags: irrelevant}\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "No FeatureFlag resources found") {
		t.Fatal(output.String())
	}
}

func TestListsAndMultipleDocumentsCannotHideFlags(t *testing.T) {
	data := "apiVersion: v1\nkind: List\nitems:\n" + "  - " + strings.ReplaceAll(strings.TrimSuffix(validFlag, "\n"), "\n", "\n    ") + "\n---\n" + validFlag
	var output bytes.Buffer
	if err := validate(fixture(t, data), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Validated 2 FeatureFlag") {
		t.Fatal(output.String())
	}
	data = strings.ReplaceAll(data, "\"off\": false", "\"off\": 42")
	if err := validate(fixture(t, data), &output); err == nil {
		t.Fatal("nested invalid flags passed")
	}
}

func TestUnreadableRootIsUnknown(t *testing.T) {
	if err := validate(filepath.Join(t.TempDir(), "missing"), &bytes.Buffer{}); err == nil {
		t.Fatal("unreadable root reported clean")
	}
}

func TestTypedListItemsWithoutHeadersAreValidated(t *testing.T) {
	item := strings.SplitN(validFlag, "metadata:", 2)[1]
	data := "apiVersion: core.openfeature.dev/v1beta1\nkind: FeatureFlagList\nitems:\n  - metadata:" + strings.ReplaceAll(item, "\n", "\n    ")
	var output bytes.Buffer
	if err := validate(fixture(t, data), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Validated 1 FeatureFlag") {
		t.Fatal(output.String())
	}
	data = strings.ReplaceAll(data, "\"off\": false", "\"off\": 42")
	if err := validate(fixture(t, data), &output); err == nil {
		t.Fatal("typed list hid an invalid definition")
	}
}

func TestSymlinkedDirectoriesCannotHideFlagCoverage(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(fixture(t, validFlag), filepath.Join(dir, "hidden")); err != nil {
		t.Fatal(err)
	}
	if err := validate(dir, &bytes.Buffer{}); err == nil {
		t.Fatal("symlink was reported as empty coverage")
	}
}

func TestNonManifestSymlinksDoNotBlockValidation(t *testing.T) {
	dir := fixture(t, validFlag)
	target := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(target, []byte("documentation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := validate(dir, &bytes.Buffer{}); err != nil {
		t.Fatalf("non-manifest symlink blocked valid flags: %v", err)
	}
}

func TestManifestAndUnresolvedSymlinksCannotHideCoverage(t *testing.T) {
	for _, name := range []string{"flag.yaml", "flag.yml", "unresolved"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(fixture(t, validFlag), "flag.yaml")
			if name == "unresolved" {
				target = filepath.Join(t.TempDir(), "missing")
			}
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if err := validate(dir, &bytes.Buffer{}); err == nil {
				t.Fatal("unexamined manifest coverage reported clean")
			}
		})
	}
}
