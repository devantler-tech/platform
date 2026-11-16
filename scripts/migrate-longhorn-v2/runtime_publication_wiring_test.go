package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func runtimePublicationWiring(raw []byte) bool {
	var composite struct {
		Runs struct {
			Steps []struct {
				ID       string            `json:"id"`
				Uses     string            `json:"uses"`
				Run      string            `json:"run"`
				Shell    string            `json:"shell"`
				If       string            `json:"if"`
				Continue any               `json:"continue-on-error"`
				Env      map[string]string `json:"env"`
			} `json:"steps"`
		} `json:"runs"`
	}
	if yaml.Unmarshal(raw, &composite) != nil {
		return false
	}
	positions := map[string]int{}
	goPosition := -1
	for i, step := range composite.Runs.Steps {
		if step.ID != "" {
			if _, exists := positions[step.ID]; exists {
				return false
			}
			positions[step.ID] = i
		}
		if strings.HasPrefix(step.Uses, "actions/setup-go@") {
			goPosition = i
		}
		if step.ID == "runtime_inputs" || step.ID == "runtime_capacity_gate" {
			operation := runtimePublicationCaptureOperation
			if step.ID == "runtime_capacity_gate" {
				operation = runtimePublicationCheckOperation
			}
			if step.If != "" || step.Continue != nil || step.Uses != "" || step.Shell != "bash" || strings.TrimSpace(step.Run) != "go run ./scripts/migrate-longhorn-v2" || step.Env["LONGHORN_V2_OPERATION"] != operation ||
				step.Env["LONGHORN_RUNTIME_RECOVERY_SOURCE"] != "${{ inputs.recovery-source-sha }}" {
				return false
			}
			if step.ID == "runtime_capacity_gate" && step.Env["LONGHORN_RUNTIME_INPUT_FINGERPRINT"] != "${{ steps.runtime_inputs.outputs.source_fingerprint }}" {
				return false
			}
		}
	}
	for _, id := range []string{"runtime_inputs", "push_staging", "verify_evidence", "verify_matcher_accepts_staged", "runtime_capacity_gate", "promote_latest"} {
		if _, exists := positions[id]; !exists {
			return false
		}
	}
	return goPosition >= 0 && goPosition < positions["runtime_inputs"] && positions["runtime_inputs"] < positions["push_staging"] &&
		positions["verify_evidence"] < positions["runtime_capacity_gate"] && positions["verify_matcher_accepts_staged"] < positions["runtime_capacity_gate"] &&
		positions["runtime_capacity_gate"]+1 == positions["promote_latest"]
}

func TestRuntimePublicationCannotBeSkippedBeforeMutablePromotion(t *testing.T) {
	raw, err := os.ReadFile("../../.github/actions/deploy-prod/publish-platform-manifests/action.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !runtimePublicationWiring(raw) {
		t.Fatal("publisher lacks the unconditional input-bound capacity gate")
	}
	for _, bad := range []string{
		strings.ReplaceAll(string(raw), "id: runtime_capacity_gate", "id: removed_capacity_gate"),
		strings.ReplaceAll(string(raw), "id: runtime_capacity_gate", "id: runtime_capacity_gate\n      if: success()"),
		strings.ReplaceAll(string(raw), "id: runtime_capacity_gate", "id: runtime_capacity_gate\n      continue-on-error: true"),
		strings.ReplaceAll(string(raw), "${{ steps.runtime_inputs.outputs.source_fingerprint }}", "unbound"),
		strings.ReplaceAll(string(raw), "check-runtime-publication", "inventory-v1-runtime"),
	} {
		if runtimePublicationWiring([]byte(bad)) {
			t.Fatal("publisher bypass accepted")
		}
	}
}

func TestRuntimePublicationReviewedComponentHasNoAccountingChanges(t *testing.T) {
	raw, err := os.ReadFile("../../k8s/bases/components/longhorn-v1-retirement-runtime/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !runtimePublicationProfile(raw) {
		t.Fatal("runtime component differs from the isolated reviewed image/strategy profile")
	}
	for _, bad := range []string{strings.ReplaceAll(string(raw), "maxUnavailable: 1", "maxUnavailable: 2"), strings.ReplaceAll(string(raw), "longhornManager:", "defaultSettings: {guaranteedInstanceManagerCPU: 20}\n          longhornManager:")} {
		if runtimePublicationProfile([]byte(bad)) {
			t.Fatal("unreviewed rollout or accounting change accepted")
		}
	}
}
