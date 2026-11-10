package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestNamespaceIdentityRequiresTheNativeFixedActiveNamespace(t *testing.T) {
	const valid = `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"longhorn-system","uid":"namespace-uid","resourceVersion":"1"},"status":{"phase":"Active"}}`
	if uid, err := namespaceIdentity([]byte(valid)); err != nil || uid != "namespace-uid" {
		t.Fatalf("valid namespace refused: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(valid, `"Namespace"`, `"Secret"`, 1),
		strings.Replace(valid, `"longhorn-system"`, `"default"`, 1),
		strings.Replace(valid, `"Active"`, `"Terminating"`, 1),
		strings.Replace(valid, `"uid":"namespace-uid"`, `"uid":"foreign","uid":"namespace-uid"`, 1),
		strings.Replace(valid, `"uid":`, `"UID":`, 1),
		valid + `{}`,
	} {
		if _, err := namespaceIdentity([]byte(bad)); err == nil {
			t.Fatal("unbound namespace accepted")
		}
	}
}

func TestCommandPipeCannotBypassOutputBound(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("scripts", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("scripts/verify-prod-recovery-source.sh", []byte("head -c 2097153 /dev/zero\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := fixedCommand(context.Background(), "bash", "./scripts/verify-prod-recovery-source.sh", strings.Repeat("a", 40))
	if err == nil || len(raw) != 0 {
		t.Fatalf("oversized subprocess output retained: bytes=%d error=%v", len(raw), err)
	}
}

func TestCommandOutputBoundDiscardsPartialResponses(t *testing.T) {
	var output boundedOutput
	if _, err := output.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte(strings.Repeat("x", 2*1024*1024))); err == nil {
		t.Fatal("output bound ignored")
	}
	if !output.exceeded || output.Len() != 0 {
		t.Fatal("partial oversized response retained")
	}
}
