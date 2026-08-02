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
      with:
        subject-digest: ${{ steps.cosign-sign.outputs.digest }}
        push-to-registry: "true"
    - name: Attest provenance
      uses: actions/attest-build-provenance@a2bbfa25375fe432b6a289bc6b6cd05ecd0c4c32
      with:
        subject-digest: ${{ steps.cosign-sign.outputs.digest }}
        push-to-registry: "true"
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
      with:
        subject-digest: ${{ steps.cosign-sign.outputs.digest }}
        push-to-registry: "true"
`
	provenanceStep = `    - name: Attest provenance
      uses: actions/attest-build-provenance@a2bbfa25375fe432b6a289bc6b6cd05ecd0c4c32
      with:
        subject-digest: ${{ steps.cosign-sign.outputs.digest }}
        push-to-registry: "true"
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

// TestRejectsSigningInsideAStaticallyFalseBranch is the last bypass of the matcher family: the step
// carries no `if:`, keeps its position, and builds REF from the resolved digest, so every check above
// is satisfied — while the invocation sits in a branch that never runs. GitHub attests and reconciles
// an unsigned artifact.
func TestRejectsSigningInsideAStaticallyFalseBranch(t *testing.T) {
	nested := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        if false; then\n"+
			`          cosign sign --yes --recursive "${REF}"`+"\n"+
			"        fi", 1)
	mustChange(t, validAction, nested)

	if err := validate([]byte(nested)); err == nil {
		t.Fatal("expected a signing invocation inside a conditional branch to be rejected")
	}
}

// TestRejectsSigningInAOneLineConditional is the same bypass written on one line, where the block
// opens and closes before the line ends. A depth counter alone returns to zero by the end of it, so
// the line would look unnested.
func TestRejectsSigningInAOneLineConditional(t *testing.T) {
	inline := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        if false; then cosign sign --yes --recursive "${REF}"; fi`, 1)
	mustChange(t, validAction, inline)

	if err := validate([]byte(inline)); err == nil {
		t.Fatal("expected a one-line conditional around the signing invocation to be rejected")
	}
}

// TestAllowsControlFlowElsewhereInAStep keeps the rule from banning shell control flow outright.
// Only the required invocations must be unconditional; a retry loop or a precondition check
// alongside them is ordinary scripting and stays legal.
func TestAllowsControlFlowElsewhereInAStep(t *testing.T) {
	withLoop := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		"        for attempt in 1 2 3; do\n"+
			`          DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}') && break`+"\n"+
			"        done", 1)
	mustChange(t, validAction, withLoop)

	if err := validate([]byte(withLoop)); err != nil {
		t.Fatalf("control flow around non-required work must stay legal, got: %v", err)
	}
}

// signingReplacements are the ways a required invocation can appear in the text while not running as
// a plain top-level command. Enumerated together rather than one per review round: they are one
// class, and a matcher that reads raw text is blind to all of them for the same reason.
var signingReplacements = map[string]string{
	"short-circuit &&":     `false && cosign sign --yes --recursive "${REF}"`,
	"short-circuit ||":     `true || cosign sign --yes --recursive "${REF}"`,
	"pipeline":             `printf '' | cosign sign --yes --recursive "${REF}"`,
	"string literal":       `echo "would run: cosign sign --yes --recursive ${REF}"`,
	"command substitution": `RESULT=$(cosign sign --yes --recursive "${REF}")`,
	"uninvoked function":   "sign_it() {\n          cosign sign --yes --recursive \"${REF}\"\n        }",
	"trailing semicolon":   `false; cosign sign --yes --recursive "${REF}" && true`,
	// An arbitrary command in front of the match, with the invocation as its unquoted ARGUMENTS.
	// The quoted "string literal" case above was rejected incidentally, by its quote — this one has
	// no quote to catch, so it isolates the prefix rule itself.
	"printed as arguments": `echo cosign sign --yes --recursive "${REF}"`,
	// A here-document body is INPUT, not commands. The invocation has no prefix and carries the
	// resolved reference, so every line-level rule accepts it while bash feeds it to `:`.
	"heredoc body":     ": <<'UNSIGNED'\n          cosign sign --yes --recursive \"${REF}\"\n          UNSIGNED",
	"heredoc unquoted": ": <<UNSIGNED\n          cosign sign --yes --recursive \"${REF}\"\n          UNSIGNED",
}

// TestRejectsSigningThatIsNotAPlainTopLevelCommand is the general form of the comment, conditional
// and control-flow bypasses: the step keeps its position and its digest assignment, the text contains
// the invocation, and no signature is produced. The guard requires the invocation to BE a command,
// rather than trying to enumerate every way it can fail to be one.
func TestRejectsSigningThatIsNotAPlainTopLevelCommand(t *testing.T) {
	for name, replacement := range signingReplacements {
		t.Run(name, func(t *testing.T) {
			mutated := strings.Replace(validAction,
				`        cosign sign --yes --recursive "${REF}"`,
				"        "+replacement, 1)
			mustChange(t, validAction, mutated)

			if err := validate([]byte(mutated)); err == nil {
				t.Fatalf("expected %q to be rejected; it produces no signature", replacement)
			}
		})
	}
}

// TestAllowsAPlainInvocationWithArguments keeps the rule usable: the real steps invoke a script with
// arguments, so a command path preceding the matched text must stay legal.
func TestAllowsAPlainInvocationWithArguments(t *testing.T) {
	if err := validate([]byte(validAction)); err != nil {
		t.Fatalf("the reference sequence invokes a script with arguments and must pass, got: %v", err)
	}
}

// operationReplacements covers the same prefix bypass on the OTHER two required operations. `echo`
// is a plain command path, so a rule that allows any word before the match accepts these while bash
// only prints them. They need their own table because they are matched as subcommands of a wrapper
// (`./scripts/run-ksail-prod-with-pull-auth.sh workload push`), where — unlike `cosign sign` — a
// preceding word is legitimate and cannot simply be banned.
var operationReplacements = map[string]struct{ from, to string }{
	"publish printed as arguments": {
		"./scripts/run-ksail-prod-with-pull-auth.sh workload push",
		"echo workload push",
	},
	"release printed as arguments": {
		"./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile",
		"echo workload reconcile",
	},
	// These replace the whole `run:` line with a BLOCK scalar. Substituting a bare heredoc into the
	// existing plain scalar would fold its lines into one, so the delimiter would end up inside the
	// prefix and the case would be rejected for the wrong reason — passing without ever exercising
	// heredoc handling.
	"publish inside a heredoc": {
		"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push",
		"      run: |\n        : <<'NOPE'\n        ./scripts/run-ksail-prod-with-pull-auth.sh workload push\n        NOPE",
	},
	"release inside a heredoc": {
		"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile",
		"      run: |\n        : <<'NOPE'\n        ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile\n        NOPE",
	},
}

// TestRejectsOperationsPrintedRatherThanRun extends the plain-command rule to publish and release.
// Requiring the match to be a command is not enough on its own when the match is a SUBCOMMAND: the
// check also has to know which executable carries it, or `echo` passes for the same reason `ksail`
// does.
func TestRejectsOperationsPrintedRatherThanRun(t *testing.T) {
	for name, replacement := range operationReplacements {
		t.Run(name, func(t *testing.T) {
			mutated := strings.Replace(validAction, replacement.from, replacement.to, 1)
			mustChange(t, validAction, mutated)

			if err := validate([]byte(mutated)); err == nil {
				t.Fatalf("expected %q to be rejected; bash only prints it", replacement.to)
			}
		})
	}
}

// TestAllowsAHeredocAlongsideTheRequiredInvocation is the over-tightening control for heredoc
// skipping. Hiding a here-document body must not cost a step the ability to USE one: a step that
// writes an unrelated payload with a heredoc and then signs normally is legitimate, and rejecting it
// would train people to treat this check as noise. The delimiter must also stop the skipping — if it
// did not, everything after the heredoc (including the real invocation) would vanish with it.
func TestAllowsAHeredocAlongsideTheRequiredInvocation(t *testing.T) {
	withHeredoc := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        cat <<'NOTES' > /tmp/notes.txt\n"+
			"        cosign sign is described here but not run\n"+
			"        NOTES\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, withHeredoc)

	if err := validate([]byte(withHeredoc)); err != nil {
		t.Fatalf("a step may use a heredoc and still sign; got: %v", err)
	}
}

// TestAllowsAHereStringInARequiredStep pins the here-string exclusion, which shipped WRONG in the
// first version of heredoc skipping. `<<<` contains two overlapping `<<` pairs, so a pattern that
// merely starts at `<<` matches one character later and reads the here-string's operand as a
// delimiter — swallowing every line after it, including the real `cosign sign`, and failing a
// perfectly valid action. The claim "a here-string is not an opener" therefore needs a test rather
// than a comment: the comment was there, and it was false.
func TestAllowsAHereStringInARequiredStep(t *testing.T) {
	withHereString := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		"        grep -q x <<<\"sentinel\"\n"+
			`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`, 1)
	mustChange(t, validAction, withHereString)

	if err := validate([]byte(withHereString)); err != nil {
		t.Fatalf("a here-string is not a here-document opener; got: %v", err)
	}
}

// The five tests below are the Codex round at 5add867d. Three of them —
// backslash-quoted heredoc, swallowed failure, trailing comment — are the SAME root cause wearing
// three disguises: the matchers read the `run:` block as TEXT, so anything that changes whether a
// line runs, or what a word actually is, was invisible. Patching each shape individually is the
// blocklist this guard has already been bitten by, so they are fixed together by parsing the script
// as shell. Each still gets its own test: a shared cause is not a shared proof.

