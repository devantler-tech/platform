package refreshfluxghcrauth

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type yamlParent struct {
	indent int
	key    string
}

func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate contract test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
}

func readRepositoryFile(t *testing.T, relativePath string) string {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join(repositoryRoot(t), relativePath))
	if err != nil {
		t.Fatalf("read %s: %v", relativePath, err)
	}
	return string(contents)
}

func yamlScalarAtPath(document string, path ...string) (string, error) {
	parents := make([]yamlParent, 0, len(path))
	for rawLine := range strings.SplitSeq(document, "\n") {
		line, _, _ := strings.Cut(rawLine, "#")
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}

		for len(parents) > 0 && parents[len(parents)-1].indent >= indent {
			parents = parents[:len(parents)-1]
		}

		currentPath := make([]string, 0, len(parents)+1)
		for _, parent := range parents {
			currentPath = append(currentPath, parent.key)
		}
		currentPath = append(currentPath, key)
		value = strings.TrimSpace(value)
		if slicesEqual(currentPath, path) {
			if value == "" {
				break
			}
			return strings.Trim(value, "\"'"), nil
		}

		if value == "" {
			parents = append(parents, yamlParent{indent: indent, key: key})
		}
	}

	return "", fmt.Errorf("missing scalar YAML path: %s", strings.Join(path, "."))
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func requireContains(t *testing.T, document, expected string) {
	t.Helper()
	if !strings.Contains(document, expected) {
		t.Errorf("expected document to contain %q", expected)
	}
}

func requireNotContains(t *testing.T, document, unexpected string) {
	t.Helper()
	if strings.Contains(document, unexpected) {
		t.Errorf("expected document not to contain %q", unexpected)
	}
}

func requireIndex(t *testing.T, document, needle string) int {
	t.Helper()
	index := strings.Index(document, needle)
	if index < 0 {
		t.Fatalf("expected document to contain %q", needle)
	}
	return index
}

func requireBefore(t *testing.T, earlier, later int, message string) {
	t.Helper()
	if earlier >= later {
		t.Errorf("expected %s: positions %d >= %d", message, earlier, later)
	}
}

func TestKsailProdConfigTalosVersionIgnoresUnrelatedVersionFields(t *testing.T) {
	config := `
spec:
  version: v0.0.0
  cluster:
    kubernetesVersion: v1.36.2
    talos:
      version: v1.13.6
`

	version, err := yamlScalarAtPath(config, "spec", "cluster", "talos", "version")
	if err != nil {
		t.Fatalf("read Talos version: %v", err)
	}
	if version != "v1.13.6" {
		t.Errorf("Talos version = %q, want %q", version, "v1.13.6")
	}
}

func TestDeployActionClusterLifecycleUsesSOPSAuthButPublishKeepsActionsToken(t *testing.T) {
	action := readRepositoryFile(t, ".github/actions/deploy-prod/action.yml")
	workflow := readRepositoryFile(t, ".github/workflows/dr-rebuild.yaml")
	wrapper := "./scripts/run-ksail-prod-with-pull-auth.sh"

	actionReconcile := requireIndex(t, action, "id: reconcile")
	actionUpdate := requireIndex(t, action, "name: 🔄 Update cluster")
	actionReassert := requireIndex(t, action, "id: reassert_flux_ghcr_auth")
	requireContains(t, action[actionReconcile:actionUpdate], "run: "+wrapper+" workload reconcile")
	requireNotContains(t, action[actionReconcile:actionUpdate], "GHCR_TOKEN:")
	requireContains(t, action[actionUpdate:actionReassert], "run: "+wrapper+" cluster update")
	requireNotContains(t, action[actionUpdate:actionReassert], "GHCR_TOKEN:")

	actionPublish := requireIndex(t, action, "id: publish_platform_manifest")
	actionVerify := requireIndex(t, action, "id: verify_flux_ghcr_auth_after_push")
	requireContains(t, action[actionPublish:actionVerify], "uses: ./.github/actions/deploy-prod/publish-platform-manifests")
	requireContains(t, action[actionPublish:actionVerify], "ghcr-token: ${{ inputs.ghcr-token }}")

	drCreate := requireIndex(t, workflow, "name: 🏗️ Create cluster")
	drStage := requireIndex(t, workflow, "id: stage_flux_ghcr_auth")
	requireContains(t, workflow[drCreate:drStage], "run: "+wrapper+" cluster create")
	requireNotContains(t, workflow[drCreate:drStage], "GHCR_TOKEN:")

	drPush := requireIndex(t, workflow, "id: publish_platform_manifest")
	drVerify := requireIndex(t, workflow, "id: verify_flux_ghcr_auth_after_push")
	requireContains(t, workflow[drPush:drVerify], "uses: ./.github/actions/deploy-prod/publish-platform-manifests")
	requireContains(t, workflow[drPush:drVerify], "ghcr-token: ${{ secrets.GHCR_TOKEN }}")

	drReconcile := requireIndex(t, workflow, "name: 🔁 Trigger Flux reconciliation")
	drWait := requireIndex(t, workflow, "name: ⏳ Wait for Flux to settle")
	requireContains(t, workflow[drReconcile:drWait], "run: "+wrapper+" workload reconcile")
	requireNotContains(t, workflow[drReconcile:drWait], "GHCR_TOKEN:")
}

