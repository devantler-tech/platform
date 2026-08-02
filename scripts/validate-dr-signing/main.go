// Command validate-dr-signing checks that the disaster-recovery rebuild can
// publish an artifact production is allowed to trust.
//
// Production's root Flux source is the whole platform: every controller, every
// tenant binding, every policy arrives through it. Signature verification on
// that source is only worth switching on if EVERY writer to the mutable tag
// signs, because Flux rejects what it cannot verify — and a disaster recovery
// rebuild is exactly the moment a rejected artifact is unrecoverable by hand.
//
// Both production paths delegate to one local action. That action pushes a
// run-unique staging reference, signs and attests its resolved digest, and only
// then promotes those exact bytes to latest. The DR job must grant the OIDC and
// attestation permissions that action needs, and its workflow identity must
// appear in the cluster's verification allow-list.
//
// None of these failures is loud at authoring time. With verification off they
// fail silently forever; with verification on they fail during an incident, at
// the one moment nobody can afford to debug a supply-chain policy. Pinning all
// here turns that into a CI failure on the pull request that breaks one.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// drIdentitySubject is the Fulcio certificate subject the DR rebuild signs
// under: the reusable workflow path at the ref it is dispatched from. It must
// be present in the cluster's matchOIDCIdentity allow-list for Flux to accept a
// DR-published artifact.
const drIdentitySubject = `^https://github\.com/devantler-tech/platform/\.github/workflows/dr-rebuild\.yaml@refs/heads/main$`

func validateDRWorkflow(workflow string) error {
	job, ok := extractJob(workflow, "  rebuild:")
	if !ok {
		return errors.New("missing rebuild job")
	}

	if !containsExactLine(job, "      id-token: write # keyless cosign signing (Fulcio OIDC)") {
		return errors.New(
			"rebuild job is missing `id-token: write`, so keyless cosign signing cannot mint a Fulcio certificate",
		)
	}
	if !containsExactLine(job, "      packages: write # push OCI artifacts to GHCR") {
		return errors.New(
			"rebuild job is missing `packages: write`, so the publication transaction cannot update GHCR",
		)
	}
	if !containsExactLine(job, "      attestations: write # write SBOM + SLSA provenance attestations") {
		return errors.New(
			"rebuild job is missing `attestations: write`, so the SBOM and provenance cannot be recorded",
		)
	}
	if !containsLine(job, "uses: ./.github/actions/deploy-prod/publish-platform-manifests") {
		return errors.New(
			"rebuild job must use the shared publication action so normal and disaster-recovery delivery cannot drift",
		)
	}
	if !containsLine(job, "ghcr-token: ${{ secrets.GHCR_TOKEN }}") ||
		!containsLine(job, "hcloud-token: ${{ secrets.HCLOUD_TOKEN }}") {
		return errors.New("rebuild job does not pass the required publication credentials to the shared action")
	}
	return nil
}

