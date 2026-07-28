package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validAction is the publication sequence in the order the guarantee requires.
// Each test below moves or removes exactly one step, so a failure names the
// property that broke.
const validAction = `
runs:
  using: composite
  steps:
    - name: Push
      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push
    - name: Sign
      id: cosign-sign
      run: |
        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')
        REF="ghcr.io/devantler-tech/platform/manifests@${DIGEST}"
        cosign sign --yes --recursive "${REF}"
    - name: Attest SBOM
      uses: actions/attest@59d89421af93a897026c735860bf21b6eb4f7b26
    - name: Attest provenance
      uses: actions/attest-build-provenance@a2bbfa25375fe432b6a289bc6b6cd05ecd0c4c32
    - name: Reconcile
      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile
`

func mustChange(t *testing.T, before, after string) {
	t.Helper()

	if before == after {
		t.Fatal("fixture did not change; the test would pass vacuously")
	}
}

func TestValidActionPasses(t *testing.T) {
	if err := validate([]byte(validAction)); err != nil {
		t.Fatalf("expected the reference sequence to satisfy the contract, got: %v", err)
	}
}

const (
	reconcileStep = `    - name: Reconcile
      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile
`
	sbomStep = `    - name: Attest SBOM
      uses: actions/attest@59d89421af93a897026c735860bf21b6eb4f7b26
`
	provenanceStep = `    - name: Attest provenance
      uses: actions/attest-build-provenance@a2bbfa25375fe432b6a289bc6b6cd05ecd0c4c32
`
)

// TestRejectsReconcileBeforeAttestations is the defect the check exists for:
// telling Flux to look before the evidence is complete. Moving the reconcile
// up is exactly the edit that would read as "fail faster" in review.
func TestRejectsReconcileBeforeAttestations(t *testing.T) {
	broken := strings.Replace(validAction, reconcileStep, "", 1)
	mustChange(t, validAction, broken)

	broken = strings.Replace(broken, sbomStep, reconcileStep+sbomStep, 1)

	err := validate([]byte(broken))
	if err == nil {
		t.Fatal("expected reconcile-before-attestation to be rejected")
	}

	if !strings.Contains(err.Error(), "reconcile") {
		t.Errorf("error should name the release step, got: %v", err)
	}
}

// TestAllowsSwappingTheTwoAttestations pins a deliberate freedom. Their
// relative order carries no guarantee, so constraining it would fail CI on a
// legitimate edit and teach people to route around this check.
func TestAllowsSwappingTheTwoAttestations(t *testing.T) {
	swapped := strings.Replace(validAction, sbomStep+provenanceStep, provenanceStep+sbomStep, 1)
	mustChange(t, validAction, swapped)

	if err := validate([]byte(swapped)); err != nil {
		t.Fatalf("the two attestations may appear in either order, got: %v", err)
	}
}

func TestRejectsSigningBeforePublishing(t *testing.T) {
	// Signing first would sign whatever the tag pointed at previously.
	broken := strings.Replace(validAction,
		"    - name: Push\n      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push\n",
		"", 1)
	mustChange(t, validAction, broken)

	broken = strings.Replace(broken,
		"    - name: Attest SBOM",
		"    - name: Push\n      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push\n    - name: Attest SBOM", 1)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected signing before publishing to be rejected")
	}
}

func TestRejectsMissingSigningStep(t *testing.T) {
	broken := strings.Replace(validAction, `cosign sign --yes --recursive "${REF}"`, "echo skip", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a publication with no signing step to be rejected")
	}
}

func TestRejectsMissingProvenanceAttestation(t *testing.T) {
	broken := strings.Replace(validAction,
		"      uses: actions/attest-build-provenance@a2bbfa25375fe432b6a289bc6b6cd05ecd0c4c32\n",
		"      uses: actions/checkout@v7\n", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a missing provenance attestation to be rejected")
	}
}

func TestRejectsMissingReconcileStep(t *testing.T) {
	broken := strings.Replace(validAction, "workload reconcile", "workload noop", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a missing reconcile step to be rejected")
	}
}

// TestRejectsSigningTheMutableTag pins the resolve-then-sign shape: a
// concurrent deploy can move the tag between the two.
func TestRejectsSigningTheMutableTag(t *testing.T) {
	broken := strings.Replace(validAction,
		`        REF="ghcr.io/devantler-tech/platform/manifests@${DIGEST}"`,
		`        REF="ghcr.io/devantler-tech/platform/manifests:latest"`, 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected signing the mutable tag to be rejected")
	}
}

func TestRejectsActionWithNoSteps(t *testing.T) {
	if err := validate([]byte("name: empty\n")); err == nil {
		t.Fatal("expected an action with no steps to be rejected rather than pass vacuously")
	}
}

func TestRejectsUnparseableAction(t *testing.T) {
	if err := validate([]byte("runs: [ unbalanced")); err == nil {
		t.Fatal("expected unparseable YAML to be rejected")
	}
}

func TestRunFailsClosedOnUnreadableFile(t *testing.T) {
	var stderr bytes.Buffer
	if err := run([]string{filepath.Join(t.TempDir(), "absent.yml")}, &stderr); err == nil {
		t.Fatal("expected an unreadable action to fail, not to pass vacuously")
	}
}

func TestRunRequiresAPath(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(nil, &stderr); err == nil {
		t.Fatal("expected no arguments to be a usage error rather than a silent pass")
	}
}

// TestRealDeployActionSatisfiesTheContract pins the shipped composite, so the
// contract cannot be satisfied only by the fixture above.
func TestRealDeployActionSatisfiesTheContract(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", ".github", "actions", "deploy-prod", "action.yml"))
	if err != nil {
		t.Fatalf("could not read the real composite action: %v", err)
	}

	if err := validate(source); err != nil {
		t.Fatalf("the shipped deploy-prod action violates the publication ordering contract: %v", err)
	}
}
