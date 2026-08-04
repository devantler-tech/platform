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
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// drIdentitySubject is the Fulcio certificate subject the DR rebuild signs
// under: the reusable workflow path at the ref it is dispatched from. It must
// be present in the cluster's matchOIDCIdentity allow-list for Flux to accept a
// DR-published artifact.
const drIdentitySubject = `^https://github\.com/devantler-tech/platform/\.github/workflows/dr-rebuild\.yaml@refs/heads/main$`

// cdContractGateJob is the cd.yaml job that runs this validator on the
// direct-push production path. Named once so the wiring check and the workflow
// cannot drift apart silently.
const cdContractGateJob = "validate-publication-contract"

// mergeQueueContractGateJob is the ci.yaml job that carries this validator on
// the merge-queue production path.
//
// Unlike cdContractGateJob this is NOT a dedicated gate job: `changes` also
// detects changed paths and runs the other contract validators. That difference
// is why the gate-job rule that constrains what else may run beside the
// validator (gateJobRunsOnlyItsValidator) is deliberately NOT applied here —
// every OTHER rule is, because none of them depends on the job being dedicated.
const mergeQueueContractGateJob = "changes"

// mergeQueueProductionJob is a ci.yaml job that reaches production, together
// with whether its PURPOSE requires it to survive a failed dependency.
type mergeQueueProductionJob struct {
	name string
	// mayOverrideDependencyStatus is true only where running after a failed
	// dependency is the job's entire reason to exist. Everywhere else a status
	// function makes a publishing job eligible after the gate before it FAILED,
	// which is the bypass this distinction exists to refuse.
	mayOverrideDependencyStatus bool
}

// mergeQueueProductionJobs are the ci.yaml jobs that reach production. Both
// must go through the checked shared action, or the ordering contract covers
// only the direct-push route.
var mergeQueueProductionJobs = []mergeQueueProductionJob{
	{name: "deploy-prod"},
	// The unsuccessful-path cleanup: it re-deploys main's signed artifact after
	// deploy-prod fails or is cancelled, so `always()` is REQUIRED here and
	// refusing it would break the healing this repo added deliberately.
	{name: "heal-prod-on-failure", mayOverrideDependencyStatus: true},
}

// dependencyStatusOverrides are the GitHub status-check functions that make a
// job eligible even when a job it `needs` has failed. Without one of them a
// job runs only if every dependency succeeded, which is the property the
// merge-queue gate relies on; `success()` is absent because it asserts that
// property rather than overriding it.
var dependencyStatusOverrides = []string{"always", "failure", "cancelled"}

// The exact action paths and command the direct-push gate must wire together.
// Named once so the checks, the workflow, and the error text cannot drift.
const (
	sharedDeployAction    = "./.github/actions/deploy-prod"
	nestedPublisherAction = "./.github/actions/deploy-prod/publish-platform-manifests"
	validatorCommand      = "go run ./scripts/validate-dr-signing .github/workflows/dr-rebuild.yaml ksail.prod.yaml"
)

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

