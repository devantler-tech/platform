package kubescaperuleschema_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestRulesSchema(t *testing.T) {
	data, err := os.ReadFile("../../../schemas/kubescape.io/rules_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	const schemaURL = "https://platform.invalid/schemas/rules_v1.json"
	if err := compiler.AddResource(schemaURL, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{"profile projection object", func(r map[string]any) {
			r["profileDataRequired"] = map[string]any{"execs": []any{"paths"}}
		}, true},
		{"optional projection", func(map[string]any) {}, true},
		{"projection wrong type", func(r map[string]any) { r["profileDataRequired"] = "all" }, false},
		{"unknown rule field", func(r map[string]any) { r["unrecognized"] = true }, false},
		{"invalid dependency", func(r map[string]any) { r["profileDependency"] = float64(3) }, false},
		{"missing required field", func(r map[string]any) { delete(r, "enabled") }, false},
		{"unknown expression field", func(r map[string]any) {
			r["expressions"].(map[string]any)["unrecognized"] = true
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := map[string]any{
				"enabled": true, "id": "R2001", "name": "Port forward to pod",
				"description": "Port-forward baseline", "profileDependency": float64(2),
				"severity": float64(5), "supportPolicy": false, "isTriggerAlert": true,
				"mitreTechnique": "T1572", "mitreTactic": "TA0011",
				"expressions": map[string]any{
					"message": "port forward", "uniqueId": "event.ID",
					"ruleExpression": []any{map[string]any{
						"eventType": "k8s-admission", "expression": "event.Kind == \"PodPortForwardOptions\"",
					}},
				},
			}
			tc.change(rule)
			resource := map[string]any{
				"apiVersion": "kubescape.io/v1", "kind": "Rules",
				"metadata": map[string]any{"name": "fixture", "namespace": "kubescape"},
				"spec":     map[string]any{"rules": []any{rule}},
			}
			err := schema.Validate(resource)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, validation error=%v", tc.valid, err)
			}
		})
	}
}
