package cleanup_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type object = map[string]any

const (
	ns         = "customer-identity"
	volume     = "pvc-48c25421-defa-4132-bfca-c5a6c6d95435"
	nsUID      = "c6ce7557-6df5-453d-9cd3-f0a2716f7bad"
	releaseUID = "8c85192a-e332-466c-b529-20d707998a05"
	clusterUID = "2eeef2ae-da51-4835-a910-1773df49a042"
	claimUID   = "48c25421-defa-4132-bfca-c5a6c6d95435"
	pvUID      = "927373f3-c828-47d3-b536-16aab906e428"
	backendUID = "63cc671a-856f-493e-b02a-d55183a18876"
)

type resource struct {
	group, version, plural, kind string
	namespaced                   bool
}

// The discovery the workflow sees. Only the remote API is replaced: the real
// shell, jq and kubectl run the committed step, so kubectl must send
// DeleteOptions with the UID precondition rather than a name-only delete.
var resources = []resource{
	{"", "v1", "namespaces", "Namespace", false},
	{"", "v1", "persistentvolumes", "PersistentVolume", false},
	{"", "v1", "persistentvolumeclaims", "PersistentVolumeClaim", true},
	{"", "v1", "configmaps", "ConfigMap", true},
	{"", "v1", "secrets", "Secret", true},
	{"", "v1", "pods", "Pod", true},
	{"", "v1", "services", "Service", true},
	{"", "v1", "endpoints", "Endpoints", true},
	{"", "v1", "serviceaccounts", "ServiceAccount", true},
	{"", "v1", "events", "Event", true},
	{"apps", "v1", "deployments", "Deployment", true},
	{"apps", "v1", "statefulsets", "StatefulSet", true},
	{"batch", "v1", "jobs", "Job", true},
	{"postgresql.cnpg.io", "v1", "clusters", "Cluster", true},
	{"postgresql.cnpg.io", "v1", "backups", "Backup", true},
	{"helm.toolkit.fluxcd.io", "v2", "helmreleases", "HelmRelease", true},
	{"kustomize.toolkit.fluxcd.io", "v1", "kustomizations", "Kustomization", true},
	{"source.toolkit.fluxcd.io", "v1", "ocirepositories", "OCIRepository", true},
	{"longhorn.io", "v1beta2", "volumes", "Volume", true},
	{"longhorn.io", "v1beta2", "replicas", "Replica", true},
	{"cilium.io", "v2", "ciliumnetworkpolicies", "CiliumNetworkPolicy", true},
	{"wgpolicyk8s.io", "v1alpha2", "policyreports", "PolicyReport", true},
}

type apiFixture struct {
	mu          sync.Mutex
	objects     map[string]object
	readFailure int
	replaceAt   int
	deletes     int
	replaced    bool
	// declareAfter > 0 makes the namespace appear in the apps inventory from
	// that Kustomization list read onwards, while the cleanup is under way.
	declareAfter      int
	kustomizationList int
}

const revision = "latest@sha256:0000000000000000000000000000000000000000000000000000000000000001"

func ready(status string) []object {
	return []object{{"type": "Ready", "status": status, "reason": "Fixture"}}
}

func (a *apiFixture) apps() object {
	return a.objects[path(find("Kustomization"), "flux-system", "apps")]
}

func base(r resource) string {
	if r.group == "" {
		return "/api/" + r.version
	}
	return "/apis/" + r.group + "/" + r.version
}

func path(r resource, namespace, name string) string {
	p := base(r)
	if r.namespaced {
		p += "/namespaces/" + namespace
	}
	return p + "/" + r.plural + "/" + name
}

func find(kind string) resource {
	for _, r := range resources {
		if r.kind == kind {
			return r
		}
	}
	panic("unknown kind " + kind)
}

func (a *apiFixture) add(kind, namespace, name, uid string, extra object) object {
	r := find(kind)
	metadata := object{"name": name, "uid": uid, "resourceVersion": "1"}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	apiVersion := r.version
	if r.group != "" {
		apiVersion = r.group + "/" + r.version
	}
	o := object{"apiVersion": apiVersion, "kind": kind, "metadata": metadata}
	for key, value := range extra {
		if key == "metadata" {
			for mk, mv := range value.(object) {
				metadata[mk] = mv
			}
			continue
		}
		o[key] = value
	}
	a.objects[path(r, namespace, name)] = o
	return o
}

