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

	if err := validateCDWiring(repoFile(t, ".github/workflows/cd.yaml")); err != nil {
		t.Fatalf("shipped cd.yaml can deploy to production without the publication contract: %v", err)
	}
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

	if err := validateCDWiring(repoFile(t, ".github/workflows/cd.yaml")); err != nil {
		t.Fatalf("the refusal never reaches the direct-push deploy: %v", err)
	}
}

func TestCDWiringRejectsEachAblation(t *testing.T) {
	t.Parallel()

	cd := repoFile(t, ".github/workflows/cd.yaml")
	if err := validateCDWiring(cd); err != nil {
		t.Fatalf("precondition: the shipped cd.yaml must PASS before ablating it: %v", err)
	}

	// Each ablation removes exactly one link in the chain that makes the gate
	// real, and each must be refused. `unwired` matters most: the job is still
	// present and still runs the validator, so the workflow reads as gated
	// while the deploy no longer waits for it.
	for name, ablate := range map[string]func(string) string{
		"unwired": func(s string) string {
			return strings.Replace(s, ", validate-publication-contract]", "]", 1)
		},
		"gate job removed": func(s string) string {
			return strings.Replace(s, "  validate-publication-contract:", "  unrelated-job:", 1)
		},
		"gate no longer runs the validator": func(s string) string {
			return strings.Replace(s, "go run ./scripts/validate-dr-signing", "echo skipped", 1)
		},
		"needs shape unreadable": func(s string) string {
			return strings.Replace(
				s,
				"    needs: [validate-eks-authorization, validate-publication-contract]",
				"    needs:\n      - validate-eks-authorization\n      - validate-publication-contract",
				1,
			)
		},
		"deploy no longer uses the shared action": func(s string) string {
			return strings.Replace(s, "uses: ./.github/actions/deploy-prod\n", "uses: ./.github/actions/other\n", 1)
		},
		// Raised by CodeRabbit: a substring match is satisfied by a longer path
		// that merely starts with the wanted one, so the tripwire would stay
		// green across exactly the change it exists to notice. Both directions
		// of that mistake are pinned — a sibling action and a nested one.
		"deploy uses a SIBLING action with the same prefix": func(s string) string {
			return strings.Replace(s, "uses: ./.github/actions/deploy-prod\n", "uses: ./.github/actions/deploy-prod-canary\n", 1)
		},
		"deploy uses a NESTED action under the same path": func(s string) string {
			return strings.Replace(
				s,
				"uses: ./.github/actions/deploy-prod\n",
				"uses: ./.github/actions/deploy-prod/publish-platform-manifests\n",
				1,
			)
		},
	} {
		ablated := ablate(cd)
		if ablated == cd {
			t.Fatalf("%s: ablation changed nothing; re-aim it rather than trusting the result", name)
		}
		if err := validateCDWiring(ablated); err == nil {
			t.Fatalf("%s: ablation was accepted, so the check does not constrain it", name)
		}
	}
}

func TestJobNeedsReportsUnreadableShapesAsUnestablished(t *testing.T) {
	t.Parallel()

	// The failure direction that matters: a shape this cannot parse must never
	// come back as a populated list, or an unrecognised workflow style would
	// read as "gated" with nothing having been checked.
	for name, job := range map[string]string{
		"block form":   "    needs:\n      - a\n      - b\n",
		"empty inline": "    needs: []\n",
		"scalar form":  "    needs: validate-eks-authorization\n",
		"absent":       "    runs-on: ubuntu-latest\n",
	} {
		if names, ok := jobNeeds(job); ok {
			t.Fatalf("%s: expected unestablished, got %v", name, names)
		}
	}

	names, ok := jobNeeds("    needs: [a, b]\n")
	if !ok || len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("inline form should parse to [a b], got %v (ok=%v)", names, ok)
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