func TestPlatformManifestPublicationMovesLatestOnlyAfterEvidence(t *testing.T) {
	publish := readRepositoryFile(t, ".github/actions/deploy-prod/publish-platform-manifests/action.yml")

	staging := requireIndex(t, publish, "id: staging_reference")
	push := requireIndex(t, publish, "id: push_staging")
	resolve := requireIndex(t, publish, "id: resolve_staging")
	sign := requireIndex(t, publish, "id: cosign_sign")
	sbom := requireIndex(t, publish, "id: generate_sbom")
	attestSBOM := requireIndex(t, publish, "id: attest_sbom")
	attestProvenance := requireIndex(t, publish, "id: attest_provenance")
	promote := requireIndex(t, publish, "id: promote_latest")

	requireBefore(t, staging, push, "unique staging reference before push")
	requireBefore(t, push, resolve, "staging push before digest resolution")
	requireBefore(t, resolve, sign, "digest resolution before signing")
	requireBefore(t, sign, sbom, "signature before SBOM generation")
	requireBefore(t, sbom, attestSBOM, "SBOM generation before attestation")
	requireBefore(t, attestSBOM, attestProvenance, "SBOM attestation before provenance")
	requireBefore(t, attestProvenance, promote, "all evidence before latest promotion")

	requireContains(t, publish[staging:push], "GITHUB_SHA")
	requireContains(t, publish[staging:push], "GITHUB_RUN_ID")
	requireContains(t, publish[staging:push], "GITHUB_RUN_ATTEMPT")
	requireContains(t, publish[push:resolve], "STAGING_OCI_REF: ${{ steps.staging_reference.outputs.oci_ref }}")
	requireContains(t, publish[push:resolve], "workload push \"${STAGING_OCI_REF}\"")
	requireContains(t, publish[resolve:promote], "steps.resolve_staging.outputs.digest")
	requireNotContains(t, publish[staging:promote], "manifests:latest")
	requireContains(t, publish[promote:], "docker buildx imagetools create --prefer-index=false")
	requireContains(t, publish[promote:], "${SUBJECT_NAME}@${STAGING_DIGEST}")
	requireContains(t, publish[promote:], "LATEST_DIGEST")
	requireContains(t, publish[promote:], "STAGING_DIGEST")
}