// validateCDWiring checks that the direct-push production path cannot deploy
// without this contract having been checked first, and that the contract is
// checked against the code that route actually runs.
//
// The contract above is only as strong as the weakest route to production, and
// there are two. The merge-queue route runs this validator through ci.yaml,
// which triggers on pull_request and merge_group. cd.yaml is the documented
// direct-push recovery route and has neither event: it is dispatched manually
// after a push to main, went straight from the authorization job into the
// shared deploy action, and so reached production without the ordering contract
// being examined even once.
//
// 🔴 THE WORKFLOW IS PARSED, NOT SCANNED, and that is the whole point of this
// function rather than an implementation preference. Four ways a text scan says
// "gated" about a workflow that is not, each raised against the scanning
// version of this check and each reproduced as an ablation below:
//
//  1. The gate step ECHOES the command instead of running it. A substring match
//     on the run block accepts `echo "go run ./scripts/validate-dr-signing"`,
//     which exits 0 having validated nothing.
//  2. The dependency is read from ANY line of the job text, so a heredoc or an
//     env value containing `needs: [validate-publication-contract]` satisfies
//     it while GitHub sees no dependency at all.
//  3. `needs` is satisfied but the deploy carries `if: always()`, which
//     schedules it after the gate FAILS. Membership alone cannot see this, and
//     it is the most dangerous of the four because the workflow reads correct.
//  4. The contract is checked against the nested publisher while the top-level
//     deploy action no longer invokes it — so the validator keeps verifying an
//     unused file and reports success about code that does not run.
//
// A gate the destructive job does not depend on protects nothing; so does a
// gate it depends on but ignores, and so does a contract checked against code
// that is no longer reached.
func validateCDWiring(cd string, deployAction string) error {
	documents, err := decodeWorkflow(cd)
	if err != nil {
		return fmt.Errorf("direct-push deploy workflow does not parse, so its gating cannot be established: %w", err)
	}
	jobs, ok := documents["jobs"].(map[string]any)
	if !ok {
		return errors.New("direct-push deploy workflow has no jobs mapping")
	}
	deploy, ok := jobs["deploy-prod"].(map[string]any)
	if !ok {
		return errors.New("missing deploy-prod job in the direct-push production workflow")
	}

	// If the deploy job stops delegating to the shared action, this check is
	// aimed at the wrong thing and must be re-aimed rather than left passing.
	// Compared as a whole step value, so neither a sibling path
	// (`…/deploy-prod-canary`) nor the nested publisher satisfies it.
	if _, err := requireEnforcedStep(
		deploy, usesAction(sharedDeployAction),
		errors.New("deploy-prod job no longer uses the shared production deploy action, so this wiring check no longer covers the path it names"),
		"the deploy step in the direct-push deploy-prod job",
	); err != nil {
		return err
	}

	// (4) The contract is validated against the nested publisher. That is only
	// meaningful while the shared action still calls it — and the composite
	// action is PARSED for the same reason cd.yaml is: a YAML block scalar (a
	// heredoc inside a run block) can contain the exact `uses:` text while the
	// action invokes nothing of the sort.
	composite, err := decodeWorkflow(deployAction)
	if err != nil {
		return fmt.Errorf("shared deploy action does not parse, so its publisher link cannot be established: %w", err)
	}
	runs, _ := composite["runs"].(map[string]any)
	if runs == nil {
		return errors.New("shared deploy action has no runs mapping")
	}
	if _, err := requireEnforcedStep(
		map[string]any{"steps": runs["steps"]}, usesAction(nestedPublisherAction),
		fmt.Errorf(
			"the shared deploy action no longer invokes %s as a step, so the publication contract is being checked against code production does not run",
			nestedPublisherAction,
		),
		"the publisher step in the shared deploy action",
	); err != nil {
		return err
	}

	gate, ok := jobs[cdContractGateJob].(map[string]any)
	if !ok {
		return fmt.Errorf(
			"missing %s job: the direct-push production path has no publication-contract gate",
			cdContractGateJob,
		)
	}
	// (1) The command must be a complete executable line of a run block, not
	// text that merely appears inside one — AND the step that carries it must
	// actually run and actually fail the job.
	step, err := requireEnforcedStep(
		gate, runsCommand(validatorCommand),
		fmt.Errorf(
			"%s job does not EXECUTE %q as its own command line, so the gate can report success without validating anything",
			cdContractGateJob, validatorCommand,
		),
		"the validator step in "+cdContractGateJob,
	)
	if err != nil {
		return err
	}
	if err := runBlockRunsOnlyAllowedCommands(
		documents, gate, step, cdContractGateJob, gateRunBlockCommands,
	); err != nil {
		return err
	}
	// Constraining the validator step says nothing about what ran BEFORE it in
	// the same job, and the runner's env/path bridges carry across steps.
	if err := gateJobRunsOnlyItsValidator(gate, cdContractGateJob, gateStepActions); err != nil {
		return err
	}
	// The gate JOB is the same question one level up: a skipped or
	// failure-suppressed job still satisfies a `needs` dependency.
	if err := enforcesFailure(gate, "the "+cdContractGateJob+" job"); err != nil {
		return err
	}

	// (2) Read from the parsed job, so only a real `needs` key counts.
	if !stringListContains(deploy["needs"], cdContractGateJob) {
		return fmt.Errorf(
			"deploy-prod does not require %s, so a direct-push deploy can reach production without the publication contract",
			cdContractGateJob,
		)
	}

	// (3) A job-level condition can schedule the deploy after a FAILED
	// prerequisite. Anything other than an absent condition is refused rather
	// than interpreted: this decides whether a destructive job runs, and a
	// condition expression this cannot evaluate must read as ungated.
	if condition, present := deploy["if"]; present {
		return fmt.Errorf(
			"deploy-prod declares a job-level condition (%v); a condition can schedule the deploy after the gate FAILS, "+
				"so it must be removed or this check extended to prove the condition still requires the gate to succeed",
			condition,
		)
	}
	return nil
}

