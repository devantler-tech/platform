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
	"reflect"
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
	for _, path := range []string{"poller-native/main.go", "poller-native/result.go", "poller-native/graph.go", "poller-native/go.mod", "poller-native/go.sum"} {
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
			marker := "NATIVE_FLUX_POLLER=WAITED"
			if neverAbsent {
				marker = "NATIVE_FLUX_POLLER=WAITED_TIMEOUT"
			}
			if strings.TrimSpace(waiterOutput.String()) != marker {
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

func TestNativeKubectlRawDryRunPreservesServerQueryAndBoundIdentity(t *testing.T) {
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		if os.Getenv("PORTAL_REQUIRE_NATIVE_CONTROL") == "1" {
			t.Fatal("required native dry-run control needs kubectl")
		}
		t.Skip("native dry-run control requires kubectl")
	}
	var mu sync.Mutex
	requests, ordinaryDelete := 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		w.Header().Set("Content-Type", "application/json")
		var opts map[string]any
		ordinaryDelete = r.Method != "DELETE" || r.URL.Path != "/apis/apps/v1/namespaces/backstage/deployments/backstage" || r.URL.Query().Get("dryRun") != "All" || json.NewDecoder(r.Body).Decode(&opts) != nil || !reflect.DeepEqual(opts["dryRun"], []any{"All"}) || field(opts, "preconditions", "uid") != "native-deployment-uid" || field(opts, "preconditions", "resourceVersion") != "17"
		if ordinaryDelete {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Status","status":"Success"}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "kubeconfig")
	body := "apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\ncurrent-context: admin@prod\nusers:\n- name: fixture\n  user: {}\n"
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", cfg)
	e := engine{execute: func(c context.Context, p string, args ...string) ([]byte, error) {
		if p != "kubectl" {
			return nil, fmt.Errorf("unexpected native dry-run program")
		}
		return capture(c, bin, args...)
	}}
	resource := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "backstage", "namespace": "backstage", "uid": "native-deployment-uid", "resourceVersion": "17"}}
	path, err := resourcePath(resource)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := e.write(ctx, dryRunDeleteOptions(resource), "delete", "--raw", path+"?dryRun=All", "-f", "REQUEST")
	mu.Lock()
	defer mu.Unlock()
	if err != nil || requests != 1 || ordinaryDelete || response["status"] != "Success" {
		t.Fatalf("native raw dry-run query or identity lost: error=%t requests=%d ordinaryDelete=%t", err != nil, requests, ordinaryDelete)
	}
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
				bound = opts["apiVersion"] == "v1" && opts["kind"] == "DeleteOptions" && opts["propagationPolicy"] == "Orphan" && opts["dryRun"] == nil && field(opts, "preconditions", "uid") == "native-release-uid" && field(opts, "preconditions", "resourceVersion") == "17"
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
			var submitted bool
			submitted, err = e.remove(ctx, r, "/apis/helm.toolkit.fluxcd.io/v2/namespaces/backstage/helmreleases/backstage")
			stateMu.Lock()
			defer stateMu.Unlock()
			if (err != nil) != replaced || !submitted || deletes != 1 || !bound {
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

// Exercise the complete guarded entrypoint with a real kubectl journal PATCH.
// All other lifecycle requests stay inside the existing offline fixture.
func TestNativeJournalWriterKeepsItsManagerAndIdentityPreconditions(t *testing.T) {
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		if os.Getenv("PORTAL_REQUIRE_NATIVE_CONTROL") == "1" {
			t.Fatal("required journal writer control needs kubectl")
		}
		t.Skip("native journal writer control requires kubectl")
	}
	retirementRunner(t)
	f := retirementFixture(t)
	initial := encode(t, f.namespace)
	var mu sync.Mutex
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			_, _ = io.WriteString(w, `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"],"serverAddressByClientCIDRs":[]}`)
		case "/apis":
			_, _ = io.WriteString(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
		case "/api/v1":
			_, _ = io.WriteString(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[{"name":"namespaces","singularName":"namespace","namespaced":false,"kind":"Namespace","verbs":["get","patch"]}]}`)
		case "/api/v1/namespaces/backstage":
			if r.Method == http.MethodGet {
				_, _ = w.Write(initial)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			patches++
			var ops []map[string]any
			if r.Method != http.MethodPatch || r.URL.Query().Get("fieldManager") != "backstage-retirement" || r.Header.Get("Content-Type") != "application/json-patch+json" || json.NewDecoder(r.Body).Decode(&ops) != nil || len(ops) != 3 || ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != "namespace-uid" || ops[1]["op"] != "test" || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != "12" || ops[2]["op"] != "add" || ops[2]["path"] != "/metadata/annotations/platform.devantler.tech~1backstage-retirement" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","code":403}`)
				return
			}
			journal, decodeErr := decode([]byte(text(ops[2]["value"])))
			if decodeErr != nil || journal["namespaceUID"] != "namespace-uid" || journal["run"] != "42" || journal["phase"] != "fenced" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			namespace, _ := decode(initial)
			object(field(namespace, "metadata", "annotations"))[fenceAnnotation] = ops[2]["value"]
			object(namespace["metadata"])["resourceVersion"] = "13"
			_ = json.NewEncoder(w).Encode(namespace)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := filepath.Join(t.TempDir(), "kubeconfig")
	body := "apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\ncurrent-context: admin@prod\nusers:\n- name: fixture\n  user: {}\n"
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", cfg)
	execute := func(ctx context.Context, program string, args ...string) ([]byte, error) {
		if program == "kubectl" && strings.Contains(strings.Join(args, " "), "patch namespace backstage") {
			data, err := capture(ctx, bin, args...)
			if err == nil {
				f.namespace, err = decode(data)
				f.fenceInstalled = err == nil
			}
			return data, err
		}
		return f.execute(ctx, program, args...)
	}
	var out bytes.Buffer
	code := run([]string{"--execute"}, &out, execute)
	mu.Lock()
	defer mu.Unlock()
	if code != 0 || patches != 1 || !f.fenceInstalled {
		t.Fatalf("native journal writer lost its field manager or identity: code=%d patches=%d %s", code, patches, &out)
	}
}