// TestRejectsSigningInsideABackslashQuotedHeredoc covers the delimiter form bash accepts and a
// hand-rolled matcher missed. `<<\UNSIGNED` quotes the delimiter with a backslash rather than quotes,
// so a pattern enumerating '...', "..." and bare identifiers does not recognise it — and the body it
// opens then reads as ordinary top-level lines. Bash feeds those words to `:` and signs nothing.
func TestRejectsSigningInsideABackslashQuotedHeredoc(t *testing.T) {
	hidden := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        : <<\\UNSIGNED\n"+
			`        cosign sign --yes --recursive "${REF}"`+"\n"+
			"        UNSIGNED", 1)
	mustChange(t, validAction, hidden)

	err := validate([]byte(hidden))
	if err == nil {
		t.Fatal("a signing command inside a backslash-quoted heredoc body is input to `:`, not a signature")
	}

	if !strings.Contains(err.Error(), "sign") {
		t.Errorf("error should name the signing step, got: %v", err)
	}
}

// TestRejectsSigningWhoseFailureIsSwallowed covers `|| true`. The invocation is real and its
// arguments are right, so every text-level check passes — but the step succeeds even when cosign
// fails, the attestations still run, and reconciliation releases an artifact carrying no signature.
// The guard exists to prove a signature was produced, not that a signing command was written down.
func TestRejectsSigningWhoseFailureIsSwallowed(t *testing.T) {
	swallowed := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        cosign sign --yes --recursive "${REF}" || true`, 1)
	mustChange(t, validAction, swallowed)

	err := validate([]byte(swallowed))
	if err == nil {
		t.Fatal("a signing failure that cannot fail the step does not evidence a signature")
	}

	if !strings.Contains(err.Error(), "sign") {
		t.Errorf("error should name the signing step, got: %v", err)
	}
}

// TestRejectsSigningTheMutableTagWithTheDigestInAComment covers the trailing comment. Substring
// matching for `"${REF}"` cannot tell an argument from a comment, so signing the mutable tag while
// merely MENTIONING the resolved reference satisfied the digest check — restoring precisely the
// tag-resolution race the check was added to prevent.
func TestRejectsSigningTheMutableTagWithTheDigestInAComment(t *testing.T) {
	commented := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        cosign sign --yes --recursive "${REF_TAG}" # "${REF}"`, 1)
	mustChange(t, validAction, commented)

	err := validate([]byte(commented))
	if err == nil {
		t.Fatal("a resolved reference named only in a comment is not passed to cosign")
	}

	if !strings.Contains(err.Error(), signRefArg) {
		t.Errorf("error should name the required argument, got: %v", err)
	}
}

// TestRejectsASecondReleaseThatEvidenceCannotGate covers condition checking against only the FIRST
// release. Conditions were compared to releaseIdx[0], so evidence guarded by the same condition as a
// never-firing first reconcile looked correctly coupled — while a second, unconditional reconcile
// released the artifact with every piece of evidence skipped.
//
// A required step must stand or fall with EVERY release, not with whichever one happens to come
// first.
func TestRejectsASecondReleaseThatEvidenceCannotGate(t *testing.T) {
	const falseCondition = "      if: ${{ false }}\n"

	// The evidence and the first release share a condition that never fires, so the existing
	// same-condition allowance accepts them as coupled.
	gated := strings.Replace(validAction, sbomStep,
		`    - name: Attest SBOM
`+falseCondition+`      uses: actions/attest@59d89421af93a897026c735860bf21b6eb4f7b26
`, 1)
	mustChange(t, validAction, gated)

	withGatedRelease := strings.Replace(gated, reconcileStep,
		`    - name: Reconcile
`+falseCondition+`      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile
`, 1)
	mustChange(t, gated, withGatedRelease)

	// ...and then an unconditional second release publishes anyway.
	broken := withGatedRelease + reconcileStep
	mustChange(t, withGatedRelease, broken)

	err := validate([]byte(broken))
	if err == nil {
		t.Fatal("a second unconditional release runs with the gated evidence skipped")
	}
}

// TestRejectsADigestNotResolvedFromThePublishedTag covers DIGEST's provenance, which nothing bound.
// The validator required the REF assignment's exact text but never asked where DIGEST came from, so
// substituting a constant digest for an older manifest passed: cosign and both attestations then
// cover that old artifact in full, while reconciliation releases the newly pushed — and unsigned —
// `latest`. Every ordering check still holds, which is what makes this the worst of the five.
func TestRejectsADigestNotResolvedFromThePublishedTag(t *testing.T) {
	constant := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST="sha256:1111111111111111111111111111111111111111111111111111111111111111"`, 1)
	mustChange(t, validAction, constant)

	err := validate([]byte(constant))
	if err == nil {
		t.Fatal("a constant digest signs whatever it names, not what this run published")
	}

	if !strings.Contains(err.Error(), "DIGEST") {
		t.Errorf("error should name the unbound variable, got: %v", err)
	}
}

// TestAllowsResolvingTheDigestWithAnotherTool is the negative control for the provenance check. The
// requirement is that DIGEST is derived from the published tag, NOT that one specific tool derives
// it — pinning `docker buildx imagetools` would fail a legitimate switch to crane or a cosign
// equivalent, and a guard that rejects correct input gets suppressed.
func TestAllowsResolvingTheDigestWithAnotherTool(t *testing.T) {
	otherTool := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST=$(crane digest "${REF_TAG}")`, 1)
	mustChange(t, validAction, otherTool)

	if err := validate([]byte(otherTool)); err != nil {
		t.Fatalf("any tool may resolve the digest so long as it reads the published tag; got: %v", err)
	}
}

// TestRejectsAConstantDigestBesideAResolvedOne is the "every assignment" rule's own proof. Accepting
// the mere PRESENCE of a correct resolution would be a bypass in one move: a later assignment
// overwrites an earlier one, so a constant sitting beside a resolved digest decides what cosign
// actually signs while the resolution makes the step look right.
func TestRejectsAConstantDigestBesideAResolvedOne(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`+"\n"+
			`        DIGEST="sha256:1111111111111111111111111111111111111111111111111111111111111111"`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("a constant assignment after the resolution decides what is signed")
	}
}

// TestRejectsASecondRefAssignmentAfterTheResolvedOne is the same rule for REF, which cosign consumes
// directly. Reassigning it to the mutable tag after building the digest reference restores the race
// while leaving the required assignment textually present.
func TestRejectsASecondRefAssignmentAfterTheResolvedOne(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        REF="${REF_TAG}"`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("a later REF assignment decides what cosign signs")
	}
}

// TestAllowsRetryingTheDigestResolution pins the freedom the provenance check must NOT take away.
// Wrapping the resolution in a retry is correct code — if every attempt fails, DIGEST stays empty,
// REF is malformed and cosign fails the step — so requiring the assignment to sit at the top level
// would reject a legitimate action. A guard that fails correct input gets suppressed, not fixed.
func TestAllowsRetryingTheDigestResolution(t *testing.T) {
	withRetry := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		"        for attempt in 1 2 3; do\n"+
			`          DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}') && break`+"\n"+
			"        done", 1)
	mustChange(t, validAction, withRetry)

	if err := validate([]byte(withRetry)); err != nil {
		t.Fatalf("a retry around the digest resolution is correct code; got: %v", err)
	}
}

// TestRejectsSigningWhoseFailureIsDetached covers `&`, the sibling of `|| true`. A backgrounded
// cosign cannot fail the step either: the shell moves on, the attestations run, and reconciliation
// releases before the signature is known to exist — if it ever does.
func TestRejectsSigningWhoseFailureIsDetached(t *testing.T) {
	detached := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        cosign sign --yes --recursive "${REF}" &`, 1)
	mustChange(t, validAction, detached)

	if err := validate([]byte(detached)); err == nil {
		t.Fatal("a backgrounded signing command cannot fail the step")
	}
}

// TestRejectsAnUnparseableRunBlock pins the fail-closed direction. If the shell cannot be parsed the
// guard learns nothing about what runs, so it must report a missing operation rather than fall back
// to matching text — the fallback is what every bypass in this file exploited.
func TestRejectsAnUnparseableRunBlock(t *testing.T) {
	broken := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        cosign sign --yes --recursive "${REF}" $(`, 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("an unparseable run block must not satisfy a required marker")
	}
}

// The tests below cover CodeRabbit's round-6 P1 and its siblings. They are a different AXIS from
// everything above: the grammar says the command runs, and it does — but `set +e` stops its failure
// from reaching the step's exit status, so cosign can fail while the step succeeds and the deploy
// releases unsigned. Syntax alone cannot see that; the shell's failure MODE has to be tracked too.

// TestRejectsSigningAfterErrexitIsDisabled is the reported case.
func TestRejectsSigningAfterErrexitIsDisabled(t *testing.T) {
	disabled := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        set +e\n"+
			`        cosign sign --yes --recursive "${REF}"`+"\n"+
			`        echo "digest=${DIGEST}" >> "${GITHUB_OUTPUT}"`, 1)
	mustChange(t, validAction, disabled)

	if err := validate([]byte(disabled)); err == nil {
		t.Fatal("under `set +e` a failing cosign does not fail the step")
	}
}

// TestRejectsSigningAfterErrexitIsDisabledTheLongWay covers `set +o errexit`, the same instruction
// spelled differently. Matching only `+e` would be a blocklist with one entry.
func TestRejectsSigningAfterErrexitIsDisabledTheLongWay(t *testing.T) {
	disabled := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        set +o errexit\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, disabled)

	if err := validate([]byte(disabled)); err == nil {
		t.Fatal("`set +o errexit` disables failure propagation exactly as `set +e` does")
	}
}

