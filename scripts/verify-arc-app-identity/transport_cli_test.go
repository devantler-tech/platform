package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the compiled main entrypoint, including its production-mode wiring.
// Only the external Kubernetes process is replaced; TLS and health are real.
func TestCompiledTransportCommandNeverRequestsReaderOrAppAccess(t *testing.T) {
	var requests atomic.Int32
	server, ca := transportTLSFixture(t, "openbao-arc.openbao.svc.cluster.local", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.RequestURI() != "/v1/sys/health?standbyok=true" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Vault-Token") != "" {
			t.Error("compiled transport command reached an authentication endpoint")
		}
		_, _ = io.WriteString(w, `{"initialized":true,"sealed":false,"standby":true}`)
	}))
	root := configFixture(t)
	config, status := loadConfiguration(root)
	if status != pass {
		t.Fatal(status)
	}
	mutateFixture(t, root, storePath, config.store.caBundle, base64.StdEncoding.EncodeToString(ca))
	for path, name := range map[string]string{bootstrapPath: "bootstrap.json", storePath: "store.json"} {
		value, err := readYAMLFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(root, "verify-arc-app-identity")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CLI build failed: %v, %s", err, output)
	}
	port := strings.TrimPrefix(server.URL, "https://127.0.0.1:")
	script := `#!/bin/bash
set -euo pipefail
printf '%s\n' "$*" >>"${ARC_FIXTURE_ROOT}/commands"
case "$*" in
  '--namespace=flux-system get configmap variables-cluster --output=json') cat "${ARC_FIXTURE_ROOT}/bootstrap.json" ;;
  '--namespace=arc-runners get secretstore openbao --output=json') cat "${ARC_FIXTURE_ROOT}/store.json" ;;
  '--namespace=openbao port-forward --address=127.0.0.1 service/openbao-arc 0:8204')
    printf '%s' "$$" >"${ARC_FIXTURE_ROOT}/forward.pid"
    printf 'Forwarding from 127.0.0.1:%s -> 8204\n' "${ARC_FIXTURE_PORT}"
    exec sleep 60 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "kubectl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		data, _ := os.ReadFile(filepath.Join(root, "forward.pid"))
		pid, err := strconv.Atoi(string(data))
		if err == nil && pid > 1 {
			process, _ := os.FindProcess(pid)
			if process != nil {
				_ = process.Kill()
			}
		}
	})
	for name, value := range map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REPOSITORY": "devantler-tech/platform", "ARC_IDENTITY_CONFIRM": "verify-arc-app-transport", "ARC_FIXTURE_ROOT": root, "ARC_FIXTURE_PORT": port} {
		t.Setenv(name, value)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--transport")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "ARC_APP_TRANSPORT=PASS\n" {
		t.Fatalf("compiled transport command failed: %v, output=%q", err, output)
	}
	commands, err := os.ReadFile(filepath.Join(root, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	want := "--namespace=flux-system get configmap variables-cluster --output=json\n--namespace=arc-runners get secretstore openbao --output=json\n--namespace=openbao port-forward --address=127.0.0.1 service/openbao-arc 0:8204\n"
	if string(commands) != want || requests.Load() != 1 {
		t.Fatalf("compiled command escaped fixed transport operations: commands=%q, requests=%d", commands, requests.Load())
	}
}
