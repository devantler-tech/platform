package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Approve only the exact source-only typed instance, never its future app or
// capabilities. The full ordinary validator must reject each widened intent.
func TestBusinessSourceOnlyAuthorization(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)
	if err := validateAuthorization(role, boundary, rendered); err != nil {
		t.Fatalf("reviewed source-only registration refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		remove bool
		mutate func(map[string]any)
	}{
		{name: "source removed", remove: true},
		{name: "application reconciliation", mutate: func(spec map[string]any) { spec["source"].(map[string]any)["reconcileApplication"] = true }},
		{name: "webapp capability", mutate: func(spec map[string]any) { spec["webApp"] = true }},
		{name: "external DNS", mutate: func(spec map[string]any) { spec["externalDns"] = true }},
		{name: "SOPS", mutate: func(spec map[string]any) { spec["sops"] = true }},
		{name: "different publisher", mutate: func(spec map[string]any) {
			spec["source"].(map[string]any)["publisherRevision"] = strings.Repeat("1", 40)
		}},
		{name: "different artifact", mutate: func(spec map[string]any) {
			spec["source"].(map[string]any)["digest"] = "sha256:" + strings.Repeat("1", 64)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			documents, err := decodeDocuments(rendered)
			if err != nil {
				t.Fatal(err)
			}
			var changed bytes.Buffer
			encoder := yaml.NewEncoder(&changed)
			selected := 0
			for _, document := range documents {
				identity := identityOf(document)
				if identity == (resourceIdentity{apiVersion: "kro.run/v1alpha1", kind: "Tenant", namespace: "flux-system", name: "business-site"}) {
					selected++
					if tc.remove {
						continue
					}
					tc.mutate(document["spec"].(map[string]any))
				}
				if err := encoder.Encode(document); err != nil {
					t.Fatal(err)
				}
			}
			if err := encoder.Close(); err != nil {
				t.Fatal(err)
			}
			if selected != 1 {
				t.Fatalf("selected %d source-only registrations, want one", selected)
			}
			var mismatch *surfaceMismatchError
			if err := validateAuthorization(role, boundary, changed.Bytes()); !errors.As(err, &mismatch) {
				t.Fatalf("changed source-only authorization error = %v, want ledger mismatch", err)
			}
			for _, line := range mismatch.delta {
				if !strings.Contains(line, "kro.run/v1alpha1|Tenant|flux-system|business-site ") {
					t.Fatalf("source-only mutation changed an unrelated authorization: %s", line)
				}
			}
		})
	}
}