func TestDeployActionTalosctlIsInstalledBeforeAnyMutatingBridge(t *testing.T) {
	action := readRepositoryFile(t, ".github/actions/deploy-prod/action.yml")
	workflow := readRepositoryFile(t, ".github/workflows/dr-rebuild.yaml")
	ksailConfig := readRepositoryFile(t, "ksail.prod.yaml")
	talosVersion, err := yamlScalarAtPath(ksailConfig, "spec", "cluster", "talos", "version")
	if err != nil {
		t.Fatalf("read production Talos version: %v", err)
	}
	talosVersion = strings.TrimPrefix(talosVersion, "v")

	tests := []struct {
		name           string
		document       string
		requireRestore bool
	}{
		{name: "deploy action", document: action, requireRestore: true},
		{name: "DR workflow", document: workflow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setup := requireIndex(t, test.document, "name: ⚙️ Setup talosctl")
			stage := requireIndex(t, test.document, "id: stage_flux_ghcr_auth")
			requireBefore(t, setup, stage, "talosctl setup before credential staging")
			setupStep := test.document[setup:stage]
			requireContains(t, setupStep, fmt.Sprintf("TALOS_VERSION: %q", talosVersion))
			requireContains(t, setupStep, "talosctl-linux-amd64")
			if test.requireRestore {
				restore := requireIndex(t, test.document, "name: 🔑 Restore talosconfig")
				requireBefore(t, restore, stage, "talosconfig restore before credential staging")
			}
		})
	}
}

func TestDeployActionConsumerStagingPrecedesPublishAndIsReassertedAfterUpdate(t *testing.T) {
	action := readRepositoryFile(t, ".github/actions/deploy-prod/action.yml")
	wrapper := "./scripts/run-ksail-prod-with-pull-auth.sh"

	firstRefresh := requireIndex(t, action, "id: stage_flux_ghcr_auth")
	push := requireIndex(t, action, "id: publish_platform_manifest")
	postPushRefresh := requireIndex(t, action, "id: verify_flux_ghcr_auth_after_push")
	reconcile := requireIndex(t, action, "id: reconcile")
	clusterUpdate := requireIndex(t, action, "run: "+wrapper+" cluster update")
	finalRefresh := requireIndex(t, action, "id: reassert_flux_ghcr_auth\n")

	requireBefore(t, firstRefresh, push, "initial refresh before publish")
	requireBefore(t, push, postPushRefresh, "publish before post-publish refresh")
	requireBefore(t, postPushRefresh, reconcile, "post-publish refresh before reconcile")
	requireBefore(t, reconcile, clusterUpdate, "reconcile before cluster update")
	requireBefore(t, clusterUpdate, finalRefresh, "cluster update before final refresh")
	requireContains(t, action[firstRefresh:push], "--record-runtime-proof")
	requireContains(t, action[firstRefresh:push], "${RUNNER_TEMP}/ghcr-runtime-proof-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.json")
	requireNotContains(t, action[firstRefresh:push], "--check-only")
	requireContains(t, action[postPushRefresh:reconcile], "run: ./scripts/refresh-flux-ghcr-auth.sh --check-only")
	finalRefreshStep := action[finalRefresh:]
	requireContains(t, finalRefreshStep, "!cancelled() &&")
	requireContains(t, finalRefreshStep, "steps.verify_flux_ghcr_auth_after_push.outcome == 'success'")
	requireContains(t, finalRefreshStep, "steps.reconcile.outcome == 'success'")
	requireContains(t, finalRefreshStep, "--reuse-runtime-proof")
	requireContains(t, finalRefreshStep, "${RUNNER_TEMP}/ghcr-runtime-proof-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.json")
	// The fourth reference is the orphaned-fence pre-flight. It is asserted
	// explicitly rather than absorbed into the count, so a future invocation still
	// has to be accounted for here deliberately — which is what this exhaustive
	// count exists to force.
	// Anchored on the step, not its `run:` line: the gating `if:` precedes the
	// command, so a slice starting at `run:` would miss the very condition that
	// keeps this default-off.
	recoverStep := requireIndex(t, action, "- name: 🧯 Recover an orphaned GHCR deploy fence")
	requireBefore(t, recoverStep, firstRefresh, "fence recovery before staging")
	requireContains(t, action[recoverStep:firstRefresh], "inputs.recover-orphaned-fence == 'true'")
	requireContains(t, action[recoverStep:firstRefresh], "run: ./scripts/refresh-flux-ghcr-auth.sh --recover-fences")
	// The gate above keeps this off only while the input's own default does, and
	// the two fail INDEPENDENTLY: a default flipped to "true" leaves the assertion
	// above passing unchanged while arming automatic Lease mutation against prod on
	// every deploy and every heal. Read structurally rather than as a substring, so
	// a `default: "false"` belonging to some other input cannot satisfy it.
	recoverDefault, err := yamlScalarAtPath(action, "inputs", "recover-orphaned-fence", "default")
	if err != nil {
		t.Fatalf("read the fence recovery input default: %v", err)
	}
	if recoverDefault != "false" {
		t.Errorf("recover-orphaned-fence default = %q, want %q", recoverDefault, "false")
	}
	if count := strings.Count(action, "scripts/refresh-flux-ghcr-auth.sh"); count != 4 {
		t.Errorf("refresh helper references = %d, want 4", count)
	}
}

