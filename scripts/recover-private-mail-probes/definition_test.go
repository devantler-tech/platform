package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func reviewedProbePatch() ([]map[string]any, checks) {
	p := func() *probe {
		return &probe{Exec: &struct{ Command []string }{[]string{"/usr/bin/env", "-i", "probe"}}, TimeoutSeconds: 2}
	}
	want := checks{p(), p(), p()}
	want.Startup.PeriodSeconds, want.Startup.FailureThreshold = 5, 24
	spec := podSpec{Containers: []container{{Name: "private-mail", StartupProbe: want.Startup, ReadinessProbe: want.Ready, LivenessProbe: want.Live}}}
	return []map[string]any{
		{"op": "test", "path": "/apiVersion", "value": "apps/v1"},
		{"op": "test", "path": "/kind", "value": "StatefulSet"},
		{"op": "test", "path": "/metadata/name", "value": "private-mail"},
		{"op": "test", "path": "/metadata/namespace", "value": "private-mail"},
		{"op": "replace", "path": "/spec/template/spec", "value": spec},
	}, want
}

func TestReviewedProbeOperationsRetainExactIdentityAndChecks(t *testing.T) {
	operations, want := reviewedProbePatch()
	data, err := json.Marshal(operations)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loadChecks(data)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("guarded patch changed reviewed checks: %v", err)
	}
}

func TestReviewedProbeOperationsRefuseAmbiguity(t *testing.T) {
	for _, name := range []string{"version", "kind", "name", "namespace", "guard-op", "replace-op", "path", "unknown-field", "extra-operation", "missing-operation", "second-document", "invalid-spec"} {
		t.Run(name, func(t *testing.T) {
			operations, _ := reviewedProbePatch()
			switch name {
			case "version":
				operations[0]["value"] = "foreign/v1"
			case "kind":
				operations[1]["value"] = "Deployment"
			case "name":
				operations[2]["value"] = "foreign"
			case "namespace":
				operations[3]["value"] = "foreign"
			case "guard-op":
				operations[2]["op"] = "add"
			case "replace-op":
				operations[4]["op"] = "add"
			case "path":
				operations[4]["path"] = "/spec/template"
			case "unknown-field":
				operations[4]["from"] = "/foreign"
			case "extra-operation":
				operations = append(operations, operations[4])
			case "missing-operation":
				operations = operations[1:]
			case "invalid-spec":
				operations[4]["value"] = "unreadable"
			}
			data, err := json.Marshal(operations)
			if err != nil {
				t.Fatal(err)
			}
			if name == "second-document" {
				data = append(data, []byte("\n---\n[]\n")...)
			}
			if _, err := loadChecks(data); err == nil {
				t.Fatal("ambiguous patch accepted as reviewed probe evidence")
			}
		})
	}
}
