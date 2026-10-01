package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The trial has five authorization-bearing documents. Its owner binding is
// approved with the literal admin_email substitution, so a different subject,
// rule or reconciliation identity must still move the complete ledger entry.
func TestWorldAtRuinTrialApprovalRejectsRemovalAndPrivilegeChanges(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)
	if err := validateAuthorization(role, boundary, rendered); err != nil {
		t.Fatalf("reviewed trial control failed: %v", err)
	}
	trialKeys := map[string]bool{
		"kustomize.toolkit.fluxcd.io/v1|Kustomization|world-at-ruin|world-at-ruin":                 true,
		"rbac.authorization.k8s.io/v1|RoleBinding|world-at-ruin|world-at-ruin":                     true,
		"rbac.authorization.k8s.io/v1|RoleBinding|world-at-ruin|world-at-ruin-zone-trial-operator": true,
		"rbac.authorization.k8s.io/v1|Role|world-at-ruin|world-at-ruin-zone-trial-operator":        true,
		"source.toolkit.fluxcd.io/v1|OCIRepository|world-at-ruin|world-at-ruin":                    true,
	}
	entries, _, _, err := evaluateRenderedSurface(rendered)
	if err != nil {
		t.Fatal(err)
	}
	selected := 0
	for _, entry := range entries {
		key := surfaceEntryKey(entry)
		if strings.Contains(key, "|world-at-ruin|") {
			if !trialKeys[key] {
				t.Fatalf("unexpected trial authorization identity %s", key)
			}
			selected++
		}
	}
	if selected != len(trialKeys) {
		t.Fatalf("selected trial authorization documents = %d, want %d", selected, len(trialKeys))
	}

	operatorRole := "rbac.authorization.k8s.io/v1|Role|world-at-ruin|world-at-ruin-zone-trial-operator"
	operatorBinding := "rbac.authorization.k8s.io/v1|RoleBinding|world-at-ruin|world-at-ruin-zone-trial-operator"
	for _, tc := range []struct {
		name       string
		remove     bool
		key        string
		mutate     func(map[string]any)
		wantDeltas int
	}{
		{name: "removed trial authorization objects", remove: true, wantDeltas: 5},
		{
			name: "operator can read Secrets", key: operatorRole, wantDeltas: 2,
			mutate: func(document map[string]any) {
				rules, ok := document["rules"].([]any)
				if !ok {
					t.Fatal("operator Role fixture has no rules list")
				}
				document["rules"] = append(rules, map[string]any{
					"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"get"},
				})
			},
		},
		{
			name: "different operator substitution", key: operatorBinding, wantDeltas: 2,
			mutate: func(document map[string]any) {
				subjects, ok := document["subjects"].([]any)
				if !ok || len(subjects) != 1 {
					t.Fatal("operator binding fixture has no unique subject")
				}
				subject, ok := subjects[0].(map[string]any)
				if !ok {
					t.Fatal("operator binding fixture subject is not a mapping")
				}
				subject["name"] = "oidc:${another_admin_email}"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			documents, err := decodeDocuments(rendered)
			if err != nil {
				t.Fatal(err)
			}
			var changed bytes.Buffer
			encoder := yaml.NewEncoder(&changed)
			mutations := 0
			for _, document := range documents {
				identity := identityOf(document)
				key := strings.Join([]string{identity.apiVersion, identity.kind, identity.namespace, identity.name}, "|")
				if tc.remove && trialKeys[key] {
					mutations++
					continue
				}
				if !tc.remove && key == tc.key {
					tc.mutate(document)
					mutations++
				}
				if err := encoder.Encode(document); err != nil {
					t.Fatal(err)
				}
			}
			if err := encoder.Close(); err != nil {
				t.Fatal(err)
			}
			wantMutations := 1
			if tc.remove {
				wantMutations = len(trialKeys)
			}
			if mutations != wantMutations {
				t.Fatalf("fixture changed %d documents, want %d", mutations, wantMutations)
			}
			validationErr := validateAuthorization(role, boundary, changed.Bytes())
			var mismatch *surfaceMismatchError
			if !errors.As(validationErr, &mismatch) {
				t.Fatalf("changed trial surface error = %v, want exact ledger mismatch", validationErr)
			}
			if len(mismatch.delta) != tc.wantDeltas {
				t.Fatalf("changed trial ledger has %d differences, want %d: %v", len(mismatch.delta), tc.wantDeltas, mismatch.delta)
			}
			for _, line := range mismatch.delta {
				if tc.remove {
					fields := strings.Fields(line)
					if fields[0] != "-" || !trialKeys[fields[1]] {
						t.Fatalf("removal changed an unrelated ledger entry: %s", line)
					}
				} else if !strings.HasPrefix(line, "- "+tc.key+" ") && !strings.HasPrefix(line, "+ "+tc.key+" ") {
					t.Fatalf("mutation changed an unrelated ledger entry: %s", line)
				}
			}
		})
	}
}
