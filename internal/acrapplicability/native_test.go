package acrapplicability

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devantler-tech/platform/internal/scansnapshot"
)

func TestNativeACRApplicabilityAndPairedSSA(t *testing.T) {
	if os.Getenv("PLATFORM_ACR_NATIVE_PROOF") != "1" {
		t.Skip("optional native diagnostic; production Prove always performs both native analyses")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	snapshot, cleanup, err := scansnapshot.Clone(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	proof, proofErr := Prove(ctx, snapshot.Root)
	if dir := os.Getenv("PLATFORM_ACR_EVIDENCE_DIR"); dir != "" {
		work, e := os.MkdirTemp(dir, "native-proof-")
		if e != nil {
			t.Fatal(e)
		}
		meta, e := json.MarshalIndent(proof, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		for name, data := range map[string][]byte{"proof.json": meta, "original.jsons": proof.OriginalJSON, "original.stderr": proof.OriginalStderr, "paired.jsons": proof.PairedJSON, "paired.stderr": proof.PairedStderr, "packages.jsons": proof.GoListJSON, "packages.stderr": proof.GoListStderr, "paired-packages.jsons": proof.PairedGoListJSON, "paired-packages.stderr": proof.PairedGoListStderr, "tool-install.stderr": proof.ToolInstallStderr, "advisory.json": proof.AdvisoryJSON, "index.json": proof.DatabaseIndexJSON} {
			if e := os.WriteFile(filepath.Join(work, name), data, 0600); e != nil {
				t.Fatal(e)
			}
		}
		t.Logf("private native evidence: %s", work)
	}
	if proofErr != nil {
		t.Fatal(proofErr)
	}
	if err := snapshot.Verify(); err != nil {
		t.Fatal(err)
	}
	if proof.Status != "not_affected" || proof.Justification != "vulnerable_code_not_in_execute_path" || len(proof.OriginalJSON) == 0 || len(proof.PairedJSON) == 0 {
		t.Fatal("native proof lacks the narrow disposition or reports")
	}
}

func TestReplayCapturedNativeProtocol(t *testing.T) {
	dir := os.Getenv("PLATFORM_ACR_NATIVE_REPLAY")
	if dir == "" {
		t.Skip("optional parser check of retained native evidence")
	}
	data, err := os.ReadFile(filepath.Join(dir, "proof.json"))
	if err != nil {
		t.Fatal(err)
	}
	var proof Proof
	if err := json.Unmarshal(data, &proof); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeReport(proof.OriginalJSON, proof.AdvisoryJSON, proof.DatabaseModified, false, proof.Roots); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeReport(proof.PairedJSON, proof.AdvisoryJSON, proof.DatabaseModified, true, proof.Roots); err != nil {
		t.Fatal(err)
	}
}