// TestAllowsErrexitDisabledThenRestored is the negative control, and it is why this is tracked as
// STATE rather than matched as a shape. Turning errexit off around unrelated work and back on before
// the signing call is correct code; rejecting it would fail a legitimate action.
func TestAllowsErrexitDisabledThenRestored(t *testing.T) {
	restored := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        set +e\n"+
			"        docker logout ghcr.io\n"+
			"        set -e\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, restored)

	if err := validate([]byte(restored)); err != nil {
		t.Fatalf("errexit restored before the signing call is correct code; got: %v", err)
	}
}

// TestAllowsTheCombinedSetFlagsForm pins that `set -euo pipefail` — what the real composite uses —
// reads as enabling errexit. A parser that only recognised a bare `set -e` would treat the real
// action's own hardening as unrecognised.
func TestAllowsTheCombinedSetFlagsForm(t *testing.T) {
	hardened := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		"        set -euo pipefail\n"+
			`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`, 1)
	mustChange(t, validAction, hardened)

	if err := validate([]byte(hardened)); err != nil {
		t.Fatalf("`set -euo pipefail` enables errexit; got: %v", err)
	}
}

// TestRejectsSigningInAShellThatDoesNotPropagateFailure covers the step-level sibling of `set +e`.
// GitHub runs `shell: bash` as `bash --noprofile --norc -eo pipefail`, but a step may name another
// shell — and then nothing promises that a failing cosign fails the step, without a single `set` line
// appearing anywhere in the script.
func TestRejectsSigningInAShellThatDoesNotPropagateFailure(t *testing.T) {
	otherShell := strings.Replace(validAction,
		`    - name: Sign
      id: cosign-sign
      run: |`,
		`    - name: Sign
      id: cosign-sign
      shell: sh
      run: |`, 1)
	mustChange(t, validAction, otherShell)

	if err := validate([]byte(otherShell)); err == nil {
		t.Fatal("a shell GitHub does not run with -e cannot be assumed to gate the step")
	}
}

// TestAllowsAnotherShellThatEnablesErrexitItself is that rule's negative control: naming a different
// shell is not itself the defect, failing to propagate is. A script that turns errexit on explicitly
// gates the step regardless of how the shell was launched.
func TestAllowsAnotherShellThatEnablesErrexitItself(t *testing.T) {
	explicit := strings.Replace(validAction,
		`    - name: Sign
      id: cosign-sign
      run: |
        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`    - name: Sign
      id: cosign-sign
      shell: sh
      run: |
        set -e
        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`, 1)
	mustChange(t, validAction, explicit)

	if err := validate([]byte(explicit)); err != nil {
		t.Fatalf("an explicit `set -e` gates the step whatever the shell; got: %v", err)
	}
}

// TestRejectsSigningShadowedByAShellFunction covers the third axis: NAME RESOLUTION. The grammar says
// the command runs and errexit says its failure would matter, but bash looks up functions before
// PATH — so a script can define `cosign` as a no-op and then issue a textually perfect invocation
// that signs nothing.
func TestRejectsSigningShadowedByAShellFunction(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		"        cosign() {\n"+
			"          true\n"+
			"        }\n"+
			`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("a `cosign` shell function shadows the real binary and produces no signature")
	}
}

// TestRejectsPublishShadowedByAShellFunction pins that the rule is not cosign-specific. The wrapper
// the composite publishes through is an ordinary command name too, so the same trick hides a push.
func TestRejectsPublishShadowedByAShellFunction(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push`,
		"      run: |\n"+
			"        ./scripts/run-ksail-prod-with-pull-auth.sh() {\n"+
			"          true\n"+
			"        }\n"+
			"        ./scripts/run-ksail-prod-with-pull-auth.sh workload push", 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("a function shadowing the publish wrapper hides the push")
	}
}

// TestAllowsAnUnrelatedShellFunction is the over-tightening control. Defining helpers is ordinary,
// correct scripting; only a function that shadows a REQUIRED operation's own name is a problem, and
// banning functions outright would fail legitimate actions.
func TestAllowsAnUnrelatedShellFunction(t *testing.T) {
	withHelper := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		"        log() {\n"+
			"          echo \"$@\" >&2\n"+
			"        }\n"+
			"        log resolving digest\n"+
			`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`, 1)
	mustChange(t, validAction, withHelper)

	if err := validate([]byte(withHelper)); err != nil {
		t.Fatalf("an unrelated helper function is ordinary scripting; got: %v", err)
	}
}

// TestAllowsAQuotedCommandSubstitution is an over-tightening control for the provenance check.
// `DIGEST="$(…)"` wraps the substitution in a DblQuoted node, and quoting it is BETTER practice than
// leaving it bare — so scanning only the word's top-level parts rejected the more careful spelling of
// correct code. A guard that fails a correct refactor gets suppressed rather than fixed.
func TestAllowsAQuotedCommandSubstitution(t *testing.T) {
	quoted := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST="$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')"`, 1)
	mustChange(t, validAction, quoted)

	if err := validate([]byte(quoted)); err != nil {
		t.Fatalf("quoting a command substitution is good practice, not a defect; got: %v", err)
	}
}

// TestRejectsAConstantDigestViaDeclare covers the declaration builtins. `export`, `declare`, `local`,
// `readonly` and `typeset` all assign, but the parser models them as a DeclClause rather than a
// CallExpr — so reading only CallExpr made a constant override invisible, and it silently won over a
// correct resolution earlier in the same script.
func TestRejectsAConstantDigestViaDeclare(t *testing.T) {
	overridden := strings.Replace(validAction,
		`        REF="ghcr.io/devantler-tech/platform/manifests@${DIGEST}"`,
		`        declare -g DIGEST="sha256:1111111111111111111111111111111111111111111111111111111111111111"`+"\n"+
			`        REF="ghcr.io/devantler-tech/platform/manifests@${DIGEST}"`, 1)
	mustChange(t, validAction, overridden)

	if err := validate([]byte(overridden)); err == nil {
		t.Fatal("a `declare -g` constant overrides the resolved digest and decides what is signed")
	}
}

// TestAllowsExportingTheResolvedDigest is that rule's control: a declaration builtin is not itself
// suspicious. Exporting a correctly resolved value is ordinary scripting and must still pass.
func TestAllowsExportingTheResolvedDigest(t *testing.T) {
	exported := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        export DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`, 1)
	mustChange(t, validAction, exported)

	if err := validate([]byte(exported)); err != nil {
		t.Fatalf("exporting a correctly resolved digest is ordinary scripting; got: %v", err)
	}
}

// TestRejectsADigestWhoseResolutionOnlyMENTIONSThePublishedTag closes the provenance check's last
// text-shaped hole. `resolvesDigestFromPublishedTag` compared the assignment's raw SOURCE SPAN, and
// a source span includes the comments inside a command substitution — so a substitution that emits a
// constant digest and merely names `${REF_TAG}` in a comment satisfied it. Bash never expands that
// comment, so cosign and both attestations complete in full over an OLDER manifest while the tag
// this run actually published ships unsigned. This is the same class the parser closed for the
// required OPERATIONS, arriving one level down: a mention is not an expansion.
func TestRejectsADigestWhoseResolutionOnlyMENTIONSThePublishedTag(t *testing.T) {
	mentioned := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST="$(`+"\n"+
			`          printf '%s' 'sha256:1111111111111111111111111111111111111111111111111111111111111111'`+"\n"+
			`          # ${REF_TAG}`+"\n"+
			`        )"`, 1)
	mustChange(t, validAction, mentioned)

	err := validate([]byte(mentioned))
	if err == nil {
		t.Fatal("a commented mention of the published tag resolves nothing; the digest is still a constant")
	}

	if !strings.Contains(err.Error(), "DIGEST") {
		t.Errorf("error should name the unbound variable, got: %v", err)
	}
}

// TestRejectsADigestThatOnlyCONCATENATESThePublishedTag is the same rule read from the other side.
// The published tag has to be consumed by the command that resolves the digest, not merely stand
// next to its output: `${REF_TAG}$(…constant…)` expands the tag for real, yet the value cosign ends
// up signing is still whatever the substitution printed.
func TestRejectsADigestThatOnlyCONCATENATESThePublishedTag(t *testing.T) {
	concatenated := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST="${REF_TAG}$(printf '%s' 'sha256:1111111111111111111111111111111111111111111111111111111111111111')"`, 1)
	mustChange(t, validAction, concatenated)

	if err := validate([]byte(concatenated)); err == nil {
		t.Fatal("expanding the tag beside a constant does not make the constant derived from it")
	}
}

// TestRejectsAPipelineResolver pins the last construct this guard accepted on an unenforceable
// claim. A pipeline of plain calls has no branches, so it LOOKS provenance-preserving, and an
// earlier round accepted it on the reasoning that later stages "only reshape" the resolver output.
// Shell syntax does not enforce that: `crane digest "${REF_TAG}" | printf '%s' 'sha256:…'`
// reads the tag in its first stage, and printf never reads stdin, so the constant becomes DIGEST.
//
// A single call is therefore the whole accepted set — the only construct whose output is bound to a
// command that demonstrably consumed the tag. The cost is real and deliberate: a pipeline resolver
// must be rewritten as one command, and the error message says so.
func TestRejectsAPipelineResolver(t *testing.T) {
	piped := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST=$(crane digest "${REF_TAG}" | printf '%s' 'sha256:1111111111111111111111111111111111111111111111111111111111111111')`, 1)
	mustChange(t, validAction, piped)

	if err := validate([]byte(piped)); err == nil {
		t.Fatal("a later pipeline stage can emit a digest the resolver never produced")
	}
}

