package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// boundedDiagnostics keeps the newest output. It never returns a write error, because
// that would make kubectl's own result fail for a reason the client did not cause.
type boundedDiagnostics struct{ data []byte }

func (b *boundedDiagnostics) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if extra := len(b.data) - 1<<16; extra > 0 {
		b.data = b.data[extra:]
	}
	return len(p), nil
}

func (b *boundedDiagnostics) String() string { return string(b.data) }

// fakeKubectl puts a synthetic kubectl first on PATH for one test.
func fakeKubectl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCommandDiagnosticsSeparateFailureFromStall(t *testing.T) {
	t.Run("failing client with partial output", func(t *testing.T) {
		fakeKubectl(t, `printf '{"metadata":'; echo 'connection refused by fixture' >&2; exit 7`)
		var diagnostics boundedDiagnostics
		var pod mailPod
		found, err := commandClient{diagnostics: &diagnostics}.read(context.Background(), podKey, &pod)
		if err == nil || found {
			t.Fatalf("failure after partial output must not read as an observation: found=%v err=%v", found, err)
		}
		for _, want := range []string{"connection refused by fixture", "exit status 7", "deadline: <nil>"} {
			if !strings.Contains(diagnostics.String(), want) {
				t.Errorf("diagnostics lack %q:\n%s", want, diagnostics.String())
			}
		}
	})
	t.Run("stalled client", func(t *testing.T) {
		fakeKubectl(t, `echo 'request sent to fixture' >&2; exec sleep 60`)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		var diagnostics boundedDiagnostics
		var pod mailPod
		if _, err := (commandClient{diagnostics: &diagnostics}).read(ctx, podKey, &pod); err == nil {
			t.Fatal("a stalled client must fail")
		}
		for _, want := range []string{"request sent to fixture", "deadline: context deadline exceeded"} {
			if !strings.Contains(diagnostics.String(), want) {
				t.Errorf("diagnostics lack %q:\n%s", want, diagnostics.String())
			}
		}
	})
}

func TestProductionClientKeepsKubectlDetailPrivate(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	fakeKubectl(t, `printf '%s\n' "$@" > '`+argsFile+`'; echo 'private operator detail' >&2; exit 7`)
	var pod mailPod
	_, err := commandClient{}.read(context.Background(), podKey, &pod)
	if err == nil || err.Error() != "bounded Kubernetes request failed" {
		t.Fatalf("production error changed: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "--v=") {
		t.Fatalf("production request enabled tracing:\n%s", args)
	}
}

func TestBoundedDiagnosticsKeepNewestOutput(t *testing.T) {
	var diagnostics boundedDiagnostics
	if n, err := diagnostics.Write(make([]byte, 1<<17)); n != 1<<17 || err != nil {
		t.Fatalf("write must always report success: n=%d err=%v", n, err)
	}
	if n, err := diagnostics.Write([]byte("newest")); n != 6 || err != nil {
		t.Fatalf("write must always report success: n=%d err=%v", n, err)
	}
	if len(diagnostics.data) != 1<<16 || !strings.HasSuffix(diagnostics.String(), "newest") {
		t.Fatalf("kept %d bytes", len(diagnostics.data))
	}
}

// Called by the verified, isolated API-server fixture. Never use ambient config.
func TestNativeCorePodDeletePreconditions(t *testing.T) {
	path := os.Getenv("MAIL_NATIVE_API_CONFIG")
	if path == "" {
		t.Skip("requires the isolated native API-server fixture")
	}
	expected, err := url.Parse(os.Getenv("MAIL_NATIVE_API_SERVER"))
	if err != nil || expected.Scheme != "https" || expected.Hostname() != "127.0.0.1" || expected.Port() == "" {
		t.Fatal("fixture must use a loopback TLS server")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Clusters []struct{ Cluster struct{ Server string } }
	}
	if err = yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Cluster.Server != expected.String() {
		t.Fatal("fixture config is not bound to the isolated server")
	}
	var diagnostics boundedDiagnostics
	c := commandClient{kubeconfig: path, diagnostics: &diagnostics}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("kubectl diagnostics (isolated fixture only):\n%s", diagnostics.String())
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var pod mailPod
	var claim persistentClaim
	for _, item := range []struct {
		key string
		out any
	}{{podKey, &pod}, {claimKey, &claim}} {
		found, err := c.read(ctx, item.key, item.out)
		if err != nil || !found {
			t.Fatal("fixture object unavailable")
		}
	}
	if pod.Metadata.UID == "" || pod.Metadata.ResourceVersion == "" || claim.Metadata.UID == "" {
		t.Fatal("real API identities missing")
	}
	for _, test := range []struct{ name, uid, version string }{
		{"wrong UID", "00000000-0000-4000-8000-000000000000", pod.Metadata.ResourceVersion},
		{"wrong version", pod.Metadata.UID, "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if c.deletePod(ctx, test.uid, test.version) == nil {
				t.Fatal("core API accepted conflicting deletion")
			}
			var current mailPod
			found, err := c.read(ctx, podKey, &current)
			if err != nil || !found || current.Metadata.UID != pod.Metadata.UID || current.Metadata.DeletionTimestamp != nil {
				t.Fatal("conflict altered the Pod")
			}
		})
	}
	if err = c.deletePod(ctx, pod.Metadata.UID, pod.Metadata.ResourceVersion); err != nil {
		t.Fatal("matching conditional deletion failed")
	}
	var deleted mailPod
	if found, err := c.read(ctx, podKey, &deleted); err != nil || found {
		t.Fatal("matching Pod deletion not observed")
	}
	var retained persistentClaim
	found, err := c.read(ctx, claimKey, &retained)
	if err != nil || !found || retained.Metadata.UID != claim.Metadata.UID {
		t.Fatal("claim identity was not retained")
	}
	t.Log("Native Kubernetes refused both conflicting identities, deleted the matching Pod, and retained its separate claim.")
}
