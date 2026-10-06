package main

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveryMetadataReadsRequireCompleteEmptyMetadataLists(t *testing.T) {
	const empty = `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"123"},"items":[]}`
	for _, tc := range []struct {
		name, body string
		code       int
		ok         bool
	}{
		{"empty", empty, 200, true},
		{"live resource", strings.Replace(empty, `"items":[]`, `"items":[{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":"active"}}]`, 1), 200, false},
		{"full pod list", strings.Replace(empty, "PartialObjectMetadataList", "PodList", 1), 200, false},
		{"full data masquerading as metadata", strings.Replace(empty, `"items":[]`, `"spec":{"token":"synthetic"},"items":[]`, 1), 200, false},
		{"missing items", strings.Replace(empty, `,"items":[]`, "", 1), 200, false},
		{"null items", strings.Replace(empty, `"items":[]`, `"items":null`, 1), 200, false},
		{"pagination", strings.Replace(empty, `"resourceVersion":"123"`, `"resourceVersion":"123","continue":"next"`, 1), 200, false},
		{"missing revision", strings.Replace(empty, `"resourceVersion":"123"`, `"resourceVersion":""`, 1), 200, false},
		{"wrong api", strings.Replace(empty, "meta.k8s.io/v1", "v1", 1), 200, false},
		{"trailing document", empty + empty, 200, false},
		{"malformed", `{`, 200, false},
		{"too large", empty + strings.Repeat(" ", 1<<20), 200, false},
		{"forbidden", empty, 403, false},
		{"redirect", empty, 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Accept") != metadataAccept {
					t.Error("metadata request widened its method or response type")
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := requireEmptyMetadata(context.Background(), metadataClient(), server.URL+"/api/v1/namespaces/arc-runners/pods")
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v, want %v: %v", err == nil, tc.ok, err)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic") {
				t.Fatal("response contents escaped into diagnostics")
			}
		})
	}
}

func TestRecoveryDeclarationRefusesSuspensionAndImplicitRunnerBounds(t *testing.T) {
	for _, tc := range []struct {
		name, controller, legacy string
		ok                       bool
	}{
		{"drained", "false", "suspend: false\n  values: {minRunners: 0, maxRunners: 0}", true},
		{"suspended controller", "true", "suspend: false\n  values: {minRunners: 0, maxRunners: 0}", false},
		{"suspended legacy", "false", "suspend: true\n  values: {minRunners: 0, maxRunners: 0}", false},
		{"implicit maximum", "false", "suspend: false\n  values: {minRunners: 0}", false},
		{"implicit minimum", "false", "suspend: false\n  values: {maxRunners: 0}", false},
		{"admits one runner", "false", "suspend: false\n  values: {minRunners: 0, maxRunners: 1}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"k8s/bases/infrastructure/controllers/actions-runner-controller/helm-release.yaml": "metadata:\n  annotations: {platform.devantler.tech/arc-recovery: drain-only}\nspec:\n  suspend: " + tc.controller + "\n  values:\n    flags: {watchSingleNamespace: arc-runners}\n",
				"k8s/providers/hetzner/infrastructure/retained-ksail-analysis/helm-release.yaml":   "spec:\n  " + tc.legacy + "\n",
			}
			for path, data := range files {
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			armed, err := recoveryArmed(root)
			if (err == nil && armed) != tc.ok {
				t.Fatalf("armed=%v err=%v", armed, err)
			}
		})
	}
}

