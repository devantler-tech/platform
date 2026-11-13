package main

import (
	"strings"
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// lookupControlPolicy reads a Namespace through resource.Get and the
// requester's groups, the two inputs a fence over a retired namespace needs.
// It fails closed, so a lookup that cannot be answered is an evaluation error.
const lookupControlPolicy = `apiVersion: policies.kyverno.io/v1
kind: ValidatingPolicy
metadata: {name: lookup-control}
spec:
  validationActions: [Deny]
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE]
        resources: [secrets]
  variables:
    - name: fence
      expression: "resource.Get('v1', 'namespaces', '', 'fenced')"
  validations:
    - expression: >-
        !(has(variables.fence.metadata.annotations) &&
          variables.fence.metadata.annotations[?'example.test/retired'].orValue('') == 'true') ||
        (has(request.userInfo.groups) && 'system:masters' in request.userInfo.groups)
      message: the fenced namespace is retired
`

func lookupControlPolicies(t *testing.T) map[string]*policiesv1beta1.ValidatingPolicy {
	t.Helper()
	var policy policiesv1beta1.ValidatingPolicy
	if err := yaml.UnmarshalStrict([]byte(lookupControlPolicy), &policy); err != nil {
		t.Fatal(err)
	}
	return map[string]*policiesv1beta1.ValidatingPolicy{"lookup-control": &policy}
}

func fencedNamespace(retired bool) *unstructured.Unstructured {
	namespace := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "fenced"},
	}}
	if retired {
		namespace.SetAnnotations(map[string]string{"example.test/retired": "true"})
	}
	return namespace
}

func lookupControlFixture(label, expected string) fixture {
	return fixture{
		Label: label, Policy: "lookup-control", Actor: "fixture", Operation: "CREATE", Expected: expected,
		Object: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "credential", "namespace": "fenced"},
		}},
	}
}

func TestLookupsGroupsAndFailuresReachThePolicy(t *testing.T) {
	policies := lookupControlPolicies(t)
	cases := []struct {
		label    string
		expected string
		edit     func(*fixture)
	}{
		// The decision follows the served Namespace: retired denies, live allows.
		{"retired namespace denies", "fail", func(f *fixture) { f.Lookups = append(f.Lookups, fencedNamespace(true)) }},
		{"live namespace allows", "pass", func(f *fixture) { f.Lookups = append(f.Lookups, fencedNamespace(false)) }},
		// Groups reach request.userInfo.groups: the same retired read now allows.
		{"administrator group allows", "pass", func(f *fixture) {
			f.Lookups, f.Groups = append(f.Lookups, fencedNamespace(true)), []string{"system:authenticated", "system:masters"}
		}},
		// Each failed read is the policy's own fail-closed error.
		{"absent namespace errors", "error", func(*fixture) {}},
		{"refused read errors", "error", func(f *fixture) { f.LookupFailure = "forbidden" }},
		{"server failure errors", "error", func(f *fixture) { f.LookupFailure = "servererror" }},
	}
	for _, c := range cases {
		f := lookupControlFixture(c.label, c.expected)
		c.edit(&f)
		if err := evaluate(f, policies); err != nil {
			t.Errorf("%s: %v", c.label, err)
		}
	}
}

// Each input must change the result. If the driver dropped one, the opposite
// expectation would pass, so these must be refused.
func TestIgnoredLookupInputsAreDetected(t *testing.T) {
	policies := lookupControlPolicies(t)
	retired := lookupControlFixture("retired namespace must not allow", "pass")
	retired.Lookups = append(retired.Lookups, fencedNamespace(true))
	administrator := lookupControlFixture("administrator group must not deny", "fail")
	administrator.Lookups, administrator.Groups = append(administrator.Lookups, fencedNamespace(true)), []string{"system:masters"}
	failure := lookupControlFixture("server failure must not allow", "pass")
	failure.LookupFailure = "servererror"
	for _, f := range []fixture{retired, administrator, failure} {
		if err := evaluate(f, policies); err == nil {
			t.Errorf("%s: the driver accepted an expectation its input contradicts", f.Label)
		}
	}
}

func TestInvalidLookupFixturesAreRefused(t *testing.T) {
	policies := lookupControlPolicies(t)
	unknownFailure := lookupControlFixture("unknown failure", "error")
	unknownFailure.LookupFailure = "timeout"
	both := lookupControlFixture("served and failed", "error")
	both.Lookups, both.LookupFailure = append(both.Lookups, fencedNamespace(true)), "forbidden"
	wrongKind := lookupControlFixture("unsupported lookup kind", "fail")
	wrongKind.Lookups = append(wrongKind.Lookups, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "fenced"},
	}})
	unknownKind := lookupControlFixture("unsupported object kind", "pass")
	unknownKind.Object.SetKind("Unknown")
	for _, f := range []fixture{unknownFailure, both, wrongKind, unknownKind} {
		err := evaluate(f, policies)
		if err == nil || strings.HasPrefix(err.Error(), "expected ") {
			t.Errorf("%s: want a refused fixture, got %v", f.Label, err)
		}
	}
}
