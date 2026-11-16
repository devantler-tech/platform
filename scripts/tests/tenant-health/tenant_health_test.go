// Package tenanthealth_test pins ADR 0019's blast-radius rule (monorepo#5094):
// a tenant's own health never fails the platform's `apps` layer, and with it
// the merge-queue deploy, while the platform's own workloads stay gated.
package tenanthealth_test

import (
	"bytes"
	"errors"
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
func healthExpression(t *testing.T, spec map[string]any, apiGroup, kind string) (string, bool) {
	t.Helper()
	checks, _ := spec["healthCheckExprs"].([]any)
	for _, item := range checks {
		check, _ := item.(map[string]any)
		if group(check["apiVersion"]) == apiGroup && check["kind"] == kind {
			current, _ := check["current"].(string)
			_, failed := check["failed"]
			_, inProgress := check["inProgress"]
			return current, failed || inProgress
		}
	}
	return "", false
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

func TestFailingTenantDoesNotFailApps(t *testing.T) {
	spec := appsKustomization(t)
	if spec["wait"] != true {
		t.Fatal("apps must keep waiting on the platform's own workloads")
	}
	failing := map[string]any{
		"metadata": map[string]any{"name": "business-site", "namespace": "business-site", "generation": 7},
		"spec":     map[string]any{},
		"status": map[string]any{
			"observedGeneration": 6,
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "False", "reason": "ReconciliationFailed"},
				map[string]any{"type": "Stalled", "status": "True", "reason": "VerificationError"},
			},
		},
	}
	for _, kind := range []struct{ group, kind string }{
		{"kustomize.toolkit.fluxcd.io", "Kustomization"},
		{"source.toolkit.fluxcd.io", "OCIRepository"},
		{"source.toolkit.fluxcd.io", "GitRepository"},
	} {
		expression, other := healthExpression(t, spec, kind.group, kind.kind)
		if expression == "" {
			t.Errorf("apps must decide %s health itself instead of waiting for the tenant", kind.kind)
			continue
		}
		if other {
			t.Errorf("a %s failed or inProgress expression would let a tenant fail apps again", kind.kind)
		}
		if !evaluate(t, expression, failing) {
			t.Errorf("a failing tenant %s must count as healthy for apps", kind.kind)
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
