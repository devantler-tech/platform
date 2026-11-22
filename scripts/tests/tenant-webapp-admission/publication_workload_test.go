package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	registrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func publicationPolicy(t *testing.T, name string) *policiesv1beta1.ValidatingPolicy {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../../k8s/bases/infrastructure/cluster-policies/best-practices", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var policy policiesv1beta1.ValidatingPolicy
	if err := yaml.UnmarshalStrict(data, &policy); err != nil {
		t.Fatal(err)
	}
	return &policy
}

func TestPublicationDisruptionBudgetUsesActualPolicy(t *testing.T) {
	policy := publicationPolicy(t, "validate-pdb-drain-safe")
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "policy/v1", "kind": "PodDisruptionBudget",
		"metadata": map[string]any{"name": "business-site", "namespace": "business-site"},
		"spec":     map[string]any{"maxUnavailable": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"app": "business-site"}}},
	}}
	for _, control := range []string{"pass", "denial", "wrong group", "unexpected skip"} {
		t.Run(control, func(t *testing.T) {
			p := policy.DeepCopy()
			f := fixture{Label: control, Policy: p.Name, Actor: "synthetic", Object: object.DeepCopy(), Operation: "CREATE", Expected: "pass"}
			switch control {
			case "denial":
				spec := f.Object.Object["spec"].(map[string]any)
				delete(spec, "maxUnavailable")
				spec["minAvailable"] = int64(1)
				f.Expected = "fail"
			case "wrong group":
				f.Object.SetAPIVersion("other.invalid/v1")
			case "unexpected skip":
				p.Spec.MatchConditions = []registrationv1.MatchCondition{{Name: "synthetic-skip", Expression: "false"}}
			}
			err := evaluate(f, map[string]*policiesv1beta1.ValidatingPolicy{p.Name: p})
			if (err != nil) != (control == "wrong group" || control == "unexpected skip") {
				t.Fatalf("unexpected budget decision: %v", err)
			}
		})
	}
}

// These policies are authored for Pods. The actual Kyverno provider must derive
// and enforce their Job/CronJob equivalents, including refusal of a bad image.
func TestPublicationBatchPoliciesUseActualAutogen(t *testing.T) {
	for _, name := range []string{"disallow-floating-image-tags", "disallow-latest-tag"} {
		for _, kind := range []string{"Job", "CronJob"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				policy := publicationPolicy(t, name)
				for _, denied := range []bool{false, true} {
					image := "ghcr.io/devantler-tech/business-site@sha256:" + strings.Repeat("a", 64)
					expected := "pass"
					if denied {
						image, expected = "ghcr.io/devantler-tech/business-site:latest", "fail"
					}
					spec := map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "synthetic", "image": image}}}}}
					if kind == "CronJob" {
						spec = map[string]any{"jobTemplate": map[string]any{"spec": spec}}
					}
					object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "batch/v1", "kind": kind,
						"metadata": map[string]any{"name": "synthetic", "namespace": "business-site"}, "spec": spec}}
					f := fixture{Label: "actual autogen", Policy: name, Actor: "synthetic", Object: object, Operation: "CREATE", Expected: expected}
					if err := evaluate(f, map[string]*policiesv1beta1.ValidatingPolicy{name: policy}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
