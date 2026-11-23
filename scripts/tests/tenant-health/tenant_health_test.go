// Package tenanthealth_test pins ADR 0019's blast-radius rule (monorepo#5094):
// a tenant's own health never fails the platform's `apps` layer, and with it
// the merge-queue deploy, while the platform's own workloads stay gated.
package tenanthealth_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"cel.dev/cel-go/cel"
	"gopkg.in/yaml.v3"
)

// tenantKinds are the Flux kinds a tenant is delivered through.
var tenantKinds = map[string]bool{
	"kustomize.toolkit.fluxcd.io/Kustomization": true,
	"source.toolkit.fluxcd.io/OCIRepository":    true,
	"source.toolkit.fluxcd.io/GitRepository":    true,
}

func root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func render(t *testing.T, path string) []map[string]any {
	t.Helper()
	out, err := exec.Command("kubectl", "kustomize", filepath.Join(root(), path)).CombinedOutput()
	if err != nil {
		t.Fatalf("%s must render: %v: %s", path, err, out)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	var result []map[string]any
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			return result
		} else if err != nil {
			t.Fatal(err)
		}
		if object != nil {
			result = append(result, object)
		}
	}
}

func group(apiVersion any) string {
	value, _ := apiVersion.(string)
	if index := bytes.IndexByte([]byte(value), '/'); index >= 0 {
		return value[:index]
	}
	return ""
}

func appsKustomization(t *testing.T) map[string]any {
	t.Helper()
	for _, object := range render(t, "k8s/clusters/prod") {
		metadata, _ := object["metadata"].(map[string]any)
		if object["kind"] == "Kustomization" && metadata["name"] == "apps" {
			spec, _ := object["spec"].(map[string]any)
			return spec
		}
	}
	t.Fatal("prod must render the apps Kustomization")
	return nil
}

// healthExpression returns the `current` expression `apps` uses for a kind, and
// whether the entry carries any other expression.
// healthOf evaluates the `apps` health expressions for one object the way Flux does:
// inProgress, then failed, then current; the first that holds decides.
func healthOf(t *testing.T, spec map[string]any, apiGroup, kind string, object map[string]any) string {
	t.Helper()
	checks, _ := spec["healthCheckExprs"].([]any)
	for _, item := range checks {
		check, _ := item.(map[string]any)
		if group(check["apiVersion"]) != apiGroup || check["kind"] != kind {
			continue
		}
		for _, stage := range []struct{ key, result string }{{"inProgress", "InProgress"}, {"failed", "Failed"}, {"current", "Current"}} {
			if expression, ok := check[stage.key].(string); ok && evaluate(t, expression, object) {
				return stage.result
			}
		}
		return "InProgress"
	}
	return "kstatus"
}

