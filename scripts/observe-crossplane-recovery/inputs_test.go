package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestReviewedInputsPreserveCompleteConfig(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	path := "k8s/bases/infrastructure/coroot/components/crossplane-sync-exporter/"
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"config-map.yaml", []byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  managed-resources.tsv: |\n    sample-v1\n  second: unchanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deployment := []byte("apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    metadata:\n      annotations:\n        platform.devantler.tech/config-checksum: reviewed-checksum\n")
	if err := os.WriteFile(path+"deployment.yaml", deployment, 0600); err != nil {
		t.Fatal(err)
	}
	expected, checksum, err := reviewedInputs()
	if err != nil || expected["managed-resources.tsv"] != "sample-v1\n" || expected["second"] != "unchanged" || len(expected) != 2 || checksum != "reviewed-checksum" {
		t.Fatalf("source binding failed: data=%v checksum=%s err=%v", expected, checksum, err)
	}
	if err := os.WriteFile(path+"deployment.yaml", []byte("spec: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reviewedInputs(); err == nil {
		t.Fatal("missing reviewed checksum was accepted")
	}
	if err := os.WriteFile(path+"deployment.yaml", deployment, 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"data: {}\n", "data: [\n", "data: null\n", strings.Repeat(" ", 1048577)} {
		if err := os.WriteFile(path+"config-map.yaml", []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reviewedInputs(); err == nil {
			t.Fatal("invalid reviewed configuration was accepted")
		}
	}
	if err := os.Remove(path + "config-map.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reviewedInputs(); err == nil {
		t.Fatal("missing reviewed configuration was accepted")
	}
}
func TestResponseLimitRejectsAdditionalBytes(t *testing.T) {
	var response responseBuffer
	if n, err := response.Write(bytes.Repeat([]byte("x"), 1048576)); err != nil || n != 1048576 {
		t.Fatalf("bounded response failed: n=%d err=%v", n, err)
	}
	if _, err := response.Write([]byte("x")); err == nil || !response.overflow || len(response.body) != 1048576 {
		t.Fatal("oversized response was retained or accepted")
	}
}
