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

// TestRejectsASecondPushAfterReconcile pins that the order holds across EVERY
// occurrence, not just the first of each kind. Locating one publish step and
// stopping leaves a later `workload push` invisible: the earlier sequence still
// validates while the deploy republishes bytes that nothing signed or attested,
// which is the exact window this guard exists to close.
func TestRejectsASecondPushAfterReconcile(t *testing.T) {
	const latePush = `    - name: Push again
      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push
`

	withLatePush := validAction + latePush
	mustChange(t, validAction, withLatePush)

	if err := validate([]byte(withLatePush)); err == nil {
		t.Fatal("expected a second publish after the reconcile to be rejected")
	}
}

// TestRejectsASecondReconcileBeforeSigning is the mirror: an added release step
// that runs before the evidence must fail even though a correctly-placed
// reconcile also exists later.
func TestRejectsASecondReconcileBeforeSigning(t *testing.T) {
	early := strings.Replace(validAction,
		"    - name: Sign",
		reconcileStep+"    - name: Sign", 1)
	mustChange(t, validAction, early)

	if err := validate([]byte(early)); err == nil {
		t.Fatal("expected a reconcile placed before the signing step to be rejected")
	}
}

// TestRejectsSigningTheMutableTagWhileAssigningTheDigest is the finding that a
// contains-check on the assignment cannot catch: REF is still assigned the
// resolved digest, so the old check passes, but the cosign invocation consumes
// the mutable tag instead. The signature then covers whatever the tag points at
// when cosign resolves it, which is the TOCTOU the digest resolve exists to
// remove. Only the command argument changes here.
func TestRejectsSigningTheMutableTagWhileAssigningTheDigest(t *testing.T) {
	deadAssignment := strings.Replace(validAction,
		`cosign sign --yes --recursive "${REF}"`,
		`cosign sign --yes --recursive "${REF_TAG}"`, 1)
	mustChange(t, validAction, deadAssignment)

	if !strings.Contains(deadAssignment, `REF="ghcr.io/devantler-tech/platform/manifests@${DIGEST}"`) {
		t.Fatal("fixture must keep the digest assignment, or it does not isolate the command argument")
	}

	if err := validate([]byte(deadAssignment)); err == nil {
		t.Fatal("expected signing the mutable tag to be rejected even though REF is assigned the digest")
	}
}

// TestRejectsACommentedOutSigningInvocation covers a hole in the matchers themselves: both
// runContains("cosign sign ") and the digest check work on the raw script text, so commenting the
// invocation out leaves BOTH satisfied — the comment still contains the command and the reference.
// The deploy would then publish and reconcile an artifact nothing signed, while this check stays
// green. A guard that a `#` disables is not a guard.
func TestRejectsACommentedOutSigningInvocation(t *testing.T) {
	commented := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        # cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, commented)

	if err := validate([]byte(commented)); err == nil {
		t.Fatal("expected a commented-out cosign invocation to be rejected")
	}
}

// TestRejectsEvidenceThatCanSkipWhileTheReleaseRuns covers the other way the order can hold while the
// guarantee does not. Ordering is positional, so an evidence step keeps its position while carrying a
// condition that never fires; GitHub skips it without failing the composite and runs the unconditional
// reconcile anyway. Production is then released without that evidence, with every ordering comparison
// still satisfied.
func TestRejectsEvidenceThatCanSkipWhileTheReleaseRuns(t *testing.T) {
	skippable := strings.Replace(validAction,
		"    - name: Attest provenance\n",
		"    - name: Attest provenance\n      if: ${{ false }}\n", 1)
	mustChange(t, validAction, skippable)

	if err := validate([]byte(skippable)); err == nil {
		t.Fatal("expected an evidence step that can skip independently of the release to be rejected")
	}
}

// TestAllowsEvidenceSharingTheReleaseCondition is the companion that keeps the rule from becoming a
// blanket ban on conditions. Evidence guarded by the SAME condition as the release cannot be skipped
// while the release runs — they stand or fall together — so that arrangement stays legal.
func TestAllowsEvidenceSharingTheReleaseCondition(t *testing.T) {
	shared := strings.Replace(validAction,
		"    - name: Attest provenance\n",
		"    - name: Attest provenance\n      if: ${{ inputs.deploy }}\n", 1)
	shared = strings.Replace(shared,
		"    - name: Reconcile\n",
		"    - name: Reconcile\n      if: ${{ inputs.deploy }}\n", 1)
	mustChange(t, validAction, shared)

	if err := validate([]byte(shared)); err != nil {
		t.Fatalf("evidence sharing the release condition must stay legal, got: %v", err)
	}
}

// TestAllowsACommentedAlternativeBesideARealSigningInvocation pins the other half of comment
// handling. Stripping comments before matching is what stops a `#` from disabling the guard; it is
// ALSO what stops a commented-out alternative from failing a composite that signs correctly.
//
// Without it, signsTheResolvedDigest walks the raw text, finds "cosign sign " on the commented line,
// sees no "${REF}" there, and rejects a file whose real invocation is exactly right — a guard that
// fails valid input gets suppressed, which is how the real one stops being enforced.
func TestAllowsACommentedAlternativeBesideARealSigningInvocation(t *testing.T) {
	withAlternative := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        cosign sign --yes --recursive "${REF}"`+"\n"+
			`        # cosign sign --yes --recursive "${REF_TAG}"  # pre-digest form, kept for reference`, 1)
	mustChange(t, validAction, withAlternative)

	if err := validate([]byte(withAlternative)); err != nil {
		t.Fatalf("a commented alternative beside a correct invocation must stay legal, got: %v", err)
	}
}