func owner(kind, name, uid string) object {
	return object{"ownerReferences": []object{{"apiVersion": "v1", "kind": kind, "name": name, "uid": uid}}}
}

func newAPI() *apiFixture {
	a := &apiFixture{objects: map[string]object{}}
	a.add("Kustomization", "flux-system", "apps", "uid-apps", object{
		"metadata": object{"generation": 25},
		"spec":     object{"sourceRef": object{"kind": "OCIRepository", "name": "flux-system"}},
		"status": object{
			"observedGeneration": 25, "lastAppliedRevision": revision, "lastAttemptedRevision": revision, "conditions": ready("True"),
			"inventory": object{"entries": []object{{"id": "umami_umami_helm.toolkit.fluxcd.io_HelmRelease", "v": "v2"}}},
		},
	})
	a.add("OCIRepository", "flux-system", "flux-system", "uid-source", object{
		"metadata": object{"generation": 8},
		"status":   object{"observedGeneration": 8, "artifact": object{"revision": revision}, "conditions": ready("True")},
	})
	a.add("Namespace", "", ns, nsUID, object{"metadata": object{"labels": object{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}}})
	a.add("Cluster", ns, "customer-identity-db", clusterUID, object{"status": object{"phase": "Setting up primary", "instances": 1}})
	a.add("HelmRelease", ns, "zitadel", releaseUID, object{"status": object{"history": []object{{"status": "uninstalling"}}}})
	a.add("PersistentVolumeClaim", ns, "customer-identity-db-1", claimUID, object{"metadata": owner("Cluster", "customer-identity-db", clusterUID), "spec": object{"volumeName": volume}})
	a.add("PersistentVolume", "", volume, pvUID, object{"spec": object{"storageClassName": "longhorn-wffc", "claimRef": object{"namespace": ns, "name": "customer-identity-db-1", "uid": claimUID}}})
	a.add("Volume", "longhorn-system", volume, backendUID, object{"status": object{"state": "detached", "actualSize": 0, "lastBackup": "", "kubernetesStatus": object{"lastPodRefAt": ""}}})
	// Derived and generated objects the live namespace holds.
	a.add("Job", ns, "customer-identity-db-1-initdb", "uid-initdb", object{"metadata": owner("Cluster", "customer-identity-db", clusterUID)})
	a.add("Pod", ns, "customer-identity-db-1-initdb-x", "uid-initdb-pod", object{"metadata": owner("Job", "customer-identity-db-1-initdb", "uid-initdb")})
	helm := object{"labels": object{"app.kubernetes.io/managed-by": "Helm", "helm.toolkit.fluxcd.io/name": "zitadel", "helm.toolkit.fluxcd.io/namespace": ns}}
	a.add("Job", ns, "zitadel-init", "uid-zitadel-init", object{"metadata": helm})
	a.add("ConfigMap", ns, "zitadel-config-yaml", "uid-zitadel-config", object{"metadata": helm})
	a.add("ServiceAccount", ns, "zitadel", "uid-zitadel-sa", object{"metadata": helm})
	a.add("Secret", ns, "sh.helm.release.v1.zitadel.v1", "uid-release-secret", object{"type": "helm.sh/release.v1", "metadata": object{"labels": object{"owner": "helm", "name": "zitadel"}}})
	a.add("Secret", ns, "customer-identity-db-ca", "uid-db-ca", object{"metadata": owner("Cluster", "customer-identity-db", clusterUID)})
	a.add("Service", ns, "customer-identity-db-rw", "uid-rw", object{"metadata": owner("Cluster", "customer-identity-db", clusterUID)})
	a.add("Endpoints", ns, "customer-identity-db-rw", "uid-rw-endpoints", nil)
	a.add("ConfigMap", ns, "cnpg-default-monitoring", "uid-cnpg-monitoring", object{"metadata": object{"labels": object{"app.kubernetes.io/managed-by": "cloudnative-pg"}}})
	a.add("ConfigMap", ns, "kube-root-ca.crt", "uid-root-ca", nil)
	a.add("ConfigMap", ns, "cloudflare-origin-ca", "uid-bundle", object{"metadata": object{"labels": object{"trust.cert-manager.io/bundle": "cloudflare-origin-ca"}, "ownerReferences": []object{{"kind": "Bundle", "name": "cloudflare-origin-ca", "uid": "uid-cluster-bundle"}}}})
	a.add("ServiceAccount", ns, "default", "uid-default-sa", nil)
	a.add("CiliumNetworkPolicy", ns, "default-deny", "uid-default-deny", object{"metadata": object{"labels": object{"app.kubernetes.io/managed-by": "kyverno", "generate.kyverno.io/policy-name": "add-default-deny"}}})
	a.add("Event", ns, "zitadel.1", "uid-event", nil)
	a.add("PolicyReport", ns, "report-1", "uid-report", object{"metadata": owner("Pod", "gone", "uid-deleted-pod")})
	return a
}

