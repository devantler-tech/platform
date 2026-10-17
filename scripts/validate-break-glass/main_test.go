package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const kube = "apiVersion: v1\nkind: Config\ncurrent-context: recovery\nclusters:\n- name: recovery\n  cluster: {server: 'https://example.invalid', certificate-authority-data: PRIVATE-MATERIAL}\nusers:\n- name: recovery\n  user: {client-certificate-data: PRIVATE-MATERIAL, client-key-data: PRIVATE-MATERIAL}\ncontexts:\n- name: recovery\n  context: {cluster: recovery, user: recovery}\n"
const talos = "context: recovery\ncontexts:\n  recovery:\n    endpoints: [example.invalid]\n    ca: PRIVATE-MATERIAL\n    crt: PRIVATE-MATERIAL\n    key: PRIVATE-MATERIAL\n"

func export(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"data": map[string]any{"data": fields, "metadata": map[string]any{"version": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func validExport(t *testing.T) string {
	return export(t, map[string]any{"kubeconfig": kube, "talosconfig": talos})
}

func TestValidExportAndExclusiveExtraction(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "recovery.yaml")
	var out bytes.Buffer
	if status := run([]string{"--field", "kubeconfig", "--output", path}, strings.NewReader(validExport(t)), &out); status != 0 {
		t.Fatalf("valid named fields refused: status=%d output=%s", status, out.String())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != kube {
		t.Fatal("extraction changed configuration bytes")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe mode: %o", info.Mode().Perm())
	}
	if strings.Contains(out.String(), "PRIVATE-MATERIAL") {
		t.Fatal("credential leaked")
	}
	if status := run([]string{"--field", "kubeconfig", "--output", path}, strings.NewReader(validExport(t)), &out); status != 2 {
		t.Fatalf("existing file was not refused: %d", status)
	}
	body, _ = os.ReadFile(path)
	if string(body) != kube {
		t.Fatal("existing file modified")
	}
}

func TestValidationOnlyCreatesNoFile(t *testing.T) {
	var out bytes.Buffer
	if status := run(nil, strings.NewReader(validExport(t)), &out); status != 0 {
		t.Fatalf("status=%d", status)
	}
	if strings.Contains(out.String(), "PRIVATE-MATERIAL") {
		t.Fatal("credential leaked")
	}
}

func TestInvalidExportsNeverLeakOrCreateOutput(t *testing.T) {
	cases := []string{
		`{"PRIVATE-MATERIAL":""}`,
		export(t, map[string]any{kube: "", talos: ""}),
		export(t, map[string]any{"kubeconfig": kube, "talosconfig": ""}),
		export(t, map[string]any{"kubeconfig": "PRIVATE-MATERIAL", "talosconfig": talos}),
		export(t, map[string]any{"kubeconfig": kube, "talosconfig": talos, "PRIVATE-MATERIAL": ""}),
		export(t, map[string]any{"kubeconfig": kube + "---\nPRIVATE-MATERIAL\n", "talosconfig": talos}),
		export(t, map[string]any{"kubeconfig": strings.Replace(kube, "cluster: recovery, user: recovery", "cluster: missing, user: recovery", 1), "talosconfig": talos}),
		export(t, map[string]any{"kubeconfig": strings.Replace(kube, "users:\n- name: recovery", "users:\n- name: recovery\n  user: {}\n- name: recovery", 1), "talosconfig": talos}),
		export(t, map[string]any{"kubeconfig": kube, "talosconfig": "context: missing\ncontexts: {}\n"}),
		`{"data":{"data":{"kubeconfig":"PRIVATE-MATERIAL","kubeconfig":"","talosconfig":""}}}`,
		validExport(t) + `{"PRIVATE-MATERIAL":""}`,
		`{"data":{"data":{"PRIVATE-MATERIAL":`,
	}
	for i, input := range cases {
		path := filepath.Join(t.TempDir(), "must-not-exist")
		var out bytes.Buffer
		if status := run([]string{"--field", "kubeconfig", "--output", path}, strings.NewReader(input), &out); status != 1 {
			t.Errorf("case %d: status=%d", i, status)
		}
		if strings.Contains(out.String(), "PRIVATE-MATERIAL") {
			t.Errorf("case %d leaked input", i)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("case %d created output", i)
		}
	}
}

func TestSymlinkOutputNeverChangesTarget(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if status := run([]string{"--field", "talosconfig", "--output", link}, strings.NewReader(validExport(t)), &out); status != 2 {
		t.Fatalf("status=%d", status)
	}
	body, _ := os.ReadFile(target)
	if string(body) != "keep" {
		t.Fatal("symlink target modified")
	}
}

func TestNonPrivateDirectoryIsRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "must-not-exist")
	var out bytes.Buffer
	if status := run([]string{"--field", "talosconfig", "--output", path}, strings.NewReader(validExport(t)), &out); status != 2 {
		t.Fatalf("status=%d", status)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("created output in a shared directory")
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestFailedObservationsAreUnknown(t *testing.T) {
	var out bytes.Buffer
	if run(nil, failedReader{}, &out) != 2 {
		t.Fatal("failed input read was accepted")
	}
	if run(nil, strings.NewReader(strings.Repeat("x", 1024*1024+1)), &out) != 2 {
		t.Fatal("unbounded input was accepted")
	}
	if run(nil, strings.NewReader(validExport(t)), failedWriter{}) != 2 {
		t.Fatal("unpublished verdict was accepted")
	}
}