// validateProductionRoutes checks the OTHER route to production.
//
// 🔴 THE PREMISE OF THIS WHOLE CONTRACT IS "the guarantee is only as strong as
// its weakest route", and until now it checked exactly one of the two. The
// merge-queue route in ci.yaml is the NORMAL path to production; replacing its
// shared-action call passed every check here, so an alternative publication
// implementation could bypass the ordering contract on ordinary deploys while
// the direct-push route stayed perfectly gated.
//
// The job-level condition rule deliberately does NOT apply here: ci.yaml gates
// its deploy on `merge_group`, which is a legitimate and necessary condition.
// What must hold is that each production job invokes the CHECKED action and
// that the invoking step is neither skipped nor failure-suppressed.
func validateProductionRoutes(ci string) error {
	documents, err := decodeWorkflow(ci)
	if err != nil {
		return fmt.Errorf("merge-queue workflow does not parse, so its production routes cannot be established: %w", err)
	}
	jobs, ok := documents["jobs"].(map[string]any)
	if !ok {
		return errors.New("merge-queue workflow has no jobs mapping")
	}
	if err := validateMergeQueueContractGate(documents, jobs); err != nil {
		return err
	}
	for _, production := range mergeQueueProductionJobs {
		jobName := production.name
		job, ok := jobs[jobName].(map[string]any)
		if !ok {
			return fmt.Errorf("merge-queue workflow is missing the %s job", jobName)
		}
		if _, err := requireEnforcedStep(
			job, usesAction(sharedDeployAction),
			fmt.Errorf(
				"merge-queue job %s no longer uses %s, so it can publish to production through logic this contract never checks",
				jobName, sharedDeployAction,
			),
			"the deploy step in merge-queue job "+jobName,
		); err != nil {
			return err
		}
		if err := refuseDependencyStatusOverride(job, production); err != nil {
			return err
		}
		// Enforcing the gate step says nothing about whether this job WAITS for
		// it. Read from the parsed job so only a real `needs` key counts: moving
		// the validator into a job that production does not require would
		// otherwise leave every rule above satisfied and the gate unreachable.
		if !stringListContains(job["needs"], mergeQueueContractGateJob) {
			return fmt.Errorf(
				"merge-queue job %s does not require %s, so it can reach production without the job that runs the publication validator",
				jobName, mergeQueueContractGateJob,
			)
		}
	}
	return nil
}

// validateMergeQueueContractGate holds the ci.yaml validator step to the same
// enforcement rules as its cd.yaml counterpart.
//
// 🔴 THE CONTRACT CHECKED THE MERGE-QUEUE ROUTE'S DEPLOY AND NOT ITS GATE, so
// every bypass already pinned for cd.yaml was reachable on the NORMAL path to
// production while the exceptional one stayed sealed: `if: false`,
// `continue-on-error`, a here-doc, `set +e`, a shadowing shell function, a
// `defaults.run.shell` override, or a `BASH_ENV` injection. `deploy-prod` needs
// `changes`, so a `changes` job that reported success on a neutered validator
// was a green light to production.
//
// The premise of this whole file is that the guarantee is only as strong as its
// weakest route, and the weakest route was the one almost every deploy takes.
func validateMergeQueueContractGate(documents map[string]any, jobs map[string]any) error {
	gate, ok := jobs[mergeQueueContractGateJob].(map[string]any)
	if !ok {
		return fmt.Errorf(
			"merge-queue workflow is missing the %s job, so the publication validator has no home on the route production normally takes",
			mergeQueueContractGateJob,
		)
	}
	// Located by the SAME command equality cd.yaml uses, so the two routes
	// cannot drift into checking different things under one contract.
	step, err := requireEnforcedStep(
		gate, runsCommand(validatorCommand),
		fmt.Errorf(
			"%s job does not EXECUTE %q as its own command line, so the merge-queue route can reach production without validating the publication contract",
			mergeQueueContractGateJob, validatorCommand,
		),
		"the validator step in "+mergeQueueContractGateJob,
	)
	if err != nil {
		return err
	}
	// Straight-line run block, no shell override at any of the three scopes, and
	// no environment variable that could shadow `go` before the script starts.
	if err := runBlockRunsOnlyAllowedCommands(
		documents, gate, step, mergeQueueContractGateJob, gateRunBlockCommands,
	); err != nil {
		return err
	}
	// The job is the same question one level up: a skipped or failure-suppressed
	// job still satisfies the `needs` that production depends on.
	return enforcesFailure(gate, "the "+mergeQueueContractGateJob+" job")
}

