package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPublicationSubjectBindingsRejectEachRetarget(t *testing.T) {
	publisher := repoFile(t, ".github/actions/deploy-prod/publish-platform-manifests/action.yml")
	for _, id := range []string{"cosign_sign", "generate_sbom", "attest_sbom", "attest_provenance", "verify_evidence", "verify_matcher_accepts_staged", "promote_latest"} {
		t.Run(id, func(t *testing.T) {
			var action map[string]any
			if err := yaml.Unmarshal([]byte(publisher), &action); err != nil {
				t.Fatal(err)
			}
			changed := false
			for _, raw := range action["runs"].(map[string]any)["steps"].([]any) {
				step := raw.(map[string]any)
				if step["id"] != id {
					continue
				}
				mapping, key := "env", "SUBJECT_NAME"
				if strings.HasPrefix(id, "attest_") {
					mapping, key = "with", "subject-name"
				}
				values := step[mapping].(map[string]any)
				if values[key] != "${{ steps.staging_reference.outputs.subject_name }}" {
					t.Fatal("fixture no longer targets the real subject binding")
				}
				values[key] = "example.invalid/wrong-artifact"
				changed = true
			}
			if !changed {
				t.Fatal("retarget mutation changed no real producer")
			}
			data, err := yaml.Marshal(action)
			if err != nil {
				t.Fatal(err)
			}
			if err := validatePublicationAction(string(data)); err == nil || !strings.Contains(err.Error(), "shared subject") {
				t.Fatalf("retargeted %s was not refused by its subject binding: %v", id, err)
			}
		})
	}
	renamed := strings.Replace(publisher, "SUBJECT_NAME: ghcr.io/devantler-tech/platform/manifests", "SUBJECT_NAME: example.invalid/renamed-artifact", 1)
	if renamed == publisher {
		t.Fatal("single-definition control changed nothing")
	}
	if err := validatePublicationAction(renamed); err != nil {
		t.Fatalf("one-definition rename broke shared wiring: %v", err)
	}
}

func TestPublicationSubjectOutputsCannotBeOverwrittenOrSkipped(t *testing.T) {
	publisher := repoFile(t, ".github/actions/deploy-prod/publish-platform-manifests/action.yml")
	anchor := `        echo "registry_ref=${SUBJECT_NAME}:${STAGING_TAG}" >>"${GITHUB_OUTPUT}"`
	for name, injection := range map[string]string{
		"overwritten output": `echo "subject_name=example.invalid/other" >>"${GITHUB_OUTPUT}"`,
		"reassigned subject": `SUBJECT_NAME=example.invalid/other`,
		"early exit":         `exit 0`,
		"heredoc decoy":      "cat <<'EOF'",
	} {
		t.Run(name, func(t *testing.T) {
			mutant := strings.Replace(publisher, anchor, "        "+injection+"\n"+anchor, 1)
			if name == "overwritten output" {
				mutant = strings.Replace(publisher, anchor, anchor+"\n        "+injection, 1)
			}
			if name == "heredoc decoy" {
				mutant = strings.Replace(mutant, anchor, anchor+"\n        EOF", 1)
			}
			if mutant == publisher {
				t.Fatal("staging mutation changed nothing")
			}
			if err := validatePublicationAction(mutant); err == nil || !strings.Contains(err.Error(), "shared subject") {
				t.Fatalf("staging output integrity mutation was accepted: %v", err)
			}
		})
	}
}
