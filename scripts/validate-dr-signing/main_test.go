package main

import (
	"bytes"
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
// It is two facts, and BOTH are required. The contract must reject the bad
// input (first half), and the CD deploy job must be unable to run without that
// rejection reaching it (second half). Either alone is satisfied by a workflow
// that still ships unsigned bytes: a validator nothing depends on, or a
// dependency on a validator that accepts anything.
func TestMisorderedPublicationIsRefusedOnTheDirectPushPath(t *testing.T) {
	t.Parallel()

	action := repoFile(t, ".github/actions/deploy-prod/publish-platform-manifests/action.yml")
	if err := validatePublicationAction(action); err != nil {
		t.Fatalf("precondition: the shipped action must PASS before ablating it: %v", err)
	}

	// Push straight to the promoted reference instead of the staging one — the
	// exact ordering defect #2627 tracks.
	misordered := strings.Replace(
		action,
		`workload push "${STAGING_OCI_REF}"`,
		`workload push "${PROMOTED_OCI_REF}"`,
		1,
	)
	if misordered == action {
		t.Fatal("ablation changed nothing; re-aim it rather than trusting the result")
	}
	if err := validatePublicationAction(misordered); err == nil {
		t.Fatal("a mis-ordered publication was accepted by the contract")
	}

	if err := validateCDWiring(cdWorkflow(t), deployAction(t)); err != nil {
		t.Fatalf("the refusal never reaches the direct-push deploy: %v", err)
	}
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
		ablatedCD, ablatedAction := ablate(cd, action)
		if ablatedCD == cd && ablatedAction == action {
			t.Fatalf("%s: ablation changed nothing; re-aim it rather than trusting the result", name)
		}
		if err := validateCDWiring(ablatedCD, ablatedAction); err == nil {
			t.Fatalf("%s: ablation was accepted, so the check does not constrain it", name)
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

	// jobStepRunningCommand: the command quoted inside another command is not run.
	echoed := map[string]any{"steps": []any{map[string]any{"run": "echo \"go run ./x\"\n"}}}
	if _, ok := jobStepRunningCommand(echoed, "go run ./x"); ok {
		t.Fatal("an echoed command must not count as executed")
	}
	executed := map[string]any{"steps": []any{map[string]any{"run": "go test ./x\ngo run ./x\n"}}}
	if _, ok := jobStepRunningCommand(executed, "go run ./x"); !ok {
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

	// jobUsesAction: equality, so a prefix-sharing sibling is a different action.
	sibling := map[string]any{"steps": []any{map[string]any{"uses": "./a/b-canary"}}}
	if jobUsesAction(sibling, "./a/b") {
		t.Fatal("a sibling path sharing a prefix must not satisfy an action check")
	}
	nested := map[string]any{"steps": []any{map[string]any{"uses": "./a/b/c"}}}
	if jobUsesAction(nested, "./a/b") {
		t.Fatal("a nested path must not satisfy an action check")
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
