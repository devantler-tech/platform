package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRawYAMLAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"ordinary documents", "kind: WebApp\nspec: {name: fixture}\n---\nkind: Service\n", true},
		{"duplicate identity", "kind: WebApp\nspec:\n  runAsUser: 0\n  runAsUser: 65532\n", false},
		{"duplicate object", "spec: {name: first}\nspec: {name: second}\n", false},
		{"JSON duplicate", `{"spec":{"runAsUser":0,"runAsUser":65532}}`, false},
		{"trailing malformed", "kind: WebApp\n---\n[", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkYAML([]byte(tc.input)); (got == nil) != tc.valid {
				t.Fatalf("raw-source admission differs: %v", got)
			}
		})
	}
}

func TestCompleteJSONBoundary(t *testing.T) {
	valid := `{"apiVersion":"kro.run/v1alpha1","kind":"WebApp","metadata":{"name":"fixture"},"spec":{"name":"fixture","image":"registry.k8s.io/pause:3.10.2","host":"fixture.invalid"}}`
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"positive actual contract", valid, true},
		{"duplicate object", strings.TrimSuffix(valid, "}") + `,"spec":{"name":"fixture","image":"registry.k8s.io/pause:3.10.2","host":"fixture.invalid"}}`, false},
		{"trailing document", valid + valid, false},
		{"unknown override", strings.TrimSuffix(valid, "}") + `,"command":["sh"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := run([]string{"json", "-", "../webapp-inputs.jq"}, strings.NewReader(tc.input)); (err == nil) != tc.valid {
				t.Fatalf("complete admission differs: %v", err)
			}
		})
	}
	if err := run([]string{"yaml", "-"}, bytes.NewReader(bytes.Repeat([]byte("x"), maxSourceBytes+1))); err == nil {
		t.Fatal("oversized input accepted")
	}
	if err := checkYAML([]byte(strings.Repeat("kind: Service\n---\n", 1025))); err == nil {
		t.Fatal("unbounded raw document count accepted")
	}
}

func TestUnavailableValidatorNeverPasses(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := checkJSON([]byte(`{}`), "../webapp-inputs.jq"); err == nil {
		t.Fatal("missing contract evaluator accepted source")
	}
}

func TestArgumentShapes(t *testing.T) {
	for _, args := range [][]string{
		nil, {"yaml"}, {"json"}, {"json", "-"},
		{"yaml", "-", "../webapp-inputs.jq"},
		{"json", "-", "../webapp-inputs.jq", "extra"},
		{"unknown", "-"}, {"unknown", "-", "../webapp-inputs.jq"},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			input := strings.NewReader("kind: Service\n")
			if err := run(args, input); err == nil {
				t.Fatal("unsupported argument shape admitted")
			}
			if input.Len() != len("kind: Service\n") {
				t.Fatal("invalid arguments consumed candidate source")
			}
		})
	}
	if err := run([]string{"yaml", "-"}, strings.NewReader("kind: Service\n")); err != nil {
		t.Fatal("valid YAML invocation refused")
	}
}