// refuseDependencyStatusOverride is the merge-queue counterpart to the
// direct-push route's flat refusal of any job-level condition.
//
// 🔴 THAT CARVE-OUT WAS TOTAL, AND THAT WAS THE BUG. This route permits a
// condition because `merge_group` gating is legitimate and necessary — but
// permitting the key permitted its CONTENTS too, so `always() && <the real
// condition>` read as correct while making the deploy eligible after the
// `changes` job, which runs the signing validator, had failed. The condition is
// therefore permitted and its status functions refused, rather than the key
// being trusted whole.
func refuseDependencyStatusOverride(job map[string]any, production mergeQueueProductionJob) error {
	if production.mayOverrideDependencyStatus {
		return nil
	}
	condition, present := job["if"]
	if !present {
		return nil
	}
	// Whitespace and case are removed rather than matched around: GitHub's
	// expression functions are case-insensitive, and a spelling this cannot
	// read must not fall through to accepted.
	normalised := strings.ToLower(strings.Join(strings.Fields(fmt.Sprintf("%v", condition)), ""))
	for _, override := range dependencyStatusOverrides {
		// The trailing paren is what distinguishes the FUNCTION `failure()` from
		// the ordinary result comparison `needs.x.result == 'failure'`, which
		// overrides nothing and must stay allowed.
		if strings.Contains(normalised, override+"(") {
			return fmt.Errorf(
				"merge-queue job %s conditions its run on %s(), which makes it eligible even when a job it "+
					"`needs` has FAILED — including the job that runs the publication validator, so it can "+
					"publish to production behind a gate that already failed (condition: %v)",
				production.name, override, condition,
			)
		}
	}
	return nil
}

// decodeWorkflow parses a workflow into a generic mapping. `on:` is famously
// decoded as the boolean true by YAML 1.1 readers; nothing here reads that key,
// and the jobs mapping is unaffected.
func decodeWorkflow(contents string) (map[string]any, error) {
	var document map[string]any
	if err := yaml.Unmarshal([]byte(contents), &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, errors.New("empty document")
	}
	return document, nil
}

// requireEnforcedStep finds a step and proves it is enforced, in ONE call.
//
// 🔴 MATCHING AND ENFORCING ARE DELIBERATELY INSEPARABLE HERE. Every reviewed
// round of this file found the same defect at one more site: a step was matched
// and its metadata discarded, so `if: false` or `continue-on-error: true` left
// it named-but-not-run. It happened at the validator step, then the deploy
// step, then the merge-queue steps, then the nested publisher — four rounds,
// one mistake, because "find the step" and "check the step is enforced" were
// two calls and the second was easy to forget.
//
// Returning only through this function makes that omission unrepresentable:
// there is no way to obtain a matched step without its enforcement having been
// checked. That is the structural version of a rule I had been applying by
// memory, and by memory I missed it four times.
func requireEnforcedStep(
	container map[string]any,
	matches func(map[string]any) bool,
	missing error,
	description string,
) (map[string]any, error) {
	steps, _ := container["steps"].([]any)
	for _, raw := range steps {
		step, _ := raw.(map[string]any)
		if step == nil || !matches(step) {
			continue
		}
		if err := enforcesFailure(step, description); err != nil {
			return nil, err
		}
		return step, nil
	}
	return nil, missing
}

// usesAction matches a step whose `uses` is EXACTLY the wanted action.
func usesAction(want string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		uses, _ := step["uses"].(string)
		return strings.TrimSpace(uses) == want
	}
}

// runsCommand matches a step executing `want` as a complete line of its run block.
func runsCommand(want string) func(map[string]any) bool {
	return func(step map[string]any) bool {
		run, _ := step["run"].(string)
		for _, line := range strings.Split(run, "\n") {
			if strings.TrimSpace(line) == want {
				return true
			}
		}
		return false
	}
}

