package main

import (
	"fmt"
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// These real engine requests fail if a batch workload is missing, mapped to the
// wrong plural/GVR, or silently treated as an evaluated pass after a skip.
func TestBatchWorkloadDecisionBoundaries(t *testing.T) {
	for _, workload := range []struct{ kind, resource string }{{"Job", "jobs"}, {"CronJob", "cronjobs"}} {
		t.Run(workload.kind, func(t *testing.T) {
			var base policiesv1beta1.ValidatingPolicy
			if err := yaml.UnmarshalStrict([]byte(fmt.Sprintf(`apiVersion: policies.kyverno.io/v1
kind: ValidatingPolicy
metadata: {name: batch-control}
spec:
  failurePolicy: Fail
  validationActions: [Deny]
  matchConstraints:
    resourceRules:
      - apiGroups: [batch]
        apiVersions: [v1]
        operations: [CREATE]
        resources: [%s]
  matchConditions:
    - name: synthetic-selector
      expression: "true"
  validations:
    - expression: "object.spec.allowed == true"
      message: synthetic batch denial
`, workload.resource)), &base); err != nil {
				t.Fatal(err)
			}
			for _, control := range []string{"pass", "denial", "denial is not pass", "pass is not denial", "nonmatch is not pass", "skip is not pass", "wrong group", "wrong version"} {
				t.Run(control, func(t *testing.T) {
					policy := base.DeepCopy()
					object := &unstructured.Unstructured{Object: map[string]any{
						"apiVersion": "batch/v1", "kind": workload.kind,
						"metadata": map[string]any{"name": "synthetic", "namespace": "synthetic"},
						"spec":     map[string]any{"allowed": true},
					}}
					f := fixture{Label: control, Policy: "batch-control", Actor: "synthetic", Object: object, Operation: "CREATE", Expected: "pass"}
					wantError := control != "pass" && control != "denial"
					switch control {
					case "denial", "denial is not pass":
						object.Object["spec"].(map[string]any)["allowed"] = false
						if control == "denial" {
							f.Expected = "fail"
						}
					case "pass is not denial":
						f.Expected = "fail"
					case "nonmatch is not pass":
						policy.Spec.MatchConstraints.ResourceRules[0].Resources = []string{"pods"}
					case "skip is not pass":
						policy.Spec.MatchConditions[0].Expression = "false"
					case "wrong group":
						object.SetAPIVersion("other.invalid/v1")
					case "wrong version":
						object.SetAPIVersion("batch/v1beta1")
					}
					err := evaluate(f, map[string]*policiesv1beta1.ValidatingPolicy{"batch-control": policy})
					if (err != nil) != wantError {
						t.Fatalf("want error=%v, got %v", wantError, err)
					}
				})
			}
		})
	}
}
