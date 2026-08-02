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
			old:     `workload push "${{ steps.staging_reference.outputs.oci_ref }}"`,
			new:     `echo skip-push`,
			wantErr: "staging reference",
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
