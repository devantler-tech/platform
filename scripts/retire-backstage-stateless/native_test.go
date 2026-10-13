package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// This executes the pinned published controller's native Helm/SSA adapter,
// not a simulated Helm implementation or a CLI waiter with different semantics.
// The loopback API provides synthetic storage and asynchronous delete responses;
// it is not evidence of a production controller or real garbage collector run.
func TestNativeHelmHistoryHooksAndAsynchronousUninstall(t *testing.T) {
	bin := os.Getenv("PORTAL_HELM_TEST_BIN")
	if bin == "" {
		if os.Getenv("PORTAL_REQUIRE_NATIVE_CONTROL") == "1" {
			t.Fatal("required native control has no pinned Helm binary")
		}
		t.Skip("native control requires explicitly pinned PORTAL_HELM_TEST_BIN")
	}
	version, err := exec.Command(bin, "version", "--short").Output()
	if err != nil || strings.TrimSpace(string(version)) != "v4.2.0+g0646808" {
		t.Fatal("native control requires Helm v4.2.0+g0646808")
	}
	// Bind normal Go test-cache reuse to the nested driver source/locks too.
	// Required hosted runs additionally use -count=1.
	for _, path := range []string{"poller-native/main.go", "poller-native/go.mod", "poller-native/go.sum"} {
		if _, err := os.ReadFile(path); err != nil {
			t.Fatal("native driver source/checksum lock unavailable")
		}
	}
	nativeDriver := filepath.Join(t.TempDir(), "flux-poller-native")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-mod=readonly", "-race", "-o", nativeDriver, ".")
	build.Dir = "poller-native"
	if err := build.Run(); err != nil {
		t.Fatal("could not build the checksum-pinned published-controller native driver")
	}
	for _, neverAbsent := range []bool{false, true} {
		t.Run(fmt.Sprintf("wait_failure_%t", neverAbsent), func(t *testing.T) {
			api := nativeHelmAPI{objects: map[string]map[string]any{}, deletes: map[string]int{}, waitReads: map[string]int{}, neverAbsent: neverAbsent}
			r, _ := fixture()
			addObservedResource(r, "ConfigMap", "backstage-app-config")
			// Adversarial hooks would create Jobs unless the native no-hooks option
			// is honored. The production eligibility gate rejects hooks altogether.
			r["hooks"] = []any{map[string]any{"name": "unexpected-delete-hook", "kind": "Job", "path": "templates/hook.yaml", "events": []any{"pre-delete", "post-delete"}, "manifest": "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: unexpected-delete-hook\n  namespace: backstage\n"}}
			for _, values := range object(field(r, "info", "resources")) {
				for _, raw := range array(values) {
					o := object(raw)
					if o["kind"] == "PodList" {
						continue
					}
					path := "/api/v1/namespaces/backstage/" + strings.ToLower(text(o["kind"])) + "s/" + text(field(o, "metadata", "name"))
					if o["kind"] == "Deployment" {
						path = "/apis/apps/v1/namespaces/backstage/deployments/backstage"
					}
					api.objects[path] = o
				}
			}
			delete(object(r["info"]), "resources")
			api.secret = nativeReleaseSecret(t, r)
			server := httptest.NewServer(http.HandlerFunc(api.serve))
			defer server.Close()
			dir := t.TempDir()
			cfg := filepath.Join(dir, "kubeconfig")
			body := "apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\n    namespace: backstage\ncurrent-context: admin@prod\nusers:\n- name: fixture\n  user: {}\n"
			if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			opts := uninstallOptions()
			if opts["keepHistory"] != true || opts["disableHooks"] != true || opts["disableWait"] != false {
				t.Fatal("producer lost native safety controls")
			}
			timeout := text(opts["timeout"])
			if neverAbsent {
				timeout = "100ms"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			opts["timeout"] = timeout
			cmd := exec.CommandContext(ctx, nativeDriver, server.URL, string(encode(t, opts)))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "KUBECONFIG=" + cfg, "HELM_DRIVER=secret", "HELM_CACHE_HOME=" + dir, "HELM_CONFIG_HOME=" + dir, "HELM_DATA_HOME=" + dir}
			var waiterOutput bytes.Buffer
			cmd.Stdout, cmd.Stderr = &waiterOutput, io.Discard
			err = cmd.Run()
			if strings.TrimSpace(waiterOutput.String()) != "NATIVE_FLUX_POLLER=WAITED" {
				api.mu.Lock()
				defer api.mu.Unlock()
				t.Fatalf("native control did not execute the production-equivalent Flux poller: result=%q requests=%v", waiterOutput.String(), api.requests)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if (err != nil) != neverAbsent {
				t.Fatalf("native Helm result did not match actual absence: error=%t requests=%v", err != nil, api.requests)
			}
			if len(api.deletes) != 3 || api.foreignMutation || api.badPropagation || api.secret == nil || api.historyUpdates < 2 {
				t.Fatalf("native uninstall lost safety controls: targets=%d foreign=%t cascade=%t historyUpdates=%d requests=%v", len(api.deletes), api.foreignMutation, api.badPropagation, api.historyUpdates, api.requests)
			}
			waited := false
			for path := range api.objects {
				waited = waited || api.waitReads[path] > 0
				if api.deletes[path] != 1 || !neverAbsent && api.waitReads[path] < 2 {
					t.Fatalf("native delete/wait not exercised for %s", path)
				}
			}
			if !waited {
				t.Fatal("native waiter was not exercised")
			}
			// Native Helm records uninstalled even after a deletion wait failure.
			// Therefore the protected producer separately proves actual absence.
			if field(api.secret, "metadata", "labels", "status") != "uninstalled" {
				t.Fatal("native retained history did not record terminal status")
			}
		})
	}
}

func nativeReleaseSecret(t *testing.T, r map[string]any) map[string]any {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(encode(t, r)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	stored := base64.StdEncoding.EncodeToString(compressed.Bytes())
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "sh.helm.release.v1.backstage.v4", "namespace": "backstage", "uid": "native-history-uid", "resourceVersion": "1", "labels": map[string]any{"owner": "helm", "name": "backstage", "status": "deployed", "version": "4"}}, "type": "helm.sh/release.v1", "data": map[string]any{"release": base64.StdEncoding.EncodeToString([]byte(stored))}}
}

