package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFile reads a file from the repository root so the negative controls below
// ablate the REAL production workflow rather than a fixture that only resembles
// it. A fixture can drift into agreeing with the validator while the shipped
// workflow does not.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func TestRealDRWorkflowSatisfiesTheSigningContract(t *testing.T) {
	t.Parallel()

	if err := validateDRWorkflow(repoFile(t, ".github/workflows/dr-rebuild.yaml")); err != nil {
		t.Fatalf("shipped dr-rebuild.yaml violates the signing contract: %v", err)
	}
}

func TestRealClusterConfigAllowsTheDRIdentity(t *testing.T) {
	t.Parallel()

	if err := validateVerifyAllowList(repoFile(t, "ksail.prod.yaml")); err != nil {
		t.Fatalf("shipped ksail.prod.yaml violates the allow-list contract: %v", err)
	}
}

// TestDRWorkflowContractRejectsEachAblation removes exactly ONE guarantee from
// the real workflow per case. Each must fail, and each must fail for its OWN
// reason — a case that passes means the validator never depended on the line it
// claims to pin.
func TestDRWorkflowContractRejectsEachAblation(t *testing.T) {
	t.Parallel()

	workflow := repoFile(t, ".github/workflows/dr-rebuild.yaml")

	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name: "without id-token the job cannot mint a Fulcio certificate",
			mutate: func(w string) string {
				return strings.ReplaceAll(w,
					"      id-token: write # keyless cosign signing (Fulcio OIDC)\n", "")
			},
			wantErr: "id-token: write",
		},
		{
			name: "without a sign step DR publishes an unverifiable artifact",
			mutate: func(w string) string {
				return strings.ReplaceAll(w,
					`cosign sign --yes --recursive "${REF}"`, `echo skip-signing`)
			},
			wantErr: "without signing it",
		},
		{
			name: "signing a mutable tag reintroduces the resolve/sign race",
			mutate: func(w string) string {
				return strings.ReplaceAll(w,
					`REF="ghcr.io/devantler-tech/platform/manifests@${DIGEST}"`,
					`REF="ghcr.io/devantler-tech/platform/manifests:latest"`)
			},
			wantErr: "resolved digest",
		},
		{
			name: "a renamed rebuild job hides the whole contract",
			mutate: func(w string) string {
				return strings.ReplaceAll(w, "\n  rebuild:\n", "\n  rebuild-renamed:\n")
			},
			wantErr: "missing rebuild job",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ablated := testCase.mutate(workflow)
			if ablated == workflow {
				t.Fatalf("ablation changed nothing — the control does not target a real line")
			}

			err := validateDRWorkflow(ablated)
			if err == nil {
				t.Fatalf("ablated workflow passed; the validator does not depend on this line")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("failed for the wrong reason: got %q, want it to mention %q",
					err.Error(), testCase.wantErr)
			}
		})
	}
}

func TestSigningBeforePushingIsRejected(t *testing.T) {
	t.Parallel()

	// Ordering matters independently of presence: a sign step that runs before
	// the push signs whatever the PREVIOUS deploy left on the tag, which
	// verifies green while shipping stale bytes.
	job := strings.Join([]string{
		"    permissions:",
		"      id-token: write # keyless cosign signing (Fulcio OIDC)",
		"    steps:",
		`          REF="ghcr.io/devantler-tech/platform/manifests@${DIGEST}"`,
		`          cosign sign --yes --recursive "${REF}"`,
		"      - name: push",
		"        run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push",
	}, "\n")
	workflow := "jobs:\n  rebuild:\n" + job + "\n  other:\n"

	err := validateDRWorkflow(workflow)
	if err == nil {
		t.Fatal("signing before pushing was accepted")
	}
	if !strings.Contains(err.Error(), "signs before it pushes") {
		t.Fatalf("wrong reason: %v", err)
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
	if !strings.Contains(stdout.String(), "DR signing contract passed.") {
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