// Exercise the real kubectl proxy with a temporary synthetic kubeconfig and
// TLS API server. This never opens the operator's kubeconfig or a live cluster.
func TestNativeMetadataProxyPreservesAcceptAndRejectsWrites(t *testing.T) {
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Fatal("native metadata-proxy regression requires kubectl")
	}
	requests := make(chan string, 16)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		if r.Method != "GET" || r.Header.Get("Accept") != metadataAccept {
			t.Error("native proxy widened the read")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"123"},"items":[]}`))
	}))
	defer server.Close()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: synthetic\n  cluster:\n    server: %s\n    certificate-authority-data: %s\ncontexts:\n- name: admin@prod\n  context: {cluster: synthetic, user: synthetic}\nusers:\n- name: synthetic\n  user: {token: synthetic}\n", server.URL, base64.StdEncoding.EncodeToString(cert))
	configPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var child *exec.Cmd
	base, stop, err := startMetadataProxy(ctx, func(ctx context.Context, args ...string) *exec.Cmd {
		child = exec.CommandContext(ctx, kubectl, args...)
		child.Env = append(os.Environ(), "KUBECONFIG="+configPath)
		return child
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client := metadataClient()
	defer client.CloseIdleConnections()
	for _, path := range recoveryEndpoints {
		if err := requireEmptyMetadata(ctx, client, base+path); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ method, path string }{
		{"DELETE", recoveryEndpoints[0]},
		{"POST", recoveryEndpoints[0]},
		{"PUT", recoveryEndpoints[0]},
		{"PATCH", recoveryEndpoints[0]},
		{"HEAD", recoveryEndpoints[0]},
		{"OPTIONS", recoveryEndpoints[0]},
		{"PROPFIND", recoveryEndpoints[0]},
		{"get", recoveryEndpoints[0]},
		{"G", recoveryEndpoints[0]},
		{"GE", recoveryEndpoints[0]},
		{"GETTING", recoveryEndpoints[0]},
		{"GET", "/api/v1/namespaces/arc-runners/secrets"},
		{"GET", "/apis/actions.github.com/v1alpha1/namespaces/arc-runners/ephemeralrunners/active"},
	} {
		request, _ := http.NewRequestWithContext(ctx, tc.method, base+tc.path, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("unexpected denied-proxy status %d", response.StatusCode)
		}
	}
	if len(requests) != len(recoveryEndpoints) {
		t.Fatal("denied request reached the API server")
	}
	stop()
	if child.ProcessState == nil {
		t.Fatal("native proxy was not reaped")
	}
}

func TestRecoveryUsesOnlyMetadataEndpoints(t *testing.T) {
	if len(recoveryEndpoints) != 12 {
		t.Fatal("incomplete recovery observation set")
	}
	for _, path := range recoveryEndpoints {
		if strings.Contains(path, "secrets") && !strings.Contains(path, "externalsecrets") {
			t.Fatal("credential read")
		}
		if !strings.Contains(path, "/namespaces/arc-runners/") && !strings.Contains(path, "/namespaces/arc-ksail-analysis/") && !strings.Contains(path, "/namespaces/arc-systems/") {
			t.Fatal("foreign namespace")
		}
	}
}

const recoveredDeployment = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"arc-controller","namespace":"arc-systems","uid":"synthetic","generation":2},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"manager","command":["/manager"],"args":["--auto-scaling-runner-set-only","--watch-single-namespace=arc-runners"]}]}}},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}`

func TestRecoveryRequiresAppliedControllerScope(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"recovered", recoveredDeployment, true},
		{"legacy scope", strings.Replace(recoveredDeployment, "--watch-single-namespace=arc-runners", "--watch-single-namespace=arc-ksail-analysis", 1), false},
		{"duplicate scope", strings.Replace(recoveredDeployment, `"--watch-single-namespace=arc-runners"`, `"--watch-single-namespace=arc-runners","--watch-single-namespace=arc-ksail-analysis"`, 1), false},
		{"broad scope", strings.Replace(recoveredDeployment, "--watch-single-namespace=arc-runners", "--watch-namespace=", 1), false},
		{"legacy mode", strings.Replace(recoveredDeployment, "--auto-scaling-runner-set-only", "--other-mode", 1), false},
		{"stale generation", strings.Replace(recoveredDeployment, `"observedGeneration":2`, `"observedGeneration":1`, 1), false},
		{"old replica", strings.Replace(recoveredDeployment, `"replicas":1,"updatedReplicas"`, `"replicas":2,"updatedReplicas"`, 1), false},
		{"unready", strings.Replace(recoveredDeployment, `"readyReplicas":1`, `"readyReplicas":0`, 1), false},
		{"wrong resource", strings.Replace(recoveredDeployment, `"kind":"Deployment"`, `"kind":"Pod"`, 1), false},
		{"trailing document", recoveredDeployment + recoveredDeployment, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Accept") != "application/json" || r.URL.Path != controllerDeploymentPath {
					t.Error("controller read widened")
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := requireControllerScope(context.Background(), metadataClient(), server.URL+controllerDeploymentPath)
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v, want %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}