// enforcesFailure rejects a step or job that can be skipped or whose failure is
// suppressed.
//
// 🔴 THE SAME CLASS AS THE JOB-LEVEL CONDITION ON deploy-prod, one level down,
// and missing it left the identical hole: a gate that runs the right command
// under `if: false` never runs it, and one under `continue-on-error: true` runs
// it and then reports success anyway. Either way the workflow reads as gated,
// `needs` is satisfied, and the deploy proceeds on an unenforced check.
//
// Both keys are REFUSED rather than interpreted, for the reason the job-level
// check gives: this decides whether a destructive job runs, so an expression
// this cannot evaluate must read as ungated.
func enforcesFailure(node map[string]any, description string) error {
	if condition, present := node["if"]; present {
		return fmt.Errorf(
			"%s declares a condition (%v); a condition can skip it entirely while the workflow still reads as gated, "+
				"so it must be removed or this check extended to prove the condition always holds",
			description, condition,
		)
	}
	if suppress, present := node["continue-on-error"]; present && suppress != false {
		return fmt.Errorf(
			"%s sets continue-on-error: %v, so it can fail the publication contract and still report success",
			description, suppress,
		)
	}
	return nil
}

// gateRunBlockCommands are the ONLY command lines the publication-contract
// gate's run block may contain. Named here so the check, the workflow, and the
// error text cannot drift apart.
var gateRunBlockCommands = []string{
	"go test ./scripts/validate-dr-signing",
	validatorCommand,
}

// runBlockRunsOnlyAllowedCommands requires every executable line of the gate
// step to be one of `allowed`, so that finding the validator line also means
// the shell reaches it and runs it as itself.
//
// 🔴 THIS REPLACED A DENYLIST, AND THE REPLACEMENT IS THE POINT. Six rounds of
// review found six ways to leave the exact validator line present while running
// nothing: quoting it inside another command, skipping the step, sitting after
// `exit 0`, printing it from a here-doc, discarding its verdict with `set +e`,
// and — reported independently by two reviewers against the previous head —
// shadowing `go` with a shell function, since bash resolves a function before
// an executable. Each round answered with another refused token.
//
// A denylist over shell syntax is an unbounded guessing game, and the sixth
// round is the evidence: `go()`, `{` and `}` carried no refused keyword and no
// refused operator, so a gate that ran neither validator was accepted. Rather
// than add a seventh token and wait for the eighth shape, the run block is
// constrained to an ALLOWLIST of exact command lines. Every bypass above needs
// a line that is not one of the two required commands, so none of them is
// expressible — including the ones nobody has thought of yet.
//
// Deliberately narrow, and narrow in the SAFE direction: a gate that grows a
// legitimate third line fails this check and has to add it here, which is a
// reviewed act, rather than a gate that runs nothing passing quietly.
func runBlockRunsOnlyAllowedCommands(
	workflow map[string]any, job map[string]any, step map[string]any, jobName string, allowed []string,
) error {
	if err := refuseShellOverride(workflow, job, step, jobName); err != nil {
		return err
	}
	if err := envAllowsOnly(workflow, job, step, jobName, gateEnvAllowedNames); err != nil {
		return err
	}
	run, _ := step["run"].(string)
	for _, line := range strings.Split(run, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !slices.Contains(allowed, trimmed) {
			return disallowedGateCommandError(jobName, trimmed, allowed)
		}
	}
	return nil
}

// shellSetting reads the shell configured at ONE scope. A step carries it
// directly; a workflow and a job carry it under `defaults.run`.
//
// A shape this cannot read returns absent, and absent means the CALLER ACCEPTS —
// so this helper is deliberately not fail-closed the way stringListContains is,
// and the reason is specific rather than a house style. GitHub only applies
// `defaults.run.shell` when `defaults` and `run` are both mappings carrying that
// key; anything else is not a working override either, so there is no bypass for
// refusing to read it. Stated explicitly because the earlier draft of this
// comment claimed the opposite direction, and a later round would otherwise
// tighten the code against a guarantee it never made.
func shellSetting(node map[string]any, viaDefaults bool) (any, bool) {
	if node == nil {
		return nil, false
	}
	if !viaDefaults {
		shell, present := node["shell"]
		return shell, present
	}
	defaults, _ := node["defaults"].(map[string]any)
	if defaults == nil {
		return nil, false
	}
	run, _ := defaults["run"].(map[string]any)
	if run == nil {
		return nil, false
	}
	shell, present := run["shell"]
	return shell, present
}

