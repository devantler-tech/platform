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
