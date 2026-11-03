package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"golang.org/x/mod/modfile"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// osvIgnoredID matches an [[IgnoredVulns]] id in osv-scanner.toml. An entry in
// any other form is not matched, so the lists differ and the test fails closed.
var osvIgnoredID = regexp.MustCompile(`(?m)^\s*id\s*=\s*"([^"]+)"\s*$`)

// readOptional returns a file's contents, or nothing when the file is absent.
func readOptional(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if os.IsNotExist(err) {
		return ""
	} else if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// These acceptances adjudicate overbroad database ranges, not an unpatched
// runtime. A dependency update must re-read the upstream ranges before carrying
// them forward. Removing the acceptances after database correction is allowed.
// govulncheck and osv-scanner each read their own list, so both must name the
// same advisories.
func TestAdvisoryAcceptancePinsReviewedRelease(t *testing.T) {
	var accepted, ignored []string
	for _, line := range strings.Split(readOptional(t, ".govulncheck-allow.txt"), "\n") {
		line, _, _ = strings.Cut(line, "#")
		accepted = append(accepted, strings.Fields(line)...)
	}
	for _, match := range osvIgnoredID.FindAllStringSubmatch(readOptional(t, "osv-scanner.toml"), -1) {
		ignored = append(ignored, match[1])
	}
	slices.Sort(accepted)
	slices.Sort(ignored)
	if !slices.Equal(accepted, ignored) {
		t.Fatalf("osv-scanner.toml ignores %v but .govulncheck-allow.txt accepts %v", ignored, accepted)
	}
	if len(accepted) == 0 {
		return
	}
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Replace {
		if r.Old.Path == "github.com/kyverno/kyverno" {
			t.Fatal("re-read advisory ranges before replacing the reviewed Kyverno release")
		}
	}
	for _, r := range m.Require {
		if r.Mod.Path == "github.com/kyverno/kyverno" && r.Mod.Version == "v1.19.2" {
			return
		}
	}
	t.Fatal("re-read advisory ranges before carrying the acceptances to another Kyverno release; see platform#5154")
}

func TestAdapterPreservesOldObjectAndSubresource(t *testing.T) {
	var policy policiesv1beta1.ValidatingPolicy
	if err := yaml.UnmarshalStrict([]byte(`apiVersion: policies.kyverno.io/v1
kind: ValidatingPolicy
metadata: {name: adapter-control}
spec:
  validationActions: [Deny]
  matchConstraints:
    resourceRules:
      - apiGroups: [kro.run]
        apiVersions: [v1alpha1]
        operations: [UPDATE]
        resources: [webapps/status]
  validations:
    - expression: "object.spec == oldObject.spec && request.subResource == 'status'"
      message: distinct updates are denied
`), &policy); err != nil {
		t.Fatal(err)
	}
	policies := map[string]*policiesv1beta1.ValidatingPolicy{"adapter-control": &policy}
	old := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kro.run/v1alpha1", "kind": "WebApp",
		"metadata": map[string]any{"name": "fixture", "namespace": "fixture"}, "spec": map[string]any{"value": "before"},
	}}
	changed := old.DeepCopy()
	changed.Object["spec"] = map[string]any{"value": "after"}
	f := fixture{Label: "distinct update", Policy: "adapter-control", Actor: "fixture", Object: changed, OldObject: old, Operation: "UPDATE", Subresource: "status", Expected: "fail"}
	if err := evaluate(f, policies); err != nil {
		t.Fatal(err)
	}
	f.Object, f.Expected = old.DeepCopy(), "pass"
	if err := evaluate(f, policies); err != nil {
		t.Fatal(err)
	}
	f.Subresource = ""
	if err := evaluate(f, policies); err == nil {
		t.Fatal("a non-matching policy must not count as an evaluated control")
	}
	f.Subresource, f.OldObject = "status", nil
	if err := evaluate(f, policies); err == nil {
		t.Fatal("an UPDATE without its old object must be refused")
	}
}
