package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is the failed-check portion of the actual Checkov 3.3.9 scan of
// cert-approver 0.12.1's pinned ha-install.yaml. Only file_path was normalized.
// Keep the report tied to the committed resource so a vendor update cannot
// silently keep a finding fingerprint for the preceding release.
func TestCommittedCertApproverFindingsRemainCurrent(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "cert-approver-0.12.1-findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report checkovReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	deployment, err := os.ReadFile(filepath.Join("..", "..", "k8s", "providers", "hetzner",
		"infrastructure", "controllers", "kubelet-serving-cert-approver", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := stripConfiguredDispositions(string(deployment), bundleTargets["kubelet-serving-cert-approver"])
	if err != nil {
		t.Fatal(err)
	}
	source = strings.TrimPrefix(source, "---\n")
	found := false
	for _, finding := range report.Results.FailedChecks {
		if finding.CheckID != "CKV_K8S_38" {
			continue
		}
		found = true
		var block strings.Builder
		for _, line := range finding.CodeBlock {
			text, ok := line[1].(string)
			if !ok {
				t.Fatal("captured finding has a non-text code line")
			}
			block.WriteString(text)
		}
		if block.String() != source {
			t.Fatal("captured Checkov finding no longer describes the committed Deployment; refresh the actual scan")
		}
	}
	if !found {
		t.Fatal("captured report omits the service-account finding")
	}
	if err := validateCheckovFindings(data, bundleTargets["kubelet-serving-cert-approver"]); err != nil {
		t.Fatalf("the current committed cert-approver findings must be accepted: %v", err)
	}
}