func TestNativeKubectlSendsBoundDeleteOptionsAndRejectsReplacement(t *testing.T) {
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		if os.Getenv("PORTAL_REQUIRE_NATIVE_CONTROL") == "1" {
			t.Fatal("required native API control needs kubectl")
		}
		t.Skip("native API control requires kubectl")
	}
	for _, replaced := range []bool{false, true} {
		t.Run(fmt.Sprint(replaced), func(t *testing.T) {
			var stateMu sync.Mutex
			deletes := 0
			bound := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stateMu.Lock()
				defer stateMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method != "DELETE" || r.URL.Path != "/apis/helm.toolkit.fluxcd.io/v2/namespaces/backstage/helmreleases/backstage" {
					w.WriteHeader(403)
					return
				}
				deletes++
				var opts map[string]any
				if json.NewDecoder(r.Body).Decode(&opts) != nil {
					w.WriteHeader(400)
					return
				}
				bound = opts["apiVersion"] == "v1" && opts["kind"] == "DeleteOptions" && opts["propagationPolicy"] == "Orphan" && field(opts, "preconditions", "uid") == "native-release-uid" && field(opts, "preconditions", "resourceVersion") == "17"
				if !bound || replaced {
					w.WriteHeader(409)
					_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Conflict", "code": 409})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
			}))
			defer server.Close()
			dir := t.TempDir()
			cfg := filepath.Join(dir, "kubeconfig")
			body := "apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\ncurrent-context: admin@prod\nusers:\n- name: fixture\n  user: {}\n"
			if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			execute := func(ctx context.Context, p string, args ...string) ([]byte, error) {
				if p == "bash" {
					return []byte("PROD_RECOVERY_SOURCE=PASS"), nil
				}
				if p != "kubectl" {
					return nil, fmt.Errorf("unexpected fixture command")
				}
				cmd := exec.CommandContext(ctx, bin, append([]string{"--kubeconfig=" + cfg}, args...)...)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "KUBECONFIG=" + cfg}
				cmd.Stderr = io.Discard
				return cmd.Output()
			}
			e := engine{execute: execute, sha: strings.Repeat("a", 40)}
			r := map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": map[string]any{"name": "backstage", "namespace": "backstage", "uid": "native-release-uid", "resourceVersion": "17"}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = e.remove(ctx, r, "/apis/helm.toolkit.fluxcd.io/v2/namespaces/backstage/helmreleases/backstage")
			stateMu.Lock()
			defer stateMu.Unlock()
			if (err != nil) != replaced || deletes != 1 || !bound {
				t.Fatalf("native API transport lost CAS/failed-result semantics: error=%t deletes=%d bound=%t", err != nil, deletes, bound)
			}
		})
	}
}

type nativeHelmAPI struct {
	mu                                           sync.Mutex
	secret                                       map[string]any
	objects                                      map[string]map[string]any
	deletes, waitReads                           map[string]int
	requests                                     []string
	historyUpdates                               int
	neverAbsent, foreignMutation, badPropagation bool
}