// TestAllowsTheUnbracedExpansion is the second over-tightening control. `$REF_TAG` and `${REF_TAG}`
// are the SAME expansion; only the spelling differs. Requiring the braced source text made the guard
// enforce a style rule it has no stake in, and a guard that rejects an equivalent spelling of correct
// code gets suppressed rather than fixed.
func TestAllowsTheUnbracedExpansion(t *testing.T) {
	unbraced := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST=$(docker buildx imagetools inspect "$REF_TAG" --format '{{.Manifest.Digest}}')`, 1)
	mustChange(t, validAction, unbraced)

	if err := validate([]byte(unbraced)); err != nil {
		t.Fatalf("$REF_TAG and ${REF_TAG} are the same expansion; got: %v", err)
	}
}

// TestRejectsThePublishedTagExpandedOnlyInAHereDocument pins the "as an ARGUMENT" half of the
// provenance rule, which nothing else covers: a here-document body IS parsed for expansions, so
// `${REF_TAG}` inside one is a real expansion node rather than a mention. Accepting any expansion
// found anywhere inside the substitution would therefore let a body fed to a discarding command
// stand in for the resolution. The tag has to reach a command that runs, as an argument.
func TestRejectsThePublishedTagExpandedOnlyInAHereDocument(t *testing.T) {
	inHereDoc := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST="$(cat >/dev/null <<EOF`+"\n"+
			`        ${REF_TAG}`+"\n"+
			`        EOF`+"\n"+
			`        printf '%s' 'sha256:1111111111111111111111111111111111111111111111111111111111111111')"`, 1)
	mustChange(t, validAction, inHereDoc)

	if err := validate([]byte(inHereDoc)); err == nil {
		t.Fatal("a here-document body is not an argument to the command that resolves the digest")
	}
}

// TestRejectsAProcessSubstitution pins the "command SUBSTITUTION" half of the provenance rule. A
// process substitution also runs a command that reads the published tag, but it expands to a
// `/dev/fd/…` path rather than that command's output — so the digest would be a file descriptor
// name, and nothing would have resolved. Only the form whose VALUE is the command's output proves
// the digest came from this run's publication.
func TestRejectsAProcessSubstitution(t *testing.T) {
	procSubst := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST=<(crane digest "${REF_TAG}")`, 1)
	mustChange(t, validAction, procSubst)

	if err := validate([]byte(procSubst)); err == nil {
		t.Fatal("a process substitution yields a file descriptor path, not the resolved digest")
	}
}

// TestRejectsAResolverThatDISCARDSThePublishedTag closes what an earlier round documented as a
// boundary and left open. Requiring only that SOME command inside the substitution receives
// `${REF_TAG}` lets a no-op consume it while a later command prints the value that actually becomes
// the digest — a real provenance bypass with a green CI, which is the worst combination a security
// guard can have. "It would need dataflow analysis" argued for the wrong fix: the lesson from this
// guard's first five rounds is that enumerating BAD shapes never converges, and the answer is to
// narrow the ACCEPTED one instead.
func TestRejectsAResolverThatDISCARDSThePublishedTag(t *testing.T) {
	discarded := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST="$(`+"\n"+
			`          : "${REF_TAG}"`+"\n"+
			`          printf '%s' 'sha256:1111111111111111111111111111111111111111111111111111111111111111'`+"\n"+
			`        )"`, 1)
	mustChange(t, validAction, discarded)

	if err := validate([]byte(discarded)); err == nil {
		t.Fatal("a no-op that consumes the tag does not make the constant it precedes a resolved digest")
	}
}

// TestAllowsSetupBeforeTheResolver is that rule's over-tightening control. Only the command whose
// output BECOMES the digest has to read the published tag; ordinary setup in front of it — shell
// options, a variable, a guard — is correct scripting and must still pass.
func TestAllowsSetupBeforeTheResolver(t *testing.T) {
	withSetup := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST="$(`+"\n"+
			`          set -euo pipefail`+"\n"+
			`          command -v crane >/dev/null`+"\n"+
			`          crane digest "${REF_TAG}"`+"\n"+
			`        )"`, 1)
	mustChange(t, validAction, withSetup)

	if err := validate([]byte(withSetup)); err != nil {
		t.Fatalf("setup before the resolver is ordinary scripting; got: %v", err)
	}
}

// TestRejectsAResolverInAnUNTAKENBranch is the reason the accepted shape is enumerated rather than
// searched. A branch that never runs is still part of the last statement, so recursing through every
// nested call found `crane digest` inside a dead `if` while bash returned the constant from the
// `else`. Any construct whose output depends on which branch or iteration executes is rejected: it
// cannot be bound without evaluating it, and guessing is what this guard keeps being bypassed by.
func TestRejectsAResolverInAnUNTAKENBranch(t *testing.T) {
	deadBranch := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        DIGEST="$(`+"\n"+
			`          if false; then`+"\n"+
			`            crane digest "${REF_TAG}"`+"\n"+
			`          else`+"\n"+
			`            printf '%s' 'sha256:1111111111111111111111111111111111111111111111111111111111111111'`+"\n"+
			`          fi`+"\n"+
			`        )"`, 1)
	mustChange(t, validAction, deadBranch)

	if err := validate([]byte(deadBranch)); err == nil {
		t.Fatal("a resolver in an untaken branch never runs; the constant beside it is what gets signed")
	}
}

// The tests below cover a sixth axis and three refinements of earlier ones, all found by review at
// `fa710581`. Each names the property that breaks, because each was a GREEN result over a deploy
// that published nothing — the shape this guard exists to make impossible.

const signCall = `        cosign sign --yes --recursive "${REF}"`

// TestRejectsQuotedSetPlusE covers WORD INTERPRETATION: bash removes quotes before reading a word,
// so `set '+e'` disables errexit exactly as `set +e` does. Comparing the source form left errexit
// tracked as enabled, and a cosign failure could then be followed by a succeeding command with the
// step still green.
func TestRejectsQuotedSetPlusE(t *testing.T) {
	broken := strings.Replace(validAction, signCall, "        set '+e'\n"+signCall, 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a quoted `set '+e'` to disable failure propagation and be rejected")
	}
}

// TestAllowsQuotedSetMinusE is the over-tightening control for the test above: quote removal has to
// work in BOTH directions, or an ordinary `set '-e'` would start failing.
func TestAllowsQuotedSetMinusE(t *testing.T) {
	fixture := strings.Replace(validAction, signCall, "        set '-e'\n"+signCall, 1)
	mustChange(t, validAction, fixture)

	if err := validate([]byte(fixture)); err != nil {
		t.Fatalf("a quoted `set '-e'` enables errexit and is correct code, got: %v", err)
	}
}

// TestRejectsAnAliasShadowingARequiredCommand covers NAME RESOLUTION. Bash resolves aliases before
// functions and PATH, so with `shopt -s expand_aliases` a textually perfect, failure-propagating,
// top-level cosign invocation expands to `true` and signs nothing.
func TestRejectsAnAliasShadowingARequiredCommand(t *testing.T) {
	broken := strings.Replace(validAction, signCall,
		"        shopt -s expand_aliases\n        alias cosign=true\n"+signCall, 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected an alias shadowing cosign to be rejected")
	}
}

// TestAllowsAnUnrelatedAlias is the over-tightening control: defining an alias is ordinary
// scripting, and only one that rebinds a REQUIRED name may fail the check.
func TestAllowsAnUnrelatedAlias(t *testing.T) {
	fixture := strings.Replace(validAction, signCall,
		"        alias ll='ls -l'\n"+signCall, 1)
	mustChange(t, validAction, fixture)

	if err := validate([]byte(fixture)); err != nil {
		t.Fatalf("an unrelated alias is ordinary scripting, got: %v", err)
	}
}

const resolverCall = `        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`

// TestRejectsAResolverThatIgnoresTheTag closes the last hole in the digest-provenance axis. A single
// direct call taking the tag as an argument still proves nothing about the OUTPUT: printf ignores
// extra arguments when its format carries no conversion, so this returns a constant digest for an
// older manifest, which cosign and both attestations then cover in full.
func TestRejectsAResolverThatIgnoresTheTag(t *testing.T) {
	broken := strings.Replace(validAction, resolverCall,
		`        DIGEST=$(printf 'sha256:0000000000000000000000000000000000000000000000000000000000000000' "${REF_TAG}")`, 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a resolver whose output ignores the published tag to be rejected")
	}
}

// TestAllowsAnotherResolverTool is the over-tightening control. The accepted set is about a
// program's contract, not about which tool this repo currently uses; switching to crane is a
// legitimate refactor and must not fail CI.
func TestAllowsAnotherResolverTool(t *testing.T) {
	fixture := strings.Replace(validAction, resolverCall,
		`        DIGEST=$(crane digest "${REF_TAG}")`, 1)
	mustChange(t, validAction, fixture)

	if err := validate([]byte(fixture)); err != nil {
		t.Fatalf("crane is a legitimate digest resolver, got: %v", err)
	}
}

// TestRejectsSigningWithUploadDisabled covers the SIGNING OPTION surface. `--upload=false` produces
// the signature locally and never writes it to the registry: the command succeeds, carries the exact
// required reference, and leaves the artifact unsigned.
func TestRejectsSigningWithUploadDisabled(t *testing.T) {
	broken := strings.Replace(validAction, signCall,
		`        cosign sign --yes --recursive --upload=false "${REF}"`, 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected `cosign sign --upload=false` to be rejected: it publishes no signature")
	}
}

// TestRejectsAnAttestationOfAnotherDigest covers the ATTESTATION SUBJECT. The action reference says
// only which program runs; subject-digest is free, so a step can keep its position, its pinned
// `uses:` and its green result while attesting an older artifact.
func TestRejectsAnAttestationOfAnotherDigest(t *testing.T) {
	broken := strings.Replace(validAction,
		"        subject-digest: ${{ steps.cosign-sign.outputs.digest }}",
		"        subject-digest: sha256:0000000000000000000000000000000000000000000000000000000000000000", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected an attestation of a digest this run did not resolve to be rejected")
	}
}