func evaluate(t *testing.T, expression string, object map[string]any) bool {
	t.Helper()
	env, err := cel.NewEnv(
		cel.Variable("metadata", cel.DynType),
		cel.Variable("spec", cel.DynType),
		cel.Variable("status", cel.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := program.Eval(object)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := result.Value().(bool)
	return ok && value
}

const tenantSourceLabel = "platform.devantler.tech/tenant-source"

func fluxObject(labelled bool, ready, stalled string, observed int) map[string]any {
	metadata := map[string]any{"name": "fixture", "namespace": "fixture", "generation": 7}
	if labelled {
		metadata["labels"] = map[string]any{tenantSourceLabel: "true"}
	}
	return map[string]any{
		"metadata": metadata,
		"spec":     map[string]any{},
		"status": map[string]any{
			"observedGeneration": observed,
			"conditions": []any{
				map[string]any{"type": "Ready", "status": ready},
				map[string]any{"type": "Stalled", "status": stalled},
			},
		},
	}
}

func TestFailingTenantDoesNotFailApps(t *testing.T) {
	spec := appsKustomization(t)
	if spec["wait"] != true {
		t.Fatal("apps must keep waiting on the platform's own workloads")
	}
	failing := fluxObject(true, "False", "True", 6)
	if got := healthOf(t, spec, "kustomize.toolkit.fluxcd.io", "Kustomization", fluxObject(false, "False", "True", 6)); got != "Current" {
		t.Errorf("a failing tenant Kustomization must count as healthy for apps, got %s", got)
	}
	for _, kind := range []string{"OCIRepository", "GitRepository"} {
		if got := healthOf(t, spec, "source.toolkit.fluxcd.io", kind, failing); got != "Current" {
			t.Errorf("a failing labelled tenant %s must count as healthy for apps, got %s", kind, got)
		}
	}
}

// A platform chart source carries no tenant label and keeps Flux's usual readiness.
func TestPlatformSourcesStayGated(t *testing.T) {
	spec := appsKustomization(t)
	for _, kind := range []string{"OCIRepository", "GitRepository"} {
		for _, tc := range []struct {
			name   string
			object map[string]any
			want   string
		}{
			{"stalled", fluxObject(false, "False", "True", 7), "Failed"},
			{"not yet observed", fluxObject(false, "True", "False", 6), "InProgress"},
			{"failing but retrying", fluxObject(false, "False", "False", 7), "InProgress"},
			{"ready", fluxObject(false, "True", "False", 7), "Current"},
		} {
			if got := healthOf(t, spec, "source.toolkit.fluxcd.io", kind, tc.object); got != tc.want {
				t.Errorf("an unlabelled %s that is %s must be %s for apps, got %s", kind, tc.name, tc.want, got)
			}
		}
	}
}

func TestPlatformReleasesStayGated(t *testing.T) {
	spec := appsKustomization(t)
	checks, _ := spec["healthCheckExprs"].([]any)
	for _, item := range checks {
		check, _ := item.(map[string]any)
		apiGroup := group(check["apiVersion"])
		if apiGroup != "kustomize.toolkit.fluxcd.io" && apiGroup != "source.toolkit.fluxcd.io" && apiGroup != "helm.toolkit.fluxcd.io" {
			continue
		}
		if kind, _ := check["kind"].(string); !tenantKinds[apiGroup+"/"+kind] {
			t.Errorf("only tenant delivery kinds may skip the apps gate; platform HelmReleases and other Flux objects keep theirs, got %s %v", apiGroup, check["kind"])
		}
	}
}

type sourceKey struct{ kind, namespace, name string }

// Every source a tenant Kustomization reads carries the tenant label, and no source a
// platform HelmRelease reads does: the label is what lets a source skip the apps gate.
func TestTenantSourcesAreLabelled(t *testing.T) {
	objects := render(t, "k8s/providers/hetzner/apps")
	labelled := map[sourceKey]bool{}
	for _, object := range objects {
		if group(object["apiVersion"]) != "source.toolkit.fluxcd.io" {
			continue
		}
		metadata, _ := object["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		kind, _ := object["kind"].(string)
		namespace, _ := metadata["namespace"].(string)
		name, _ := metadata["name"].(string)
		labelled[sourceKey{kind, namespace, name}] = labels[tenantSourceLabel] == "true"
	}
	read := map[sourceKey]bool{}
	for _, object := range objects {
		metadata, _ := object["metadata"].(map[string]any)
		namespace, _ := metadata["namespace"].(string)
		spec, _ := object["spec"].(map[string]any)
		switch {
		case group(object["apiVersion"]) == "kustomize.toolkit.fluxcd.io" && object["kind"] == "Kustomization":
			ref, _ := spec["sourceRef"].(map[string]any)
			key := sourceKey{fmt.Sprint(ref["kind"]), namespace, fmt.Sprint(ref["name"])}
			if ns, ok := ref["namespace"].(string); ok {
				key.namespace = ns
			}
			read[key] = true
			if !labelled[key] {
				t.Errorf("tenant Kustomization %s/%v reads %v, which is not a labelled tenant source", namespace, metadata["name"], key)
			}
		case group(object["apiVersion"]) == "helm.toolkit.fluxcd.io" && object["kind"] == "HelmRelease":
			ref, _ := spec["chartRef"].(map[string]any)
			if ref == nil {
				continue
			}
			key := sourceKey{fmt.Sprint(ref["kind"]), namespace, fmt.Sprint(ref["name"])}
			if ns, ok := ref["namespace"].(string); ok {
				key.namespace = ns
			}
			if labelled[key] {
				t.Errorf("platform HelmRelease %s/%v reads %v, which is labelled as a tenant source and would skip the apps gate", namespace, metadata["name"], key)
			}
		}
	}
	for key, isTenant := range labelled {
		if isTenant && !read[key] {
			t.Errorf("%v is labelled as a tenant source but no tenant Kustomization reads it", key)
		}
	}
	if len(read) == 0 {
		t.Fatal("the prod apps layer renders no tenant Kustomization; the census proves nothing")
	}
}

// Every Flux Kustomization the prod apps layer applies must be a tenant: the
// deploy's reconcile skips it, and apps no longer waits on it. A platform
// component added as a nested Kustomization would silently lose its gate.
func TestEveryAppsKustomizationIsATenant(t *testing.T) {
	seen := 0
	for _, object := range render(t, "k8s/providers/hetzner/apps") {
		if group(object["apiVersion"]) != "kustomize.toolkit.fluxcd.io" || object["kind"] != "Kustomization" {
			continue
		}
		seen++
		metadata, _ := object["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations["ksail.devantler.tech/reconcile-exclude"] != "true" {
			t.Errorf("Kustomization %v/%v is not marked as a tenant (ksail.devantler.tech/reconcile-exclude); a platform component must not hide behind the tenant health rule", metadata["namespace"], metadata["name"])
		}
		if metadata["namespace"] == "flux-system" {
			t.Errorf("Kustomization %v in flux-system is a platform layer, not a tenant", metadata["name"])
		}
	}
	if seen == 0 {
		t.Fatal("the prod apps layer renders no tenant Kustomization; the census proves nothing")
	}
}
