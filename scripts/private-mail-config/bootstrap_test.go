package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBootstrapRequiresEveryNativeOperationReceipt(t *testing.T) {
	plan := []byte("{\"@type\":\"upsert\",\"object\":\"Domain\"}\n{\"@type\":\"update\",\"object\":\"Account\"}\n")
	valid := `{"index":0,"op":"upsert","object":"Domain","status":"ok"}
{"index":1,"op":"update","object":"Account","status":"ok"}
{"op":"summary","done":{"failed":0}}`
	if validateApply(plan, []byte(valid)) != nil {
		t.Fatal("complete native receipts rejected")
	}
	for _, bad := range []string{
		``, `{"op":"summary","done":{"failed":0}}`,
		`{"index":0,"op":"upsert","object":"Domain","status":"ok"}`,
		valid + "\n" + `{"index":1,"op":"update","object":"Account","status":"error"}`,
		`{"index":0,"op":"upsert","object":"Domain","status":"ok"}
{"index":0,"op":"update","object":"Account","status":"ok"}
{"op":"summary","done":{"failed":0}}`,
		valid[:len(valid)-2],
	} {
		if validateApply(plan, []byte(bad)) == nil {
			t.Fatal("incomplete or failed native apply accepted")
		}
	}
}

func bootstrapInputs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"settings.json":          `{"domain":"example.test","hostname":"mail.example.test","maintainer":"maintainer","agent":"agents","administrator":"operator"}`,
		"administrator-password": strings.Repeat("A", 40), "maintainer-password": strings.Repeat("B", 40), "agent-password": strings.Repeat("C", 40),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// This actual child process deliberately never opens management. The parent
// must stop and collect it when startup fails or cancellation arrives.
func TestOwnedBootstrapChild(t *testing.T) {
	if os.Getenv("STALWART_RECOVERY_MODE") != "1" {
		return
	}
	if !strings.HasPrefix(os.Getenv("STALWART_RECOVERY_ADMIN"), "bootstrap:") {
		os.Exit(3)
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1]+".port", []byte(os.Getenv("STALWART_RECOVERY_MODE_PORT")), 0600); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(3)
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	<-ch
	signal.Stop(ch)
}

func TestFailedBootstrapStopsOwnedProcessAndFencesConcurrentInitializer(t *testing.T) {
	// An inherited serving port cannot make the owned recovery child reachable
	// through the normal administration Service during startup or a retry.
	t.Setenv("STALWART_RECOVERY_MODE_PORT", "8080")
	dir, data := bootstrapInputs(t), t.TempDir()
	pidFile := filepath.Join(data, "owned-child")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime := bootstrapRuntime{server: executable, serverArgs: []string{"-test.run=TestOwnedBootstrapChild", "--", pidFile}, cli: "/must-not-run", url: "http://127.0.0.1:1", dataDir: data, startup: 5 * time.Second, shutdown: 3 * time.Second}
	runtime.applicationFile, runtime.applicationDigest = applicationAssetFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- bootstrap(ctx, dir, runtime) }()
	deadline := time.Now().Add(3 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if b, readErr := os.ReadFile(pidFile); readErr == nil {
			pid, err = strconv.Atoi(string(b))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("owned recovery process did not start")
	}
	if bootstrap(context.Background(), dir, runtime) == nil {
		t.Fatal("concurrent initializer acquired same volume")
	}
	cancel()
	if <-done == nil {
		t.Fatal("cancelled bootstrap permitted serving")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if process.Signal(syscall.Signal(0)) == nil {
		t.Fatal("owned recovery child leaked")
	}
	port, err := os.ReadFile(pidFile + ".port")
	if err != nil {
		t.Fatal(err)
	}
	value, err := strconv.Atoi(string(port))
	// This literal is the independent boundary that serving policies exclude;
	// deriving it from the implementation would miss a simultaneous port drift.
	if err != nil || value < 1024 || value > 65535 || value == 8080 || string(port) != "18080" {
		t.Fatal("bootstrap did not give its recovery child a separate unprivileged management port")
	}
}

func TestBootstrapRefusesAnExistingManagementServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	runtime := bootstrapRuntime{server: "/must-not-run", url: server.URL, dataDir: t.TempDir()}
	runtime.applicationFile, runtime.applicationDigest = applicationAssetFixture(t)
	if bootstrap(context.Background(), bootstrapInputs(t), runtime) == nil {
		t.Fatal("pre-existing management server trusted")
	}
	if response, err := http.Get(server.URL); err != nil {
		t.Fatal("unowned server was stopped")
	} else {
		_ = response.Body.Close()
	}
}

func TestPolicyReadbackRejectsExtraPermissionAndExpressionBranches(t *testing.T) {
	decode := func(s string) any {
		t.Helper()
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		want, actual string
		valid        bool
	}{
		{`{"@type":"User"}`, `{"@type":"User","default":false}`, true},
		{`{"match":{},"else":"false"}`, `{"match":{},"else":"false"}`, true},
		{`{"match":{},"else":"false"}`, `{"match":{"9":{"if":"true","then":"true"}},"else":"false"}`, false},
		{`{"enabledPermissions":{"jmapEmailGet":true}}`, `{"enabledPermissions":{"jmapEmailGet":true,"jmapEmailImport":true}}`, false},
		{`{"disabledPermissions":{}}`, `{"disabledPermissions":null}`, false},
		{`{"bind":{"0.0.0.0:1993":true}}`, `{"bind":{"0.0.0.0:1993":true,"0.0.0.0:143":true}}`, false},
	} {
		if policyMatches(decode(tc.want), decode(tc.actual), "") != tc.valid {
			t.Fatal("unsafe policy readback result")
		}
	}
}