// refuseShellOverride rejects a shell setting at ANY scope GitHub applies to
// the gate step.
//
// 🔴 THE FOURTH SHAPE OF "does the validator's verdict survive", and the third
// time this exact class has been fixed at one site while another stayed open.
// A `shell:` override replaces the default `bash -e {0}` with one that does not
// stop on error, so a failing validator no longer ends the step and a later
// succeeding line carries it to exit 0 — `set +e` by another name. Refusing
// only `steps[*].shell` left BOTH `defaults.run.shell` scopes open, and GitHub
// applies workflow defaults, then job defaults, then the step.
//
// So the scopes are a parameter LIST rather than a sequence of ifs: every
// caller must supply all three, and a fourth scope is a signature change the
// compiler enforces, not a site somebody remembers to visit.
func refuseShellOverride(workflow map[string]any, job map[string]any, step map[string]any, jobName string) error {
	for _, scope := range []struct {
		description string
		node        map[string]any
		viaDefaults bool
	}{
		{"the workflow sets defaults.run.shell", workflow, true},
		{fmt.Sprintf("the %s job sets defaults.run.shell", jobName), job, true},
		{fmt.Sprintf("the validator step in %s overrides the shell", jobName), step, false},
	} {
		if shell, present := shellSetting(scope.node, scope.viaDefaults); present {
			return fmt.Errorf(
				"%s (%v); the default already stops on error, and an override can discard "+
					"the validator failure the gate exists to surface",
				scope.description, shell,
			)
		}
	}
	return nil
}

// gateEnvAllowedNames are the environment variables the publication-contract
// gate's shell may inherit from the workflow. Deliberately EMPTY: the gate runs
// two fixed `go` commands and needs nothing configured.
//
// 🔴 THE SEVENTH SHAPE OF "present but does it run", and it is at a layer the
// run-block allowlist cannot see. `BASH_ENV` names a file bash sources before
// the script, and the runner's default `bash -e {0}` is non-interactive, so a
// `go() { true; }` defined there shadows the executable exactly as the sixth
// round's inline shadow did — while the run block still holds only the two
// permitted lines and passes every existing check.
//
// Refusing the NAME `BASH_ENV` would have been the seventh token, and `PATH`,
// `GOFLAGS` and the exported-function `BASH_FUNC_*` encoding are each another
// one behind it. So this is an allowlist for the same reason the run block is:
// every bypass needs a variable that is not on the list, so none of them is
// expressible — including the ones nobody has thought of yet. A gate that
// genuinely needs a variable adds it here, which is a reviewed act.
var gateEnvAllowedNames []string

// gateStepActions are the only `uses:` actions the gate job may run beside its
// validator step. Named WITHOUT a version so a pinned-digest bump is an
// ordinary dependency update rather than a contract change.
var gateStepActions = []string{
	"actions/checkout",
	"actions/setup-go",
}

// envAllowsOnly rejects an environment variable set at ANY scope GitHub applies
// to the gate step, unless it is named in allowed.
//
// The scopes are a parameter list for the same reason refuseShellOverride's
// are: a fourth scope must be a signature change the compiler enforces, not a
// site somebody remembers to visit.
func envAllowsOnly(
	workflow map[string]any, job map[string]any, step map[string]any, jobName string, allowed []string,
) error {
	for _, scope := range []struct {
		description string
		node        map[string]any
	}{
		{"the workflow", workflow},
		{fmt.Sprintf("the %s job", jobName), job},
		{fmt.Sprintf("the validator step in %s", jobName), step},
	} {
		if scope.node == nil {
			continue
		}
		raw, present := scope.node["env"]
		if !present {
			continue
		}
		// A shape this cannot read is REFUSED rather than skipped: an `env`
		// mapping it fails to parse still reaches the shell, so treating it as
		// absent would accept exactly the payload this check exists to stop.
		env, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf(
				"%s sets env in a shape this check cannot read (%T); the gate's shell inherits it either way, "+
					"so it must be expressed as a plain mapping or removed",
				scope.description, raw,
			)
		}
		for name := range env {
			if !slices.Contains(allowed, name) {
				return fmt.Errorf(
					"%s sets %s, which the publication-contract gate may not inherit (permitted: %s); "+
						"a variable such as BASH_ENV or PATH can shadow the validator while the run block still "+
						"reads correctly — add the name here if it genuinely belongs",
					scope.description, name, allowedNamesForMessage(allowed),
				)
			}
		}
	}
	return nil
}