func validatePublicationAction(action string) error {
	if !containsLine(action, `STAGING_TAG="staging-${GITHUB_SHA}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"`) {
		return errors.New("publication action must build a unique staging reference from the SHA, run, and attempt")
	}
	if !containsLine(action, "STAGING_OCI_REF: ${{ steps.staging_reference.outputs.oci_ref }}") {
		return errors.New("publication action must use an environment bridge for the generated staging reference")
	}
	pushIdx, ok := lineIndexContaining(action, `workload push "${STAGING_OCI_REF}"`)
	if !ok {
		return errors.New("publication action must push only the allowlisted staging reference")
	}
	resolveIdx, ok := lineIndexContaining(action, `docker buildx imagetools inspect "${STAGING_REF}"`)
	if !ok {
		return errors.New("publication action must resolve the staging reference to an immutable digest")
	}
	signIdx, ok := lineIndexContaining(action, "cosign sign ")
	if !ok {
		return errors.New("publication action would promote the artifact without signing it")
	}
	if !containsLine(action, `cosign sign --yes --recursive "ghcr.io/devantler-tech/platform/manifests@${STAGING_DIGEST}"`) {
		return errors.New(
			"publication action must sign the resolved staging digest rather than a mutable tag",
		)
	}
	sbomIdx, ok := lineIndexContaining(action, "uses: anchore/sbom-action@")
	if !ok {
		return errors.New("publication action is missing SBOM generation")
	}
	attestSBOMIdx, ok := lineIndexContaining(action, "uses: actions/attest@")
	if !ok {
		return errors.New("publication action is missing the required SBOM attestation")
	}
	provenanceIdx, ok := lineIndexContaining(action, "uses: actions/attest-build-provenance@")
	if !ok {
		return errors.New("publication action is missing the required provenance attestation")
	}
	promoteIdx, ok := lineIndexContaining(action, "docker buildx imagetools create --prefer-index=false")
	if !ok {
		return errors.New("publication action must use digest-preserving latest promotion")
	}

	if pushIdx >= resolveIdx || resolveIdx >= signIdx || signIdx >= sbomIdx ||
		sbomIdx >= attestSBOMIdx || attestSBOMIdx >= provenanceIdx || provenanceIdx >= promoteIdx {
		return errors.New("publication action must complete push, resolution, signature, SBOM, and provenance before promotion")
	}
	if strings.Count(action, "steps.resolve_staging.outputs.digest") < 4 {
		return errors.New("publication action must bind every evidence step to the resolved staging digest")
	}
	if !containsLine(action, `"${SUBJECT_NAME}@${STAGING_DIGEST}"`) {
		return errors.New("publication action must promote the exact evidenced digest")
	}
	if !containsLine(action, `if [[ "${LATEST_DIGEST}" != "${STAGING_DIGEST}" ]]; then`) {
		return errors.New("publication action must verify latest resolves to the staged digest")
	}

	return nil
}

func validateVerifyAllowList(config string) error {
	if !containsLine(config, drIdentitySubject) {
		return errors.New(
			"cosign matchOIDCIdentity does not allow the DR rebuild identity, so a DR-published artifact stays unverifiable",
		)
	}
	return nil
}

func extractJob(workflow string, header string) (string, bool) {
	lines := strings.Split(workflow, "\n")
	start := -1
	for i, line := range lines {
		if line == header {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}

	end := len(lines)
	for i := start; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "  ") &&
			!strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(line, ":") {
			end = i
			break
		}
	}

	return strings.Join(lines[start:end], "\n"), true
}

func containsExactLine(block string, want string) bool {
	for _, line := range strings.Split(block, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func containsLine(block string, want string) bool {
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func lineIndexContaining(block string, want string) (int, bool) {
	for i, line := range strings.Split(block, "\n") {
		if strings.Contains(line, want) {
			return i, true
		}
	}
	return 0, false
}

func run(workflowPath string, configPath string, stdout io.Writer, stderr io.Writer) int {
	workflow, err := os.ReadFile(workflowPath) //nolint:gosec // The explicit CLI path is the validator input.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: read workflow: %v\n", err)
		return 1
	}
	config, err := os.ReadFile(configPath) //nolint:gosec // The explicit CLI path is the validator input.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: read cluster config: %v\n", err)
		return 1
	}

	if err := validateDRWorkflow(string(workflow)); err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: %v\n", err)
		return 1
	}
	publisherPath := filepath.Clean(filepath.Join(
		filepath.Dir(workflowPath), "..", "actions", "deploy-prod", "publish-platform-manifests", "action.yml",
	))
	publisher, err := os.ReadFile(publisherPath) //nolint:gosec // Derived from the explicit workflow path.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: read publication action: %v\n", err)
		return 1
	}
	if err := validatePublicationAction(string(publisher)); err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: %v\n", err)
		return 1
	}
	if err := validateVerifyAllowList(string(config)); err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "DR publication contract passed.")
	return 0
}

func runCLI(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: validate-dr-signing <dr-workflow-path> <cluster-config-path>")
		return 2
	}
	return run(args[0], args[1], stdout, stderr)
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