// TestRejectsAnAttestationThatIsNeverPushed is the same axis from the other side: evidence that is
// generated but not published leaves the registry with nothing attached, while the step succeeds.
func TestRejectsAnAttestationThatIsNeverPushed(t *testing.T) {
	broken := strings.Replace(validAction,
		`        push-to-registry: "true"`, `        push-to-registry: "false"`, 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected an attestation that is never pushed to the registry to be rejected")
	}
}

// TestRejectsAReleaseThatRunsAfterFailure covers FAILURE COUPLING, which every ordering guarantee
// here silently assumes. A step's `if:` is implicitly `success() && …` unless it calls a status
// check function; `always()` removes that, so the release runs precisely when the evidence failed.
func TestRejectsAReleaseThatRunsAfterFailure(t *testing.T) {
	broken := strings.Replace(validAction, reconcileStep,
		"    - name: Reconcile\n      if: always()\n"+
			"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile\n", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a release conditioned with always() to be rejected")
	}
}

// TestRejectsAReleaseGatedOnAnExplicitSuccessCall is the same axis reached through the status check
// the list omitted. `success()` is ITSELF a status check function, so writing it explicitly also
// replaces the implicit gate — and once it is an operand of `||` the expression evaluates true on a
// failed evidence step, releasing exactly what `always()` would while reading as if it required
// success. Matching only always/failure/cancelled left that spelling open.
func TestRejectsAReleaseGatedOnAnExplicitSuccessCall(t *testing.T) {
	broken := strings.Replace(validAction, reconcileStep,
		"    - name: Reconcile\n      if: success() || true\n"+
			"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile\n", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a release conditioned with an explicit success() call to be rejected")
	}
}

// TestAllowsANonStatusCheckReleaseCondition is the over-tightening control. An ordinary condition
// keeps the implicit success() and therefore keeps the coupling, so gating the release on a branch
// or an input stays legal.
func TestAllowsANonStatusCheckReleaseCondition(t *testing.T) {
	fixture := strings.Replace(validAction, reconcileStep,
		"    - name: Reconcile\n      if: github.ref == 'refs/heads/main'\n"+
			"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile\n", 1)
	mustChange(t, validAction, fixture)

	if err := validate([]byte(fixture)); err != nil {
		t.Fatalf("an ordinary release condition keeps the implicit success(), got: %v", err)
	}
}

// TestRejectsAReleaseGatedOnACaseVariantStatusCheck covers the SPELLING of a status check call
// rather than which function is called. The rule keys on the call, but the match was written
// lowercase-only, so `ALWAYS()` reached the release step through a matcher looking for `always(`.
//
// This is rejected under either reading of GitHub's expression parser, which is why the guard does
// not depend on settling that question: if function names resolve case-insensitively then `ALWAYS()`
// is a working bypass and must be rejected; if they do not, it is not a legitimate condition anybody
// writes, so rejecting it costs nothing. Fail closed on both.
func TestRejectsAReleaseGatedOnACaseVariantStatusCheck(t *testing.T) {
	broken := strings.Replace(validAction, reconcileStep,
		"    - name: Reconcile\n      if: ALWAYS()\n"+
			"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile\n", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a release conditioned with a case-variant ALWAYS() call to be rejected")
	}
}

// TestRejectsAReleaseGatedOnALineBrokenStatusCheck covers the WHITESPACE axis, which the case axis
// above does not reach. Normalization stripped only U+0020, so any other whitespace between the
// function name and its `(` slipped past a matcher looking for `always(`.
//
// A YAML LITERAL block scalar is the spelling that matters, and it is worth being precise about why:
// a FOLDED scalar (`>-`) converts its newlines to spaces, so `always\n()` written that way arrives as
// `always ()` and the space-only normalization already caught it. A literal block (`|`) preserves the
// newline verbatim, so the condition arrives as `always\n()` — which GitHub evaluates as a call, and
// which the guard did not see. Measured, not assumed: folded normalized to `always()`, literal stayed
// `always\n()`.
func TestRejectsAReleaseGatedOnALineBrokenStatusCheck(t *testing.T) {
	broken := strings.Replace(validAction, reconcileStep,
		"    - name: Reconcile\n      if: |\n        always\n        ()\n"+
			"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile\n", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a release conditioned with a line-broken always() call to be rejected")
	}
}

// TestRejectsAReleaseGatedOnATabSeparatedStatusCheck pins the second whitespace spelling. A
// double-quoted scalar resolves `\t` to a real tab, so this reaches the matcher as `always\t()`.
// It is a separate fixture from the line-broken one because they fail for different reasons under a
// space-only strip, and a single-character fix could plausibly address one and miss the other.
func TestRejectsAReleaseGatedOnATabSeparatedStatusCheck(t *testing.T) {
	broken := strings.Replace(validAction, reconcileStep,
		"    - name: Reconcile\n      if: \"always\\t()\"\n"+
			"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile\n", 1)
	mustChange(t, validAction, broken)

	if err := validate([]byte(broken)); err == nil {
		t.Fatal("expected a release conditioned with a tab-separated always() call to be rejected")
	}
}

// TestStatusChecksAreLowercase pins the invariant the case-insensitive match depends on. The
// condition is lowercased before matching, so an entry added to the list with any uppercase letter
// could never match anything — the list would silently stop covering that function while still
// reading as though it did. Asserting the entries here is cheaper than discovering it from a bypass.
func TestStatusChecksAreLowercase(t *testing.T) {
	for _, check := range statusChecks {
		if check != strings.ToLower(check) {
			t.Fatalf("statusChecks entry %q must be lowercase: the condition is lowercased before"+
				" matching, so an entry with uppercase can never match", check)
		}
	}
}

// TestRejectsErrexitApparentlyRestoredBySetPositionals covers `set --`, which assigns the remaining
// words to the POSITIONAL PARAMETERS rather than to shell options (bash `help set`). A scan that
// kept reading flags past it accepted `set -- -e` as errexit being restored, so a signing block
// could turn failure propagation off, set positionals, and still be credited with a gate that no
// longer fails the step. The guard then reported the safe state exactly where the unsafe one held —
// the worst direction for it to be wrong in.
func TestRejectsErrexitApparentlyRestoredBySetPositionals(t *testing.T) {
	bypass := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        set +e\n"+
			"        docker logout ghcr.io\n"+
			"        set -- -e\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, bypass)

	if err := validate([]byte(bypass)); err == nil {
		t.Fatal("`set -- -e` assigns positional parameters; errexit is still off at the signing call")
	}
}

// TestAllowsSetOptionsBeforeTheDoubleDash is the over-tightening control for the test above.
// Terminating the scan at `--` must not blind it to the options that come BEFORE one: `set -e --`
// really does restore errexit, and rejecting it would fail correct code.
func TestAllowsSetOptionsBeforeTheDoubleDash(t *testing.T) {
	restored := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        set +e\n"+
			"        docker logout ghcr.io\n"+
			"        set -e -- keep-positionals\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, restored)

	if err := validate([]byte(restored)); err != nil {
		t.Fatalf("`set -e --` restores errexit before signing; got: %v", err)
	}
}

// TestRejectsAPrintfOverwriteOfTheSignedReference covers a builtin that writes a variable named by
// its ARGUMENTS. The parser models `printf -v REF …` as command words, not as CallExpr.Assigns, so
// the overwrite was invisible: a correct resolution stood in the source while cosign signed the
// mutable tag written after it. Same bypass as a second `REF=` assignment, spelled so the assignment
// scan could not see it.
func TestRejectsAPrintfOverwriteOfTheSignedReference(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        printf -v REF '%s' 'ghcr.io/devantler-tech/platform/manifests:latest'`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("`printf -v REF` overwrites the resolved reference before cosign reads it")
	}
}

// TestRejectsAnAttachedPrintfOverwrite pins the spelling an exact "-v then the next word" scan would
// miss. Bash accepts the option argument attached, so `-vREF` is the same write as `-v REF`, and a
// guard that caught only the detached form would document its own bypass.
func TestRejectsAnAttachedPrintfOverwrite(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        printf -vREF '%s' 'ghcr.io/devantler-tech/platform/manifests:latest'`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("`printf -vREF` is the attached spelling of the same overwrite")
	}
}

// TestAllowsPrintfReadingTheReference is the over-tightening control for the builtin-write
// detection, and it is the one that keeps the guard usable. Writing a value OUT — to a step output,
// a log line, a file — is the single most common thing a workflow does with a resolved reference,
// and it only READS the variable. A detector that flagged any command mentioning REF would reject
// correct actions, and a guard people cannot ship past is suppressed rather than satisfied.
func TestAllowsPrintfReadingTheReference(t *testing.T) {
	reported := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        printf '%s\n' "${REF}" >>"${GITHUB_OUTPUT}"`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, reported)

	if err := validate([]byte(reported)); err != nil {
		t.Fatalf("printf READING the reference is correct code; got: %v", err)
	}
}

// TestRejectsAReadOverwriteOfTheSignedReference proves the fix addresses the CLASS rather than the
// one builtin that was reported. `read` takes its destination as an operand exactly as `printf -v`
// takes it as an option argument, so a detector special-cased to printf would leave this open.
func TestRejectsAReadOverwriteOfTheSignedReference(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        read -r REF <<<'ghcr.io/devantler-tech/platform/manifests:latest'`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("`read REF` is the same overwrite through a different builtin")
	}
}