func (a *apiFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(code int, data any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(data)
	}
	status := func(code int, reason string) {
		write(code, object{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": reason, "code": code, "message": reason})
	}
	p := r.URL.Path
	if r.Method == http.MethodGet {
		switch p {
		case "/api":
			write(200, object{"apiVersion": "v1", "kind": "APIVersions", "versions": []string{"v1"}})
			return
		case "/apis":
			seen := map[string]bool{}
			groups := []object{}
			for _, res := range resources {
				if res.group == "" || seen[res.group] {
					continue
				}
				seen[res.group] = true
				v := object{"groupVersion": res.group + "/" + res.version, "version": res.version}
				groups = append(groups, object{"name": res.group, "versions": []object{v}, "preferredVersion": v})
			}
			write(200, object{"apiVersion": "v1", "kind": "APIGroupList", "groups": groups})
			return
		}
		list := []object{}
		for _, res := range resources {
			if base(res) == p {
				list = append(list, object{"name": res.plural, "singularName": strings.ToLower(res.kind), "kind": res.kind, "namespaced": res.namespaced, "verbs": []string{"get", "list", "delete", "watch"}})
			}
		}
		if len(list) > 0 {
			write(200, object{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": strings.TrimPrefix(strings.TrimPrefix(p, "/apis/"), "/api/"), "resources": list})
			return
		}
		if a.readFailure != 0 && strings.Contains(p, ns) {
			status(a.readFailure, http.StatusText(a.readFailure))
			return
		}
		if data, ok := a.objects[p]; ok {
			write(200, data)
			return
		}
		for _, res := range resources {
			prefix := base(res) + "/"
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest := strings.TrimPrefix(p, prefix)
			namespace := ""
			if strings.HasPrefix(rest, "namespaces/") && res.namespaced {
				parts := strings.SplitN(strings.TrimPrefix(rest, "namespaces/"), "/", 2)
				if len(parts) != 2 {
					continue
				}
				namespace, rest = parts[0], parts[1]
			}
			if rest != res.plural {
				continue
			}
			if res.kind == "Kustomization" {
				a.kustomizationList++
				if a.declareAfter > 0 && a.kustomizationList >= a.declareAfter {
					a.apps()["status"].(object)["inventory"] = object{"entries": []object{{"id": ns + "_zitadel_helm.toolkit.fluxcd.io_HelmRelease", "v": "v2"}}}
				}
			}
			items := []object{}
			keys := []string{}
			for key, o := range a.objects {
				m := o["metadata"].(object)
				if o["kind"] != res.kind || (namespace != "" && m["namespace"] != namespace) {
					continue
				}
				if selector := r.URL.Query().Get("labelSelector"); selector != "" {
					labels, _ := m["labels"].(object)
					k, v, _ := strings.Cut(selector, "=")
					if labels == nil || labels[k] != v {
						continue
					}
				}
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				items = append(items, a.objects[key])
			}
			write(200, object{"apiVersion": "v1", "kind": res.kind + "List", "metadata": object{"resourceVersion": "1"}, "items": items})
			return
		}
		status(404, "NotFound")
		return
	}
	if r.Method != http.MethodDelete {
		status(405, "MethodNotAllowed")
		return
	}
	a.deletes++
	data, ok := a.objects[p]
	if !ok {
		status(404, "NotFound")
		return
	}
	if a.replaceAt == a.deletes {
		data["metadata"].(object)["uid"] = "replacement-uid"
	}
	var options struct{ Preconditions struct{ UID string } }
	_ = json.NewDecoder(r.Body).Decode(&options)
	if options.Preconditions.UID == "" || options.Preconditions.UID != data["metadata"].(object)["uid"] {
		status(409, "Conflict")
		return
	}
	if data["metadata"].(object)["uid"] == "replacement-uid" {
		a.replaced = true
	}
	delete(a.objects, p)
	if data["kind"] == "Namespace" {
		for key, o := range a.objects {
			if o["metadata"].(object)["namespace"] == ns {
				delete(a.objects, key)
			}
		}
	}
	write(200, object{"apiVersion": "v1", "kind": "Status", "status": "Success", "code": 200})
}

func runCleanup(t *testing.T, api *apiFixture) (string, error) {
	t.Helper()
	for _, tool := range []string{"kubectl", "jq", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("native workflow dependency %s is required: %v", tool, err)
		}
	}
	raw, err := os.ReadFile("../../../.github/workflows/remove-failed-customer-identity-rollout.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Name, Run string } }
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	script := ""
	for _, step := range workflow.Jobs["remove"].Steps {
		if step.Name == "Verify and remove the failed rollout" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("the production removal step is missing")
	}
	server := httptest.NewServer(api)
	defer server.Close()
	// No ambient kubeconfig, credentials, discovery cache or production endpoint.
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: fixture\nusers:\n- name: fixture\n  user: {}\n", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "KUBE_CONFIG=" + config}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("cleanup stalled against the isolated API: %s", output)
	}
	return string(output), err
}

func refused(t *testing.T, a *apiFixture, stage string) {
	t.Helper()
	output, err := runCleanup(t, a)
	if err == nil || a.deletes != 0 || !strings.Contains(output, "=REFUSED stage="+stage) || strings.Contains(output, "=COMPLETE") {
		t.Fatalf("want refusal at %s before any delete: err=%v deletes=%d output=%s", stage, err, a.deletes, output)
	}
}

func TestVerifiedRolloutIsRemoved(t *testing.T) {
	a := newAPI()
	output, err := runCleanup(t, a)
	if err != nil || !strings.Contains(output, "=VERIFIED objects=") || !strings.Contains(output, "=COMPLETE namespace=ABSENT volume=ABSENT masterkey=UNTOUCHED") || a.deletes != 3 {
		t.Fatalf("verified rollout did not complete: err=%v deletes=%d output=%s", err, a.deletes, output)
	}
	for key, o := range a.objects {
		if m := o["metadata"].(object); m["namespace"] == ns || m["name"] == volume {
			t.Fatalf("target retained: %s", key)
		}
	}
	if _, ok := a.objects[path(find("Kustomization"), "flux-system", "apps")]; !ok {
		t.Fatal("an unrelated object was removed")
	}
}

func TestAlreadyRemovedRolloutCompletesWithoutDeletes(t *testing.T) {
	a := newAPI()
	for key, o := range a.objects {
		if m := o["metadata"].(object); m["namespace"] == ns || m["name"] == ns || m["name"] == volume {
			delete(a.objects, key)
		}
	}
	output, err := runCleanup(t, a)
	if err != nil || a.deletes != 0 || !strings.Contains(output, "=COMPLETE") {
		t.Fatalf("absent targets were not confirmed: err=%v deletes=%d output=%s", err, a.deletes, output)
	}
}

func TestInitializedDatabaseIsRefused(t *testing.T) {
	a := newAPI()
	a.objects[path(find("Cluster"), ns, "customer-identity-db")]["status"] = object{"readyInstances": 1, "currentPrimary": "customer-identity-db-1"}
	refused(t, a, "database-initialized")
}

func TestBackedUpDatabaseIsRefused(t *testing.T) {
	a := newAPI()
	a.add("Backup", ns, "customer-identity-db-daily", "uid-backup", nil)
	refused(t, a, "database-backed-up")
}

func TestWrittenVolumeIsRefused(t *testing.T) {
	for name, status := range map[string]object{
		"written":  {"state": "detached", "actualSize": 4096},
		"attached": {"state": "attached", "actualSize": 0},
		"used":     {"state": "detached", "actualSize": 0, "kubernetesStatus": object{"lastPodRefAt": "2026-11-15T04:00:00Z"}},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAPI()
			a.objects[path(find("Volume"), "longhorn-system", volume)]["status"] = status
			refused(t, a, "volume-written")
		})
	}
}

func TestDeployedServerIsRefused(t *testing.T) {
	a := newAPI()
	a.objects[path(find("HelmRelease"), ns, "zitadel")]["status"] = object{"history": []object{{"status": "deployed"}}}
	refused(t, a, "release-deployed")
	b := newAPI()
	b.add("Deployment", ns, "zitadel", "uid-deployment", nil)
	refused(t, b, "server-ran")
}

func TestDeclaredNamespaceIsRefused(t *testing.T) {
	a := newAPI()
	a.apps()["status"].(object)["inventory"] = object{"entries": []object{{"id": ns + "_zitadel_helm.toolkit.fluxcd.io_HelmRelease", "v": "v2"}}}
	refused(t, a, "declared")
}

func TestUnconvergedInventoryIsNotTrusted(t *testing.T) {
	for name, change := range map[string]func(a *apiFixture){
		"apply failed": func(a *apiFixture) {
			a.apps()["status"].(object)["lastAttemptedRevision"] = "latest@sha256:0000000000000000000000000000000000000000000000000000000000000002"
		},
		"not ready": func(a *apiFixture) { a.apps()["status"].(object)["conditions"] = ready("False") },
		"reconciling": func(a *apiFixture) {
			a.apps()["status"].(object)["conditions"] = append(ready("True"), object{"type": "Reconciling", "status": "True"})
		},
		"old generation": func(a *apiFixture) { a.apps()["metadata"].(object)["generation"] = 26 },
		"newer source": func(a *apiFixture) {
			a.objects[path(find("OCIRepository"), "flux-system", "flux-system")]["status"].(object)["artifact"] = object{"revision": "latest@sha256:0000000000000000000000000000000000000000000000000000000000000003"}
		},
		"source not ready": func(a *apiFixture) {
			a.objects[path(find("OCIRepository"), "flux-system", "flux-system")]["status"].(object)["conditions"] = ready("False")
		},
		"source missing": func(a *apiFixture) {
			delete(a.objects, path(find("OCIRepository"), "flux-system", "flux-system"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAPI()
			change(a)
			output, err := runCleanup(t, a)
			if err == nil || a.deletes != 0 || strings.Contains(output, "=COMPLETE") || strings.Contains(output, "=VERIFIED") {
				t.Fatalf("unconverged inventory authorized deletion: err=%v deletes=%d output=%s", err, a.deletes, output)
			}
		})
	}
}

func TestNamespaceDeclaredDuringRunIsNotDeleted(t *testing.T) {
	a := newAPI()
	a.declareAfter = 2
	output, err := runCleanup(t, a)
	if err == nil || a.deletes != 0 || !strings.Contains(output, "=REFUSED stage=declared") || strings.Contains(output, "=COMPLETE") {
		t.Fatalf("a namespace declared after verification was deleted: err=%v deletes=%d output=%s", err, a.deletes, output)
	}
}

func TestUnexpectedObjectIsRefused(t *testing.T) {
	for name, extra := range map[string]object{
		"unowned":         nil,
		"foreign owner":   {"metadata": owner("Cluster", "elsewhere", "uid-elsewhere")},
		"other release":   {"metadata": object{"labels": object{"helm.toolkit.fluxcd.io/name": "other", "helm.toolkit.fluxcd.io/namespace": ns}}},
		"other kyverno":   {"metadata": object{"labels": object{"app.kubernetes.io/managed-by": "kyverno", "generate.kyverno.io/policy-name": "something-else"}}},
		"orphan endpoint": nil,
	} {
		t.Run(name, func(t *testing.T) {
			a := newAPI()
			kind := "ConfigMap"
			if name == "orphan endpoint" {
				kind = "Endpoints"
			}
			a.add(kind, ns, "customer-data", "uid-customer-data", extra)
			output, err := runCleanup(t, a)
			if err == nil || a.deletes != 0 || !strings.Contains(output, "stage=unexpected-objects objects="+kind+"/customer-data") {
				t.Fatalf("unexpected object was not refused: err=%v deletes=%d output=%s", err, a.deletes, output)
			}
		})
	}
}

func TestReplacedTargetIsNotDeleted(t *testing.T) {
	a := newAPI()
	a.objects[path(find("Namespace"), "", ns)]["metadata"].(object)["uid"] = "other-namespace"
	refused(t, a, "replaced")
	for index := range []string{"namespace", "volume", "backend"} {
		b := newAPI()
		b.replaceAt = index + 1
		output, err := runCleanup(t, b)
		if err == nil || b.replaced || strings.Contains(output, "=COMPLETE") {
			t.Fatalf("replacement %d was not preserved: err=%v output=%s", index, err, output)
		}
	}
}

func TestFailedReadsNeverClearCleanup(t *testing.T) {
	for _, code := range []int{403, 500} {
		a := newAPI()
		a.readFailure = code
		output, err := runCleanup(t, a)
		if err == nil || a.deletes != 0 || strings.Contains(output, "=COMPLETE") || strings.Contains(output, "=VERIFIED") {
			t.Fatalf("failed read became clearance: code=%d err=%v deletes=%d output=%s", code, err, a.deletes, output)
		}
	}
}