// splitCompositeSteps returns one string per step of a composite action, each
// beginning at its own "- name:" line.
func splitCompositeSteps(document string) []string {
	const marker = "\n    - name:"
	parts := strings.Split(document, marker)
	if len(parts) < 2 {
		return nil
	}
	steps := make([]string, 0, len(parts)-1)
	for _, part := range parts[1:] {
		steps = append(steps, strings.TrimPrefix(marker, "\n")+part)
	}
	return steps
}

// stepDirectives drops comment lines, so a rationale that merely mentions a
// condition is never mistaken for the condition itself.
func stepDirectives(step string) string {
	lines := strings.Split(step, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// A step running the refresh helper without --check-only acquires the
// `ghcr-auth-refresh` Lease, which by design has no automatic expiry takeover:
// Talos machine-config writes expose no fencing token, so a non-empty holder
// always requires explicit human recovery. `always()` also runs after the job
// is cancelled, and a cancelled job is force-killed once the runner's
// post-cancellation grace expires — the EXIT trap's release never lands, and
// every later deploy then fails to acquire the Lease. A merge-queue eviction
// cancels this job routinely, so no lease-acquiring step may remain reachable
// after cancellation.
func TestDeployStepsThatTakeTheSyncLeaseNeverRunAfterCancellation(t *testing.T) {
	action := readRepositoryFile(t, ".github/actions/deploy-prod/action.yml")

	steps := splitCompositeSteps(action)
	if len(steps) == 0 {
		t.Fatal("expected the deploy action to contain composite steps")
	}

	inspected := 0
	for _, step := range steps {
		if !strings.Contains(step, "scripts/refresh-flux-ghcr-auth.sh") {
			continue
		}
		if strings.Contains(step, "--check-only") {
			// Read-only: exits before acquire_sync_lease, so it holds nothing.
			continue
		}
		if strings.Contains(step, "--recover-fences") {
			// Alternative mode: dispatches in the same early block as --fences and
			// exits there, before acquire_sync_lease is even defined, so it holds
			// nothing either. It RELEASES a Lease under CAS, which is the opposite
			// of the hazard this test guards — a step that acquires and is then
			// killed before cleanup. Its own refusals are covered by the
			// fence-autorecovery tests.
			continue
		}
		inspected++
		if strings.Contains(stepDirectives(step), "always()") {
			t.Errorf(
				"step acquires the GHCR sync Lease under always(); a cancelled job is killed before cleanup releases it, wedging every later deploy:\n%s",
				step,
			)
		}
	}
	if inspected != 2 {
		t.Fatalf("lease-acquiring deploy steps inspected = %d, want 2", inspected)
	}
}

func TestDeployActionDisasterRebuildPreflightsThenStagesBeforePublish(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/dr-rebuild.yaml")
	wrapper := "./scripts/run-ksail-prod-with-pull-auth.sh"

	preflight := requireIndex(t, workflow, "run: ./scripts/refresh-flux-ghcr-auth.sh --check-only")
	clusterCreate := requireIndex(t, workflow, "run: "+wrapper+" cluster create")
	stage := requireIndex(t, workflow, "id: stage_flux_ghcr_auth")
	push := requireIndex(t, workflow, "id: publish_platform_manifest")
	verify := requireIndex(t, workflow, "id: verify_flux_ghcr_auth_after_push")
	fanoutVerify := requireIndex(t, workflow, "id: verify_flux_ghcr_fanout")
	openbaoRestore := requireIndex(t, workflow, "name: 🔐 Restore OpenBao from the R2 snapshot mirror")
	postRestoreVerify := requireIndex(t, workflow, "id: reassert_flux_ghcr_after_restore")
	reconcile := requireIndex(t, workflow, "run: "+wrapper+" workload reconcile")

	requireBefore(t, preflight, clusterCreate, "preflight before cluster create")
	requireBefore(t, clusterCreate, stage, "cluster create before credential staging")
	requireBefore(t, stage, push, "credential staging before publish")
	requireBefore(t, push, verify, "publish before verification")
	requireBefore(t, verify, reconcile, "verification before reconcile")
	requireBefore(t, reconcile, fanoutVerify, "reconcile before fanout verification")
	requireBefore(t, fanoutVerify, openbaoRestore, "fanout verification before OpenBao restore")
	requireBefore(t, openbaoRestore, postRestoreVerify, "OpenBao restore before post-restore verification")
	requireContains(t, workflow[stage:push], "run: ./scripts/refresh-flux-ghcr-auth.sh --allow-incomplete-fanout")
	requireContains(t, workflow[verify:reconcile], "run: ./scripts/refresh-flux-ghcr-auth.sh --check-only")
	requireContains(t, workflow[fanoutVerify:], "run: ./scripts/refresh-flux-ghcr-auth.sh\n")
	requireContains(t, workflow[postRestoreVerify:], "if: ${{ !cancelled() && inputs.restore && steps.verify_flux_ghcr_fanout.outcome == 'success' }}")
	if count := strings.Count(workflow, "scripts/refresh-flux-ghcr-auth.sh"); count != 5 {
		t.Errorf("refresh helper references = %d, want 5", count)
	}
}

func TestDeployActionManualDRWaitsForFluxBeforeFullBridge(t *testing.T) {
	runbook := readRepositoryFile(t, "docs/dr/runbook.md")
	ciWorkflow := readRepositoryFile(t, ".github/workflows/ci.yaml")
	manualStart := requireIndex(t, runbook, "# 2. Prove the Git/SOPS pull credential")
	manualEnd := requireIndex(t, runbook, "# 6. ONLY if the OpenBao raft-snapshot")
	manual := runbook[manualStart:manualEnd]

	bootstrap := requireIndex(t, manual, "./scripts/refresh-flux-ghcr-auth.sh --allow-incomplete-fanout")
	reconcile := requireIndex(t, manual, "./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile")
	wait := requireIndex(t, manual, "kubectl --context admin@prod -n flux-system wait")
	fullBridge := requireIndex(t, manual, "./scripts/refresh-flux-ghcr-auth.sh  # prove completed fan-out")

	requireBefore(t, bootstrap, reconcile, "bootstrap before reconcile")
	requireBefore(t, reconcile, wait, "reconcile before Flux wait")
	requireBefore(t, wait, fullBridge, "Flux wait before full bridge")
	requireContains(t, runbook, "workload reconciliation also requires the SOPS key")
	requireContains(t, ciWorkflow, "- 'docs/dr/runbook.md'")
}

func TestDeployActionBridgeDocsAndTestsValidateWithoutDeploying(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/ci.yaml")
	filtersStart := requireIndex(t, workflow, "          filters: |")
	filtersEnd := requireIndex(t, workflow, "\n  validate:")
	filters := workflow[filtersStart:filtersEnd]
	k8sStart := requireIndex(t, filters, "            k8s:")
	k8sEnd := requireIndex(t, filters, "            bridge_validation:")
	k8sFilter := filters[k8sStart:k8sEnd]
	bridgeStart := requireIndex(t, filters, "            bridge_validation:")
	bridgeEnd := requireIndex(t, filters, "            talos:")
	bridgeFilter := filters[bridgeStart:bridgeEnd]

	for _, validationOnlyPath := range []string{
		"scripts/tests/test-refresh-flux-ghcr-auth-safety.sh",
		"scripts/tests/refresh-flux-ghcr-auth/**",
		"docs/dr/runbook.md",
	} {
		t.Run(validationOnlyPath, func(t *testing.T) {
			quotedPath := "- '" + validationOnlyPath + "'"
			requireNotContains(t, k8sFilter, quotedPath)
			requireContains(t, bridgeFilter, quotedPath)
		})
	}

	requireContains(t, workflow, "bridge_validation: ${{ steps.filter.outputs.bridge_validation }}")
	validateStart := requireIndex(t, workflow, "  validate:")
	validateEnd := requireIndex(t, workflow, "  naming:")
	validate := workflow[validateStart:validateEnd]
	requireContains(t, validate, "needs.changes.outputs.bridge_validation == 'true'")
	deployStart := requireIndex(t, workflow, "  deploy-prod:")
	deployEnd := requireIndex(t, workflow, "  heal-prod-on-failure:")
	deploy := workflow[deployStart:deployEnd]
	healStart := requireIndex(t, workflow, "  heal-prod-on-failure:")
	healEnd := requireIndex(t, workflow, "  ci-required-checks:")
	heal := workflow[healStart:healEnd]
	for _, productionJob := range []struct {
		name string
		body string
	}{
		{name: "deploy", body: deploy},
		{name: "heal", body: heal},
	} {
		t.Run(productionJob.name, func(t *testing.T) {
			requireContains(t, productionJob.body, "needs.changes.outputs.k8s == 'true'")
			requireNotContains(t, productionJob.body, "bridge_validation")
		})
	}
}

func TestManualBridgePrerequisitesYQV4IsDocumentedAndPreflighted(t *testing.T) {
	instructions := readRepositoryFile(t, "AGENTS.md")
	runbook := readRepositoryFile(t, "docs/dr/runbook.md")
	library := readRepositoryFile(t, "scripts/ghcr-auth-lib.sh")

	requireContains(t, instructions, "yq v4")
	scenarioOne := requireIndex(t, runbook, "## Scenario 1")
	requireContains(t, runbook[:scenarioOne], "yq v4")
	requireContains(t, library, "require_flux_ghcr_yaml_tool()")
	for _, entrypoint := range []string{
		"scripts/refresh-flux-ghcr-auth.sh",
		"scripts/run-ksail-prod-with-pull-auth.sh",
	} {
		t.Run(filepath.Base(entrypoint), func(t *testing.T) {
			requireContains(t, readRepositoryFile(t, entrypoint), "require_flux_ghcr_yaml_tool")
		})
	}
}

func TestManualBridgePrerequisitesYAMLToolPreflightRejectsMissingOrIncompatibleYQ(t *testing.T) {
	for _, testCase := range []string{"missing", "incompatible"} {
		t.Run(testCase, func(t *testing.T) {
			binDirectory := t.TempDir()
			if testCase == "incompatible" {
				fakeYQ := filepath.Join(binDirectory, "yq")
				if err := os.WriteFile(fakeYQ, []byte("#!/bin/bash\necho 'yq 3.4.3'\n"), 0o755); err != nil {
					t.Fatalf("write incompatible yq: %v", err)
				}
			}

			library := filepath.Join(repositoryRoot(t), "scripts", "ghcr-auth-lib.sh")
			command := exec.Command("/bin/bash", "-c", `source "$1"; require_flux_ghcr_yaml_tool`, "bash", library)
			command.Env = environmentWithPath(os.Environ(), binDirectory)
			var stderr bytes.Buffer
			command.Stderr = &stderr
			err := command.Run()
			if err == nil {
				t.Error("YAML tool preflight succeeded, want non-zero exit")
			}
			requireContains(t, stderr.String(), "yq v4 is required")
		})
	}
}

func environmentWithPath(environment []string, path string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, variable := range environment {
		if strings.HasPrefix(variable, "PATH=") {
			continue
		}
		result = append(result, variable)
	}
	return append(result, "PATH="+path)
}

func TestRequiredPackageCoverageProviderUpjetUnifiReferenceIsPreflighted(t *testing.T) {
	manifest := readRepositoryFile(t, "k8s/providers/hetzner/infrastructure/crossplane/provider-upjet-unifi.yaml")
	helper := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")

	packageLine := ""
	for line := range strings.SplitSeq(manifest, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package: ghcr.io/") {
			packageLine = line
			break
		}
	}
	if packageLine == "" {
		t.Fatal("find private provider package reference")
	}
	packageReference := strings.TrimPrefix(packageLine, "package: ghcr.io/")
	requireContains(t, helper, `"`+packageReference+`"`)
}

func TestRequiredPackageCoverageExactDeclaredKsailOperatorImageIsPreflighted(t *testing.T) {
	helper := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	helmRelease := readRepositoryFile(t, "k8s/bases/infrastructure/controllers/ksail-operator/helm-release.yaml")

	ksailOperatorVersion := ""
	for line := range strings.SplitSeq(helmRelease, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "version:") {
			ksailOperatorVersion = strings.TrimSpace(strings.TrimPrefix(line, "version:"))
			break
		}
	}
	requireContains(t, helper, `"devantler-tech/ksail:v${KSAIL_OPERATOR_VERSION}"`)
	requireContains(t, helper, ".spec.chart.spec.version")
	if ksailOperatorVersion == "" {
		t.Error("declared KSail operator version is empty")
	}
}