// TestAllowsReadIntoAnUnrelatedVariable is the control for the operand decoding: `read` writing
// something else entirely must not be mistaken for a write to the protected name, or every action
// that parses anything would be rejected.
func TestAllowsReadIntoAnUnrelatedVariable(t *testing.T) {
	unrelated := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        read -r -p 'REF: ' answer <<<'ignored'`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, unrelated)

	if err := validate([]byte(unrelated)); err != nil {
		t.Fatalf("read into an unrelated variable is correct code; got: %v", err)
	}
}

// TestRejectsANamerefOverwriteOfTheSignedReference covers the same overwrite one level of indirection
// out. `declare -n target=REF` makes every later `target=…` write REF, but a collector matching only
// assignments literally named REF sees just the correct one — so cosign signs whatever went through
// the alias while the validator reports the resolution intact.
func TestRejectsANamerefOverwriteOfTheSignedReference(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        declare -n target=REF\n"+
			`        target='ghcr.io/devantler-tech/platform/manifests:latest'`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("`declare -n target=REF` is a handle on REF; writes through it decide what cosign signs")
	}
}

// TestAllowsANamerefToAnUnrelatedVariable is the over-tightening control: namerefs are a normal bash
// feature, and refusing every one of them would reject correct code. Only an alias whose TARGET is
// the protected variable is a write to it.
func TestAllowsANamerefToAnUnrelatedVariable(t *testing.T) {
	unrelated := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        declare -n target=GITHUB_OUTPUT\n"+
			`        printf '%s\n' "${REF}" >>"${target}"`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, unrelated)

	if err := validate([]byte(unrelated)); err != nil {
		t.Fatalf("a nameref to an unrelated variable is correct code; got: %v", err)
	}
}

// TestRejectsPrefixedBuiltinBypasses covers `builtin` and `command`, which bash accepts in front of a
// builtin. Every check that keyed on a command's first word — the errexit toggle, builtin write
// detection, nameref detection — was defeated by a single prefix, so one wrapper reopened three
// separate bypasses at once. All four shapes below were verified to pass the validator before the
// prefix stripping went in.
func TestRejectsPrefixedBuiltinBypasses(t *testing.T) {
	for _, testCase := range []struct{ name, inject, wantErr string }{
		{
			"builtin printf -v",
			`        builtin printf -v REF '%s' 'ghcr.io/devantler-tech/platform/manifests:latest'`,
			"resolved digest",
		},
		{
			"command printf -v",
			`        command printf -v REF '%s' 'ghcr.io/devantler-tech/platform/manifests:latest'`,
			"resolved digest",
		},
		{
			"builtin declare -n",
			"        builtin declare -n target=REF\n" +
				`        target='ghcr.io/devantler-tech/platform/manifests:latest'`,
			"resolved digest",
		},
		{"builtin set +e", "        builtin set +e", "no step appears to"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			bypass := strings.Replace(validAction,
				`        cosign sign --yes --recursive "${REF}"`,
				testCase.inject+"\n"+`        cosign sign --yes --recursive "${REF}"`, 1)
			mustChange(t, validAction, bypass)

			err := validate([]byte(bypass))
			if err == nil {
				t.Fatal("a builtin/command prefix must not hide the write or the errexit toggle")
			}

			// Assert WHICH contract rejected it. A bare non-nil check passes for any reason at all,
			// so a case could stop exercising the prefix stripping it names and still look green.
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("want an error naming %q; got: %v", testCase.wantErr, err)
			}
		})
	}
}

// TestRejectsControlAndBindingBypasses covers four ways a signing block could look correct to the
// validator while `cosign sign` never covers the released artifact. All four were reported as P1 on
// the PR that added the surrounding guard, and all four passed the validator before these fixes.
//
// They are grouped because they share one failure signature: the step exits zero, the digest output
// is exported correctly, and both attestations plus reconciliation advance over an artifact that is
// unsigned or signed at a stale reference.
func TestRejectsControlAndBindingBypasses(t *testing.T) {
	const stale = "ghcr.io/devantler-tech/platform/manifests:latest"

	for _, testCase := range []struct{ name, inject, wantErr string }{
		{
			// An unconditional top-level `exit` ends the shell, so the `cosign sign` written after it
			// never runs — but the scan kept walking and credited it as executed.
			"exit before cosign",
			"        exit 0",
			"no step appears to",
		},
		{
			// `exec <cmd>` replaces the shell image; everything after it is equally unreachable.
			"exec before cosign",
			"        exec true",
			"no step appears to",
		},
		{
			// `builtin -- set +e` runs `set`, but the wrapper stripper advanced only past `builtin`
			// and left `--` standing as the command, so errexit stayed tracked as enabled.
			"builtin -- set +e",
			"        builtin -- set +e",
			"no step appears to",
		},
		{
			// A subscripted printf destination writes REF; the decoder compared the target exactly.
			"printf -v subscripted destination",
			`        printf -v 'REF[0]' '%s' '` + stale + `'`,
			"resolved digest",
		},
		{
			// A `for` binding overwrites REF and leaves its final value in scope after the loop.
			"for-loop binding overwrites REF",
			"        for REF in '" + stale + "'; do :; done",
			"resolved digest",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			bypass := strings.Replace(validAction,
				`        cosign sign --yes --recursive "${REF}"`,
				testCase.inject+"\n"+`        cosign sign --yes --recursive "${REF}"`, 1)
			mustChange(t, validAction, bypass)

			err := validate([]byte(bypass))
			if err == nil {
				t.Fatal("the bypass must not leave the publication-order contract satisfied")
			}

			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("want an error naming %q; got: %v", testCase.wantErr, err)
			}
		})
	}
}

// TestAllowsAnExecThatOnlyRedirects is the over-tightening control for the exit/exec termination.
// A bare `exec` with no command applies redirections to the CURRENT shell and execution continues
// past it, so treating it as terminating would blind the guard to the whole rest of the step.
func TestAllowsAnExecThatOnlyRedirects(t *testing.T) {
	withRedirect := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        exec 3>&1\n"+`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, withRedirect)

	if err := validate([]byte(withRedirect)); err != nil {
		t.Fatalf("a redirect-only exec must not hide the commands after it; got: %v", err)
	}
}

// TestRejectsNamerefWithASubscriptTarget covers a nameref target that carries an array subscript.
//
// Bash accepts a subscript on a nameref target, and on a scalar `REF[0]` denotes REF itself, so
// `declare -n target=REF[0]` followed by a write through target assigns REF. Both write detectors
// compared the target to the protected name EXACTLY, so the subscript slipped past each of them.
//
// Both spellings are covered because they are separate code paths: the unprefixed form is modelled
// as a DeclClause, while a `builtin`/`command` prefix stops the parser doing that and falls through
// to the word scanner. Only the prefixed shape was reported; fixing that one alone would have left
// the commoner unprefixed spelling open.
func TestRejectsNamerefWithASubscriptTarget(t *testing.T) {
	for _, testCase := range []struct{ name, inject string }{
		{
			"declare -n subscript",
			"        declare -n target=REF[0]\n" +
				`        target='ghcr.io/devantler-tech/platform/manifests:latest'`,
		},
		{
			"builtin declare -n subscript",
			"        builtin declare -n target=REF[0]\n" +
				`        target='ghcr.io/devantler-tech/platform/manifests:latest'`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			bypass := strings.Replace(validAction,
				`        cosign sign --yes --recursive "${REF}"`,
				testCase.inject+"\n"+`        cosign sign --yes --recursive "${REF}"`, 1)
			mustChange(t, validAction, bypass)

			err := validate([]byte(bypass))
			if err == nil {
				t.Fatal("a subscripted nameref target must not hide the write to REF")
			}

			if !strings.Contains(err.Error(), "resolved digest") {
				t.Fatalf("want the resolved-digest contract to reject it; got: %v", err)
			}
		})
	}
}

// TestAllowsCommandLookupOfARequiredTool is the over-tightening control for the prefix stripping.
// `command -v` and `command -V` only LOOK UP a command — they execute nothing — so a preflight check
// that a tool exists must stay correct code. Stripping the wrapper without honouring `-v` would read
// this as running printf and writing REF.
func TestAllowsCommandLookupOfARequiredTool(t *testing.T) {
	preflight := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        command -v cosign >/dev/null\n"+
			`        command -v printf >/dev/null`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, preflight)

	if err := validate([]byte(preflight)); err != nil {
		t.Fatalf("`command -v` is a lookup, not an execution; got: %v", err)
	}
}

// TestReportsAMissingSigningStepID pins the error a reader actually needs. Without an `id:` the
// evidence marker becomes `${{ steps..outputs.digest }}`, which no attestation can match — so the run
// failed while pointing at the attestation steps and telling the reader to write that empty
// expression into them. The defect is one step earlier, and a guard that misnames it sends people to
// the wrong file.
func TestReportsAMissingSigningStepID(t *testing.T) {
	noID := strings.Replace(validAction, "      id: cosign-sign\n", "", 1)
	mustChange(t, validAction, noID)

	err := validate([]byte(noID))
	if err == nil {
		t.Fatal("a signing step with no id: cannot bind any attestation")
	}

	if !strings.Contains(err.Error(), "id:") {
		t.Fatalf("the error must name the missing id:; got: %v", err)
	}

	if strings.Contains(err.Error(), "steps..outputs.digest") {
		t.Fatalf("the error must not quote the empty marker back at the reader; got: %v", err)
	}
}