func (a *nativeHelmAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	respond := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		respond(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "NotFound", "code": 404})
	}
	if r.Method == "GET" {
		switch r.URL.Path {
		case "/version":
			respond(map[string]any{"major": "1", "minor": "36", "gitVersion": "v1.36.0"})
			return
		case "/api":
			respond(map[string]any{"apiVersion": "v1", "kind": "APIVersions", "versions": []string{"v1"}})
			return
		case "/apis":
			respond(map[string]any{"apiVersion": "v1", "kind": "APIGroupList", "groups": []any{map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "apps/v1", "version": "v1"}}}})
			return
		case "/api/v1", "/apis/apps/v1":
			gv, resources := "v1", []any{}
			for plural, kind := range map[string]string{"services": "Service", "configmaps": "ConfigMap", "secrets": "Secret"} {
				resources = append(resources, map[string]any{"name": plural, "singularName": "", "namespaced": true, "kind": kind, "verbs": []string{"get", "list", "update", "delete"}})
			}
			if r.URL.Path == "/apis/apps/v1" {
				gv = "apps/v1"
				resources = []any{map[string]any{"name": "deployments", "singularName": "", "namespaced": true, "kind": "Deployment", "verbs": []string{"get", "list", "delete"}}}
			}
			respond(map[string]any{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": gv, "resources": resources})
			return
		case "/api/v1/namespaces/backstage/secrets":
			items := []any{}
			if !strings.Contains(r.URL.Query().Get("labelSelector"), "status=deployed") || field(a.secret, "metadata", "labels", "status") == "deployed" {
				items = append(items, a.secret)
			}
			respond(map[string]any{"apiVersion": "v1", "kind": "SecretList", "items": items})
			return
		case "/api/v1/namespaces/backstage/secrets/sh.helm.release.v1.backstage.v4":
			respond(a.secret)
			return
		}
		if o := a.objects[r.URL.Path]; o != nil {
			if a.deletes[r.URL.Path] > 0 {
				a.waitReads[r.URL.Path]++
				if !a.neverAbsent && a.waitReads[r.URL.Path] > 1 {
					notFound()
					return
				}
			}
			respond(o)
			return
		}
		notFound()
		return
	}
	if r.Method == "PUT" && r.URL.Path == "/api/v1/namespaces/backstage/secrets/sh.helm.release.v1.backstage.v4" {
		secret, err := nativeSecretRequest(r)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		object(secret["metadata"])["uid"] = "native-history-uid"
		object(secret["metadata"])["resourceVersion"] = fmt.Sprint(a.historyUpdates + 2)
		a.secret = secret
		a.historyUpdates++
		respond(secret)
		return
	}
	if r.Method == "DELETE" && a.objects[r.URL.Path] != nil {
		var opts map[string]any
		if json.NewDecoder(r.Body).Decode(&opts) != nil || opts["propagationPolicy"] != "Foreground" {
			a.badPropagation = true
		}
		a.deletes[r.URL.Path]++
		respond(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
		return
	}
	a.foreignMutation = true
	w.WriteHeader(403)
	respond(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Forbidden", "code": 403})
}

// Typed Kubernetes clients send protobuf Secret updates by default. Decode only
// the synthetic metadata/data needed by this controlled API, using the native
// wire-format reader already present in the module; no production data is used.
func nativeSecretRequest(r *http.Request) (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxResponse+1))
	if err != nil || len(b) > maxResponse {
		return nil, fmt.Errorf("incomplete fixture request")
	}
	if json.Valid(b) {
		var o map[string]any
		err = json.Unmarshal(b, &o)
		return o, err
	}
	if !bytes.HasPrefix(b, []byte{'k', '8', 's', 0}) {
		return nil, fmt.Errorf("unexpected fixture encoding")
	}
	fields := func(b []byte) (map[protowire.Number][][]byte, error) {
		m := map[protowire.Number][][]byte{}
		for len(b) > 0 {
			n, typ, k := protowire.ConsumeTag(b)
			if k < 0 {
				return nil, fmt.Errorf("bad fixture protobuf")
			}
			b = b[k:]
			if typ == protowire.BytesType {
				v, k := protowire.ConsumeBytes(b)
				if k < 0 {
					return nil, fmt.Errorf("bad fixture bytes")
				}
				m[n] = append(m[n], v)
				b = b[k:]
			} else {
				k := protowire.ConsumeFieldValue(n, typ, b)
				if k < 0 {
					return nil, fmt.Errorf("bad fixture field")
				}
				b = b[k:]
			}
		}
		return m, nil
	}
	envelope, err := fields(b[4:])
	if err != nil || len(envelope[2]) != 1 {
		return nil, fmt.Errorf("bad fixture envelope")
	}
	secret, err := fields(envelope[2][0])
	if err != nil || len(secret[1]) != 1 || len(secret[3]) != 1 {
		return nil, fmt.Errorf("bad fixture Secret")
	}
	meta, err := fields(secret[1][0])
	if err != nil {
		return nil, err
	}
	labels, data := map[string]any{}, map[string]any{}
	for _, v := range meta[11] {
		pair, err := fields(v)
		if err != nil || len(pair[1]) != 1 || len(pair[2]) != 1 {
			return nil, fmt.Errorf("bad fixture labels")
		}
		labels[string(pair[1][0])] = string(pair[2][0])
	}
	for _, v := range secret[2] {
		pair, err := fields(v)
		if err != nil || len(pair[1]) != 1 || len(pair[2]) != 1 {
			return nil, fmt.Errorf("bad fixture data")
		}
		data[string(pair[1][0])] = base64.StdEncoding.EncodeToString(pair[2][0])
	}
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "sh.helm.release.v1.backstage.v4", "namespace": "backstage", "labels": labels}, "type": string(secret[3][0]), "data": data}, nil
}