func TestTalosRegistryAuthStaticDesiredRevisionCannotClaimVerifiedPull(t *testing.T) {
	staticRevision := readRepositoryFile(t, "talos/cluster/mark-ghcr-pull-revision.yaml")
	helper := readRepositoryFile(t, "scripts/refresh-flux-ghcr-auth.sh")
	staticConfigLines := make([]string, 0)
	for line := range strings.SplitSeq(staticRevision, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		staticConfigLines = append(staticConfigLines, line)
	}
	staticConfig := strings.Join(staticConfigLines, "\n")

	requireContains(t, staticConfig, "ghcr-pull-desired-revision")
	requireNotContains(t, staticConfig, "ghcr-pull-verified-revision")
	requireContains(t, helper, "ghcr-pull-verified-revision")
}

func TestTalosRegistryAuthUsesSupportedTalosDocument(t *testing.T) {
	registryAuth := readRepositoryFile(t, "talos/cluster/authenticate-ghcr-pulls.yaml")

	requireContains(t, registryAuth, "kind: RegistryAuthConfig")
	requireContains(t, registryAuth, "username: ${GHCR_USERNAME}")
	requireContains(t, registryAuth, "password: ${GHCR_TOKEN}")
	requireNotContains(t, registryAuth, "machine:\n")
	requireNotContains(t, registryAuth, "\n---\n")
}

func TestImageVerificationUsesOneSlowFailClosedPolicy(t *testing.T) {
	policyPath := "k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml"
	policy := readRepositoryFile(t, policyPath)
	timeout, err := yamlScalarAtPath(
		policy,
		"spec",
		"webhookConfiguration",
		"timeoutSeconds",
	)
	if err != nil {
		t.Fatalf("read image-verification webhook timeout: %v", err)
	}
	if timeout != "30" {
		t.Errorf("image-verification webhook timeout = %q, want 30", timeout)
	}
	for _, expected := range []string{
		"name: publishapp",
		"name: publishprovider",
		"name: ksailcd",
		"image.startsWith('ghcr.io/devantler-tech/ksail')",
	} {
		requireContains(t, policy, expected)
	}

	kustomization := readRepositoryFile(
		t,
		"k8s/bases/infrastructure/cluster-policies/kustomization.yaml",
	)
	requireContains(t, kustomization, "best-practices/verify-app-images.yaml")
	requireNotContains(t, kustomization, "best-practices/verify-ksail-images.yaml")
}