// unmodeledStateChanges are the ways a signing block can change how the shell RESOLVES a command or
// PROPAGATES its failure, using a construct the guard does not interpret. Each one leaves the
// required invocation textually perfect, top-level, unnegated and errexit-guarded — every property
// the guard checks — while producing no signature.
//
// They are enumerated as one table rather than one per review round because they are one class: the
// guard models a fixed set of state changes (`set -e`, aliases, functions, namerefs) and SKIPPED
// everything else, so each unmodeled construct was a silent bypass. That is the same blocklist
// failure the grammar rule already fixed for "does it run"; this is the state axis of it.
var unmodeledStateChanges = map[string]string{
	// `hash -p PATH NAME` makes bash use PATH for NAME, ahead of any lookup (bash `help hash`).
	"hash rebinding": "        hash -p /usr/bin/true cosign\n" + signCall,
	// An ERR handler runs where errexit would exit, and `exit 0` then makes the step succeed.
	"ERR trap": "        trap 'exit 0' ERR\n" + signCall,
	// `set -n` reads commands without executing them (bash `help set`).
	"noexec":      "        set -n\n" + signCall,
	"noexec long": "        set -o noexec\n" + signCall,
	// `source`/`.` and `eval` run text the guard never sees.
	"sourced file": "        source ./setup.sh\n" + signCall,
	"dot file":     "        . ./setup.sh\n" + signCall,
	"eval":         "        eval 'cosign() { true; }'\n" + signCall,
	// `enable -n` turns a builtin off; `shopt` changes expansion, including alias expansion.
	"disabled builtin": "        enable -n printf\n" + signCall,
	// State changed inside a top-level BLOCK still applies to the current shell, so nesting it does
	// not make it someone else's problem.
	"block-nested trap": "        { trap 'exit 0' ERR; }\n" + signCall,
}

func TestRejectsUnmodeledShellStateChanges(t *testing.T) {
	for name, replacement := range unmodeledStateChanges {
		t.Run(name, func(t *testing.T) {
			mutated := strings.Replace(validAction, signCall, replacement, 1)
			mustChange(t, validAction, mutated)

			if err := validate([]byte(mutated)); err == nil {
				t.Fatalf("expected %q to be rejected: it changes command resolution or failure "+
					"propagation in a way the guard does not model", name)
			}
		})
	}
}

// TestRejectsBashEnvOnASigningStep covers the same axis from the STEP rather than the script: bash
// expands BASH_ENV and reads that file before running a non-interactive script, so a startup file
// defining `cosign() { true; }` shadows the required command without appearing in the run block.
func TestRejectsBashEnvOnASigningStep(t *testing.T) {
	withEnv := strings.Replace(validAction,
		"      id: cosign-sign\n",
		"      id: cosign-sign\n      env:\n        BASH_ENV: ./setup.sh\n", 1)
	mustChange(t, validAction, withEnv)

	if err := validate([]byte(withEnv)); err == nil {
		t.Fatal("BASH_ENV sources a startup file the guard never reads")
	}
}

// TestRejectsPathOnASigningStep closes the step-level half of command resolution. The script-level
// forms are already covered by resolutionAssignments below, but PATH set through the STEP's `env:`
// never appears in the run block at all: bash then resolves the textually perfect
// `cosign sign …` to whatever `cosign` sits in that directory. Same shadowing as BASH_ENV, reached
// without touching the script — and this file already treats PATH as resolution state when it is
// assigned inside one.
func TestRejectsPathOnASigningStep(t *testing.T) {
	withEnv := strings.Replace(validAction,
		"      id: cosign-sign\n",
		"      id: cosign-sign\n      env:\n        PATH: /tmp/fake\n", 1)
	mustChange(t, validAction, withEnv)

	if err := validate([]byte(withEnv)); err == nil {
		t.Fatal("a step-level PATH decides which binary the required command resolves to")
	}
}

// TestAllowsOrdinarySigningStepEnv is the over-tightening control: the real signing step carries
// `env: COSIGN_YES`, so rejecting env wholesale would reject the action this guard exists to protect.
func TestAllowsOrdinarySigningStepEnv(t *testing.T) {
	withEnv := strings.Replace(validAction,
		"      id: cosign-sign\n",
		"      id: cosign-sign\n      env:\n        COSIGN_YES: \"true\"\n", 1)
	mustChange(t, validAction, withEnv)

	if err := validate([]byte(withEnv)); err != nil {
		t.Fatalf("an ordinary step env is how the real action configures cosign, got: %v", err)
	}
}

// TestAllowsTheModeledOptionSet is the over-tightening control for the `set` allowlist: the real
// signing block opens with `set -euo pipefail`, and a guard that rejected it would reject production.
func TestAllowsTheModeledOptionSet(t *testing.T) {
	withOpts := strings.Replace(validAction, signCall,
		"        set -euo pipefail\n"+signCall, 1)
	mustChange(t, validAction, withOpts)

	if err := validate([]byte(withOpts)); err != nil {
		t.Fatalf("`set -euo pipefail` opens the real signing block, got: %v", err)
	}
}

