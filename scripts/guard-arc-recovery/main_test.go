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
		{"native empty null items", strings.Replace(empty, `"items":[]`, `"items":null`, 1), 200, true},
		{"duplicate live items hidden by null", strings.Replace(empty, `"items":[]`, `"items":[{"metadata":{"name":"active"}}],"items":null`, 1), 200, false},
		{"duplicate continuation hidden by empty", strings.Replace(empty, `"resourceVersion":"123"`, `"resourceVersion":"123","continue":"next","continue":""`, 1), 200, false},
		{"duplicate remaining count hidden by zero", strings.Replace(empty, `"resourceVersion":"123"`, `"resourceVersion":"123","remainingItemCount":1,"remainingItemCount":0`, 1), 200, false},
		{"case aliased items", strings.Replace(empty, `"items":[]`, `"Items":[]`, 1), 200, false},
		{"case aliased revision", strings.Replace(empty, `"resourceVersion"`, `"ResourceVersion"`, 1), 200, false},
		{"case alias hiding live items", strings.Replace(empty, `"items":[]`, `"items":[{"metadata":{"name":"active"}}],"Items":null`, 1), 200, false},
		{"null items without revision", strings.Replace(strings.Replace(empty, `"items":[]`, `"items":null`, 1), `"resourceVersion":"123"`, `"resourceVersion":""`, 1), 200, false},
		{"paginated null items", strings.Replace(strings.Replace(empty, `"items":[]`, `"items":null`, 1), `"resourceVersion":"123"`, `"resourceVersion":"123","continue":"next"`, 1), 200, false},
		{"object items", strings.Replace(empty, `"items":[]`, `"items":{}`, 1), 200, false},
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

