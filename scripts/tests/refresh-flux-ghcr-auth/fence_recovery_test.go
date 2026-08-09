package refreshfluxghcrauth

import (
	"os"
	"strings"
	"testing"
)

// The fence report is the supported answer to "prove the prior process is dead
// before explicitly recovering". It must run when a deploy is already refusing
// to start, so it may not depend on the credential path, and it must never
// mutate: releasing a fence is the operator's explicit step, because a Talos
// write cannot be fenced and a surviving holder could still be alive.
func TestFenceReportRunsReadOnlyAndWithoutTheCredentialPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	result := f.runHelper(validConfig(), []string{"--fences"}, nil)

	requireSuccessResult(t, result)
	if !strings.Contains(result.stdout, "GHCR deploy fences") {
		t.Errorf("fence report missing its header; stdout = %q", result.stdout)
	}
	if _, err := os.Stat(f.patchCapture); !os.IsNotExist(err) {
		t.Errorf("fence report wrote the root credential patch (%v); it must not touch the credential path", err)
	}
	for _, forbidden := range []string{"::error::", "command not found"} {
		if strings.Contains(result.stdout+result.stderr, forbidden) {
			t.Errorf("fence report emitted %q; output = %q", forbidden, result.stdout+result.stderr)
		}
	}
}

func TestFenceReportRejectsUnknownFlagsAndAdvertisesItself(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	result := f.runHelper(validConfig(), []string{"--not-a-flag"}, nil)

	if result.exitCode != 64 {
		t.Errorf("unknown flag exit = %d, want 64", result.exitCode)
	}
	if !strings.Contains(result.stderr, "--fences") {
		t.Errorf("usage does not advertise --fences; stderr = %q", result.stderr)
	}
}

// A PID belongs to a runner that no longer exists, so an identity built only
// from one cannot be checked for liveness. Every fence reuses the lease holder,
// so recording the run reference once makes all of them decidable.
func TestFenceHolderIdentityCarriesTheGitHubRunReference(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")

	requireContains(t, script, `sync_lease_holder="${desired_revision:0:16}-$(fence_run_segment)-$$-${RANDOM}"`)
	requireContains(t, script, `printf 'gh%s.%s' "${GITHUB_RUN_ID}" "${GITHUB_RUN_ATTEMPT:-1}"`)
	// Both policy fences derive from the lease holder, so they inherit it.
	requireContains(t, script, `flux_policy_parent_owner="${sync_lease_holder}"`)
	requireContains(t, script, `flux_policy_handoff_owner="${sync_lease_holder}"`)
}

// A refusal that names no recovery route is what turned this class of stall
// into archaeology twice on 2026-08-09.
func TestFenceRefusalsPointAtTheReportAndTheRunbook(t *testing.T) {
	t.Parallel()
	script := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	runbook := readRepositoryFile(t, "docs/dr/runbook.md")

	refusals := []string{
		"holds the synchronization lease",
		"already owns the image-verification policy handoff",
	}
	for _, refusal := range refusals {
		index := strings.Index(script, refusal)
		if index < 0 {
			t.Fatalf("refusal %q not found", refusal)
		}
		line := script[index:]
		if end := strings.Index(line, "\n"); end >= 0 {
			line = line[:end]
		}
		if !strings.Contains(line, "--fences") {
			t.Errorf("refusal %q does not name the fence report: %s", refusal, line)
		}
		if !strings.Contains(line, "runbook.md") {
			t.Errorf("refusal %q does not name the runbook: %s", refusal, line)
		}
	}

	requireContains(t, runbook, "Recover an orphaned GHCR deploy fence")
	// The recovery procedure is worthless if it does not stop the operator
	// releasing a fence that a running deploy legitimately holds.
	requireContains(t, runbook, "gh run view <run-id>")
}