// TestRejectsALoopBindingTheResolvedReference covers the ASSIGNMENT axis: a `for` loop binds its
// variable and leaves the final value in scope, so `for REF in <stale>` overwrites the resolved
// reference. The collector reads CallExpr and DeclClause writes, so the loop's binding is invisible
// and the original valid assignment still satisfies the check.
func TestRejectsALoopBindingTheResolvedReference(t *testing.T) {
	mutated := strings.Replace(validAction, signCall,
		"        for REF in ghcr.io/devantler-tech/platform/manifests:latest; do :; done\n"+signCall, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err == nil {
		t.Fatal("`for REF in …` overwrites the resolved reference before cosign reads it")
	}
}

// TestAllowsALoopBindingAnUnrelatedName is that rule's over-tightening control — the reference
// sequence already loops over `attempt`, and iteration is ordinary scripting.
func TestAllowsALoopBindingAnUnrelatedName(t *testing.T) {
	mutated := strings.Replace(validAction, signCall,
		"        for attempt in 1 2 3; do :; done\n"+signCall, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err != nil {
		t.Fatalf("looping over an unrelated name is ordinary scripting, got: %v", err)
	}
}

// TestRejectsASubscriptedBuiltinWrite covers the destination-normalisation gap: bash converts a
// scalar to an array on `printf -v 'REF[0]'`, and `${REF}` then returns element zero — so the write
// lands on REF while an exact name comparison sees a different destination.
func TestRejectsASubscriptedBuiltinWrite(t *testing.T) {
	mutated := strings.Replace(validAction, signCall,
		`        printf -v 'REF[0]' '%s' 'ghcr.io/devantler-tech/platform/manifests:latest'`+"\n"+signCall, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err == nil {
		t.Fatal("`printf -v 'REF[0]'` writes REF; ${REF} returns element zero")
	}
}

// TestRejectsAnInterpreterWrappedExtraPush covers the OTHER direction of the same omission. The
// guard's extra-operation rules can only reject what they can see, so an invocation whose executable
// is `bash` was omitted entirely — and a second publish AFTER the evidence steps is exactly the
// release-without-coverage this check exists to reject.
func TestRejectsAnInterpreterWrappedExtraPush(t *testing.T) {
	mutated := strings.Replace(validAction, reconcileStep,
		"    - name: Sneak\n"+
			"      run: bash ./scripts/run-ksail-prod-with-pull-auth.sh workload push\n"+
			reconcileStep, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err == nil {
		t.Fatal("a publish invoked through an interpreter still moves the tag production reads")
	}
}

// TestRecognisesAnInterpreterWrappedRequiredPush is the same rule in the SATISFYING direction: if
// looking through the interpreter only ever added rejections, the guard would reject a real deploy
// that spelled its push that way.
func TestRecognisesAnInterpreterWrappedRequiredPush(t *testing.T) {
	mutated := strings.Replace(validAction,
		"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push",
		"      run: bash ./scripts/run-ksail-prod-with-pull-auth.sh workload push", 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err != nil {
		t.Fatalf("an interpreter-wrapped push still publishes, got: %v", err)
	}
}

// TestRejectsInlineInterpreterCode is where looking through the interpreter has to stop: `-c` names
// no script, so there is nothing to resolve to and the code is never parsed by this file.
func TestRejectsInlineInterpreterCode(t *testing.T) {
	mutated := strings.Replace(validAction, signCall,
		`        bash -c 'cosign() { true; }'`+"\n"+signCall, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err == nil {
		t.Fatal("`bash -c` runs text this check never sees")
	}
}

// TestAllowsAnUnrelatedInterpretedScript is the over-tightening control for both interpreter rules —
// running a helper script through bash is ordinary scripting and must stay legal.
func TestAllowsAnUnrelatedInterpretedScript(t *testing.T) {
	mutated := strings.Replace(validAction, signCall,
		"        bash ./scripts/collect-metadata.sh\n"+signCall, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err != nil {
		t.Fatalf("running a helper script through bash is ordinary, got: %v", err)
	}
}

// resolutionAssignments are the names whose VALUE decides what a later command resolves to, or what
// the shell reads before running. They need their own table because they are assignments rather than
// commands: the construct walk above sees calls, and an assignment is not one.
//
// The environment-prefix form is the interesting member — `PATH=… cosign sign …` is one statement
// that both rebinds the lookup and issues the required command, so a rule keyed on statements that
// "only assign" would miss it.
var resolutionAssignments = map[string]string{
	"PATH prepend":       `        PATH=/tmp/fake:$PATH` + "\n" + signCall,
	"PATH exported":      `        export PATH=/tmp/fake:$PATH` + "\n" + signCall,
	"BASH_ENV assigned":  `        BASH_ENV=./setup.sh` + "\n" + signCall,
	"PATH as env prefix": `        PATH=/tmp/fake cosign sign --yes --recursive "${REF}"`,
}

func TestRejectsAssignmentsThatRedirectResolution(t *testing.T) {
	for name, replacement := range resolutionAssignments {
		t.Run(name, func(t *testing.T) {
			mutated := strings.Replace(validAction, signCall, replacement, 1)
			mustChange(t, validAction, mutated)

			if err := validate([]byte(mutated)); err == nil {
				t.Fatalf("expected %q to be rejected: it decides what `cosign` resolves to", name)
			}
		})
	}
}

// TestAllowsOrdinaryAssignments is that rule's over-tightening control. The reference signing block
// is mostly assignments — REF and DIGEST — so a rule that treated assignment itself as suspicious
// would reject every real deploy.
func TestAllowsOrdinaryAssignments(t *testing.T) {
	mutated := strings.Replace(validAction, signCall,
		"        export COSIGN_YES=true\n        SUBJECT=\"${REF}\"\n"+signCall, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err != nil {
		t.Fatalf("ordinary assignments are how the real block works, got: %v", err)
	}
}

// TestRejectsAnUnparseableStep closes the same fail-open shape as the unmodeled-construct rule.
// parseExecuted returns no commands for an unparseable block, which correctly refuses to CREDIT a
// required operation but silently hides a forbidden one — so a step that does not parse could carry a
// second publish after the evidence steps and still be reported as satisfying the contract.
func TestRejectsAnUnparseableStep(t *testing.T) {
	mutated := strings.Replace(validAction, reconcileStep,
		"    - name: Sneak\n      run: |\n"+
			"        ./scripts/run-ksail-prod-with-pull-auth.sh workload push\n"+
			"        if [ -z \"$X\"\n"+
			reconcileStep, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err == nil {
		t.Fatal("an unparseable step hides everything in it, including a second publish")
	}
}

// TestRejectsAnUnparseableSigningStep records the case that was ALREADY covered, so the two are not
// confused. An unparseable step carrying a required operation is caught on the satisfy side —
// parseExecuted yields nothing, so no step appears to sign — and removing the rule above leaves this
// test green. That asymmetry is precisely why the rule above is needed: only the forbidding side was
// open.
func TestRejectsAnUnparseableSigningStep(t *testing.T) {
	mutated := strings.Replace(validAction, signCall,
		signCall+"\n        if [ -z \"$X\"\n", 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err == nil {
		t.Fatal("an unparseable signing step cannot be certified either way")
	}
}

// TestRejectsADynamicallyExpandedSetOption closes a fail-open in the option allowlist itself: a word
// whose value is not statically known cannot be checked against the allowlist, so accepting it was
// the allowlist failing open on exactly the input it cannot read.
//
// The consequence runs through errexit rather than through `set` directly. `set "${OPTS}"` left the
// option unchecked, while errexitToggle conservatively recorded errexit as OFF — which made
// parseExecuted skip every command after it, including a second `workload push`.
func TestRejectsADynamicallyExpandedSetOption(t *testing.T) {
	mutated := strings.Replace(validAction, reconcileStep,
		"    - name: Sneak\n      run: |\n        OPTS=-u\n        set \"${OPTS}\"\n"+
			"        ./scripts/run-ksail-prod-with-pull-auth.sh workload push\n"+reconcileStep, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err == nil {
		t.Fatal("an unreadable `set` option cannot be checked, so it cannot be accepted")
	}
}

// TestSeesACommandPrefixedWorkloadOperation is the `command` half of the wrapper rule. `command X …`
// executes X with its arguments, so reading argv[0] returns `command` and hid the operation entirely
// — the same omission as the interpreter form, and fail-open in the same direction.
func TestSeesACommandPrefixedWorkloadOperation(t *testing.T) {
	mutated := strings.Replace(validAction, reconcileStep,
		"    - name: Sneak\n      run: command ./scripts/run-ksail-prod-with-pull-auth.sh workload push\n"+
			reconcileStep, 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err == nil {
		t.Fatal("a `command`-prefixed publish still moves the tag production reads")
	}
}

// TestRecognisesACommandPrefixedRequiredPush is that rule in the satisfying direction, so looking
// through the prefix cannot only ever add rejections.
func TestRecognisesACommandPrefixedRequiredPush(t *testing.T) {
	mutated := strings.Replace(validAction,
		"      run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push",
		"      run: command ./scripts/run-ksail-prod-with-pull-auth.sh workload push", 1)
	mustChange(t, validAction, mutated)

	if err := validate([]byte(mutated)); err != nil {
		t.Fatalf("a command-prefixed push still publishes, got: %v", err)
	}
}

// TestRejectsErrexitDisabledInsideACompoundConstruct covers a state change the two halves of this
// file disagreed about. unmodeledShellState walks the WHOLE tree, so it sees the nested `set +e`,
// finds `e` modeled, and accepts it; parseExecuted iterated only top-level statements and skipped
// the brace group entirely, so errexitToggle never saw the toggle. Bash runs a brace group in the
// CURRENT shell, so errexit really is off — and the validator still credited cosign as
// failure-propagating. Two individually-defensible decisions composing into a permissive one.
func TestRejectsErrexitDisabledInsideACompoundConstruct(t *testing.T) {
	disabled := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		"        { set +e; }\n"+
			`        cosign sign --yes --recursive "${REF}"`+"\n"+
			`        echo "digest=${DIGEST}" >> "${GITHUB_OUTPUT}"`, 1)
	mustChange(t, validAction, disabled)

	if err := validate([]byte(disabled)); err == nil {
		t.Fatal("a brace group runs in the current shell, so `{ set +e; }` disables errexit at the signing call")
	}
}

// TestRejectsDigestResolvedByAShadowedFunction covers the resolver half of command resolution.
// Top-level required commands are already checked against declaredNames, but the digest-resolver
// predicate trusted the literal executable name on its own. bash looks up functions before $PATH,
// so a `docker` function returns a constant while the call still reads as a genuine resolve — the
// old digest is then signed and exported before reconciliation releases the new artifact.
func TestRejectsDigestResolvedByAShadowedFunction(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`,
		`        docker() { printf 'sha256:aaaa'; }`+"\n"+
			`        DIGEST=$(docker buildx imagetools inspect "${REF_TAG}" --format '{{.Manifest.Digest}}')`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("a `docker` function shadows the resolver, so DIGEST never depends on the published tag")
	}
}

// TestRejectsIndirectPathWriteByBuiltin is the PATH sibling of the builtin-write rule that already
// guards DIGEST and REF. unmodeledShellState rejected only a `*syntax.Assign` to PATH, so
// `printf -v PATH …` reached the same cosign shadowing without ever producing an Assign node.
func TestRejectsIndirectPathWriteByBuiltin(t *testing.T) {
	shadowed := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        printf -v PATH '%s' '/tmp/fake:/usr/bin'`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, shadowed)

	if err := validate([]byte(shadowed)); err == nil {
		t.Fatal("`printf -v PATH` rebinds command resolution exactly as an assignment does")
	}
}

// TestRejectsExecWithOptionsButNoCommand covers `exec -c`, which carries a word but names no
// program, so bash CONTINUES the shell. Counting words treated it as terminating, which let a
// later `workload push` hide behind it.
func TestRejectsExecWithOptionsButNoCommand(t *testing.T) {
	hidden := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        cosign sign --yes --recursive "${REF}"`+"\n"+
			`        exec -c`+"\n"+
			`        ksail workload push`, 1)
	mustChange(t, validAction, hidden)

	if err := validate([]byte(hidden)); err == nil {
		t.Fatal("`exec -c` names no program, so the shell continues and the later push runs")
	}
}

// TestRejectsAssignmentFormParameterExpansionWrite covers `: "${DIGEST:=<stale>}"`, which assigns
// without producing an Assign, DeclClause or recognised builtin destination — so the collector saw
// only the earlier valid resolution while bash signed the fallback.
func TestRejectsAssignmentFormParameterExpansionWrite(t *testing.T) {
	stale := strings.Replace(validAction,
		`        cosign sign --yes --recursive "${REF}"`,
		`        unset DIGEST`+"\n"+
			`        : "${DIGEST:=sha256:aaaa}"`+"\n"+
			`        cosign sign --yes --recursive "${REF}"`, 1)
	mustChange(t, validAction, stale)

	if err := validate([]byte(stale)); err == nil {
		t.Fatal("an assignment-form parameter expansion writes DIGEST invisibly to the collector")
	}
}

// TestCompoundConstructShapeMatrix pins the LINE the compound-construct rule draws, in both
// directions at once. The distinction is the SHELL, not the nesting: a construct that runs in the
// step's own shell moves the errexit state parseExecuted tracks and must fail closed, while a
// subshell form changes only a child that exits and must stay legal — rejecting those would fail
// ordinary scripting such as `$(set -euo pipefail; crane digest …)`.
func TestCompoundConstructShapeMatrix(t *testing.T) {
	cases := []struct {
		name       string
		snippet    string
		wantReject bool
	}{
		{"brace group", "        { set +e; }", true},
		{"if branch", "        if true; then set +e; fi", true},
		{"for loop", "        for i in 1; do set +e; done", true},
		{"while loop", "        while false; do set +e; done", true},
		{"function body", "        f() { set +e; }", true},
		{"and-list", "        true && set +e", true},
		{"explicit subshell", "        ( set +e )", false},
		{"backgrounded", "        { set +e; } &", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(validAction,
				`        cosign sign --yes --recursive "${REF}"`,
				tc.snippet+"\n"+`        cosign sign --yes --recursive "${REF}"`+"\n"+
					`        echo "digest=${DIGEST}" >> "${GITHUB_OUTPUT}"`, 1)
			mustChange(t, validAction, mutated)

			err := validate([]byte(mutated))
			if tc.wantReject && err == nil {
				t.Error("must be rejected: this construct runs in the step's own shell")
			}

			if !tc.wantReject && err != nil {
				t.Errorf("must stay legal: a subshell cannot move the tracked state; got: %v", err)
			}
		})
	}
}