func TestRecoveryObservesRetainedPoolWithoutConfusingItWithActiveRunners(t *testing.T) {
	const item = `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":"ksail-code-quality","namespace":"arc-ksail-analysis","uid":"retained","resourceVersion":"123"}}`
	const prefix = `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"123"},"items":`
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"not yet installed", prefix + `null}`, true},
		{"retained declaration", prefix + `[` + item + `]}`, true},
		{"Helm release name is not the pool name", prefix + `[` + strings.Replace(item, "ksail-code-quality", "ksail-analysis-runners", 1) + `]}`, false},
		{"unexpected pool", prefix + `[` + strings.Replace(item, "ksail-code-quality", "other", 1) + `]}`, false},
		{"foreign namespace", prefix + `[` + strings.Replace(item, "arc-ksail-analysis", "arc-runners", 1) + `]}`, false},
		{"missing UID", prefix + `[` + strings.Replace(item, `"uid":"retained"`, `"uid":""`, 1) + `]}`, false},
		{"missing revision", prefix + `[` + strings.Replace(item, `"resourceVersion":"123"`, `"resourceVersion":""`, 1) + `]}`, false},
		{"being deleted", prefix + `[` + strings.Replace(item, `"uid":"retained"`, `"uid":"retained","deletionTimestamp":"2026-10-06T00:00:00Z"`, 1) + `]}`, false},
		{"duplicate retained identity", prefix + `[` + strings.Replace(item, `"name":"ksail-code-quality"`, `"name":"foreign","name":"ksail-code-quality"`, 1) + `]}`, false},
		{"aliased retained kind", prefix + `[` + strings.Replace(item, `"kind"`, `"Kind"`, 1) + `]}`, false},
		{"multiple pools", prefix + `[` + item + `,` + item + `]}`, false},
		{"full resource", prefix + `[` + strings.Replace(item, `"metadata":`, `"spec":{"maxRunners":0},"metadata":`, 1) + `]}`, false},
		{"pagination", strings.Replace(prefix, `"resourceVersion":"123"`, `"resourceVersion":"123","continue":"next"`, 1) + `null}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Accept") != metadataAccept {
					t.Error("retained pool observation widened")
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := requireRetainedMetadata(context.Background(), metadataClient(), server.URL)
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v, want %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestRecoveryDeclarationRefusesSuspensionAndImplicitRunnerBounds(t *testing.T) {
	for _, tc := range []struct {
		name, controller, legacy string
		ok                       bool
	}{
		{"drained", "false", "suspend: false\n  values: {runnerScaleSetName: ksail-code-quality, minRunners: 0, maxRunners: 0}", true},
		{"suspended controller", "true", "suspend: false\n  values: {runnerScaleSetName: ksail-code-quality, minRunners: 0, maxRunners: 0}", false},
		{"suspended legacy", "false", "suspend: true\n  values: {runnerScaleSetName: ksail-code-quality, minRunners: 0, maxRunners: 0}", false},
		{"implicit maximum", "false", "suspend: false\n  values: {runnerScaleSetName: ksail-code-quality, minRunners: 0}", false},
		{"implicit minimum", "false", "suspend: false\n  values: {runnerScaleSetName: ksail-code-quality, maxRunners: 0}", false},
		{"admits one runner", "false", "suspend: false\n  values: {runnerScaleSetName: ksail-code-quality, minRunners: 0, maxRunners: 1}", false},
		{"implicit pool name", "false", "suspend: false\n  values: {minRunners: 0, maxRunners: 0}", false},
		{"different pool name", "false", "suspend: false\n  values: {runnerScaleSetName: other, minRunners: 0, maxRunners: 0}", false},
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
	requests := make(chan string, len(recoveryEndpoints)+1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		if r.Method != "GET" || r.Header.Get("Accept") != metadataAccept {
			t.Error("native proxy widened the read")
		}
		if r.URL.Path == "/api/v1/namespaces/arc-systems/pods" && r.URL.Query().Get("labelSelector") != "platform.devantler.tech/arc-role=listener" {
			t.Error("native proxy lost the retained listener selector")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == retainedPoolPath {
			_, _ = w.Write([]byte(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"123"},"items":[{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":"ksail-code-quality","namespace":"arc-ksail-analysis","uid":"retained","resourceVersion":"123"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"123"},"items":null}`))
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
		if path == retainedPoolPath {
			if err := requireRetainedMetadata(ctx, client, base+path); err != nil {
				t.Fatal(err)
			}
			continue
		}
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
	if len(recoveryEndpoints) != 17 {
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
		{"valid case-sensitive metadata keys", strings.Replace(recoveredDeployment, `"generation":2`, `"generation":2,"labels":{"App":"one","app":"two"},"annotations":{"Description":"one","description":"two"}`, 1), true},
		{"legacy scope", strings.Replace(recoveredDeployment, "--watch-single-namespace=arc-runners", "--watch-single-namespace=arc-ksail-analysis", 1), false},
		{"duplicate scope", strings.Replace(recoveredDeployment, `"--watch-single-namespace=arc-runners"`, `"--watch-single-namespace=arc-runners","--watch-single-namespace=arc-ksail-analysis"`, 1), false},
		{"broad scope", strings.Replace(recoveredDeployment, "--watch-single-namespace=arc-runners", "--watch-namespace=", 1), false},
		{"legacy mode", strings.Replace(recoveredDeployment, "--auto-scaling-runner-set-only", "--other-mode", 1), false},
		{"option terminator", strings.Replace(recoveredDeployment, `"args":[`, `"args":["--",`, 1), false},
		{"positional before scope", strings.Replace(recoveredDeployment, `"args":[`, `"args":["operand",`, 1), false},
		{"positional after scope", strings.Replace(recoveredDeployment, `"--watch-single-namespace=arc-runners"`, `"--watch-single-namespace=arc-runners","operand"`, 1), false},
		{"short option", strings.Replace(recoveredDeployment, `"args":[`, `"args":["-x",`, 1), false},
		{"duplicate stale observed generation", strings.Replace(recoveredDeployment, `"observedGeneration":2`, `"observedGeneration":1,"observedGeneration":2`, 1), false},
		{"duplicate controller arguments", strings.Replace(recoveredDeployment, `"args":[`, `"args":["--watch-single-namespace=foreign"],"args":[`, 1), false},
		{"case aliased ready count", strings.Replace(recoveredDeployment, `"readyReplicas"`, `"ReadyReplicas"`, 1), false},
		{"case aliased args", strings.Replace(recoveredDeployment, `"args"`, `"Args"`, 1), false},
		{"case aliased UID", strings.Replace(recoveredDeployment, `"uid"`, `"UID"`, 1), false},
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
