package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCSICanaryCanBeRenderedWithoutProductionAuthorization(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "run", ".", "--render-csi-canary", "1234")
	cmd.Env = append(os.Environ(), "GITHUB_ACTIONS=false", "LONGHORN_V2_CONFIRM=", "LONGHORN_V2_OPERATION=")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("offline CSI rendering failed: %v", err)
	}
	var rendered struct{ Items []object }
	if json.Unmarshal(output, &rendered) != nil || len(rendered.Items) != 12 || text(rendered.Items[11], "kind") != "VolumeSnapshotClass" || value(rendered.Items[9], "spec", "snapshotMoveData") != true {
		t.Fatal("offline CSI output is not the native Data Mover canary")
	}
}

func TestWorkflowRequiresTheCSIConfirmationAndRejectsTheFSBConfirmation(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/prepare-longhorn-v2-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Run string }
		}
	}
	if err = yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	guard := workflow.Jobs["prepare"].Steps[0].Run
	for _, confirmation := range []string{"verify-longhorn-v2-csi-restore", "verify-longhorn-v2-restore", "prepare-longhorn-v2-prod-worker-1"} {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", guard)
		cmd.Env = append(os.Environ(), "DISPATCH_REF=refs/heads/main", "GITHUB_RUN_ATTEMPT=1", "LONGHORN_V2_OPERATION=canary-csi-v2", "LONGHORN_V2_CONFIRM="+confirmation)
		if err = cmd.Run(); (err == nil) != strings.Contains(confirmation, "-csi-") {
			t.Fatalf("CSI workflow confirmation %s: %v", confirmation, err)
		}
	}
}