// gateJobRunsOnlyItsValidator requires every OTHER step in the gate job to be
// one of a small set of setup actions.
//
// 🔴 THE SAME HOLE REACHED WITHOUT AN `env:` KEY ANYWHERE. The runner exposes
// `$GITHUB_ENV` and `$GITHUB_PATH` bridges whose writes apply to every LATER
// step, so a step earlier in this job can export BASH_ENV, or drop a fake `go`
// on PATH, and leave the validator step byte-for-byte unchanged. Both were
// accepted before this check existed: constraining the validator step says
// nothing about what ran before it.
//
// So the job may contain exactly one `run:` step — the validator, already
// constrained above — and otherwise only the named setup actions. An arbitrary
// action is refused too: `uses:` can write those same bridges.
func gateJobRunsOnlyItsValidator(job map[string]any, jobName string, allowedActions []string) error {
	steps, _ := job["steps"].([]any)
	runSteps := 0
	for index, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf(
				"step %d of %s is not a mapping this check can read, and an unreadable step still runs before the validator",
				index+1, jobName,
			)
		}
		if _, carriesRun := step["run"]; carriesRun {
			runSteps++
			if runSteps > 1 {
				return fmt.Errorf(
					"%s runs more than one command step; a step before the validator can export BASH_ENV or a fake go "+
						"through $GITHUB_ENV/$GITHUB_PATH and leave the validator step itself looking correct",
					jobName,
				)
			}
			continue
		}
		uses, _ := step["uses"].(string)
		action, _, _ := strings.Cut(strings.TrimSpace(uses), "@")
		if !slices.Contains(allowedActions, action) {
			return fmt.Errorf(
				"%s runs %q, which is not one of the setup actions this gate may contain (%s); an action can write "+
					"$GITHUB_ENV/$GITHUB_PATH and shadow the validator — add it here if it genuinely belongs",
				jobName, uses, strings.Join(allowedActions, ", "),
			)
		}
	}
	return nil
}

// allowedNamesForMessage renders an allow-list that is usually empty without
// producing a message that trails off after "permitted: ".
func allowedNamesForMessage(allowed []string) string {
	if len(allowed) == 0 {
		return "none"
	}
	return strings.Join(allowed, ", ")
}

func disallowedGateCommandError(jobName string, line string, allowed []string) error {
	return fmt.Errorf(
		"the validator step in %s runs %q, which is not one of the commands this gate may contain (%s); "+
			"an extra line can leave the validator present while the shell never runs it, so the gate is "+
			"restricted to exactly these commands — add the line here if it genuinely belongs",
		jobName, line, strings.Join(allowed, ", "),
	)
}

// stringListContains reports whether a parsed YAML value is a sequence (or a
// lone scalar) containing want. A shape it cannot read returns false, so an
// unrecognised `needs` declaration reads as ungated.
func stringListContains(value any, want string) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) == want
	case []any:
		for _, item := range typed {
			if name, ok := item.(string); ok && strings.TrimSpace(name) == want {
				return true
			}
		}
	}
	return false
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
	// Derived from the workflow path, exactly as the publication action above
	// is — keeping the CLI at two arguments so no caller can run a partial
	// contract by passing fewer paths.
	cdPath := filepath.Clean(filepath.Join(filepath.Dir(workflowPath), "cd.yaml"))
	cd, err := os.ReadFile(cdPath) //nolint:gosec // Derived from the explicit workflow path.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: read direct-push deploy workflow: %v\n", err)
		return 1
	}
	deployActionPath := filepath.Clean(filepath.Join(
		filepath.Dir(workflowPath), "..", "actions", "deploy-prod", "action.yml",
	))
	deployAction, err := os.ReadFile(deployActionPath) //nolint:gosec // Derived from the explicit workflow path.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: read shared deploy action: %v\n", err)
		return 1
	}
	if err := validateCDWiring(string(cd), string(deployAction)); err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: %v\n", err)
		return 1
	}
	ciPath := filepath.Clean(filepath.Join(filepath.Dir(workflowPath), "ci.yaml"))
	ci, err := os.ReadFile(ciPath) //nolint:gosec // Derived from the explicit workflow path.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "dr publication contract: read merge-queue workflow: %v\n", err)
		return 1
	}
	if err := validateProductionRoutes(string(ci)); err != nil {
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
