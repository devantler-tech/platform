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

// The exact action paths and command the direct-push gate must wire together.
// Named once so the checks, the workflow, and the error text cannot drift.
// mergeQueueProductionJobs are the ci.yaml jobs that reach production. Both
// must go through the checked shared action, or the ordering contract covers
// only the direct-push route.
var mergeQueueProductionJobs = []string{"deploy-prod", "heal-prod-on-failure"}

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
	if err := runBlockIsSimpleSequence(step, cdContractGateJob); err != nil {
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
	for _, jobName := range mergeQueueProductionJobs {
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

// jobUsesAction reports whether any step of the job has `uses` EXACTLY equal to
// the wanted action. Equality, not prefix: a sibling or nested path is a
// different action and must not satisfy a check about this one.
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

// shellControlFlow are the tokens that make a run block something other than a
// straight-line sequence of commands. Any of them means the check can no longer
// say the validator is reached.
// Keywords are matched as WORDS, operators as substrings. `strings.Contains`
// on the keywords was wrong in a way whose failure direction is safe but whose
// MESSAGE lies: `docker buildx …` contains "do" and `gh run download` contains
// "do" too, so a legitimate step was rejected naming a token it does not use.
// A guard that refuses correct work with a false explanation is a guard people
// learn to route around.
var (
	shellKeywords = []string{
		"if", "then", "else", "elif", "fi",
		"for", "while", "until", "do", "done",
		"case", "esac", "exit", "return", "trap", "eval", "source",
	}
	// Redirection and here-docs belong here for the same reason the chaining
	// operators do: `cat <<'EOF' … EOF` contains the validator line verbatim
	// while the step only PRINTS it. Reported against the previous head, where
	// the mutation passed and the contract reported success.
	shellOperators = []string{"&&", "||", ";", "|", "`", "$(", "<<", "<", ">"}
)

// shellWords splits a line on whitespace, which is enough to tell the keyword
// `do` from the command `docker`. It is not a shell lexer and does not need to
// be: anything it cannot separate stays in one token and simply fails to match
// a keyword, which leaves the line accepted only if it also carries no
// operator — and the operator set is what quoting and substitution live in.
func shellWords(line string) []string {
	return strings.Fields(line)
}

// runBlockIsSimpleSequence requires the gate step to be a straight-line
// sequence of commands, so that finding the validator line also means the shell
// reaches it.
//
// 🔴 THE THIRD SHAPE OF THE SAME QUESTION. Whole-line equality closed "the
// command is quoted inside another command"; step metadata closed "the step is
// skipped or its failure suppressed"; neither says the shell EXECUTES the line.
// `exit 0` above it, or `if false; then <command>; fi` around it, both leave the
// exact line present while running nothing.
//
// Properly answering "is this line reached" is a shell interpreter, which does
// not belong in a workflow guard. So the run block is instead constrained to a
// shape where the question does not arise: no control flow, no early exit, no
// chaining. That is deliberately narrow, and narrow in the SAFE direction — a
// legitimate gate that needs a conditional will fail this check and have to
// justify itself, rather than a skipped one passing quietly.
func runBlockIsSimpleSequence(step map[string]any, jobName string) error {
	run, _ := step["run"].(string)
	for _, line := range strings.Split(run, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, word := range shellWords(trimmed) {
			for _, keyword := range shellKeywords {
				if word == keyword {
					return controlFlowError(jobName, keyword, trimmed)
				}
			}
		}
		for _, operator := range shellOperators {
			if strings.Contains(trimmed, operator) {
				return controlFlowError(jobName, operator, trimmed)
			}
		}
	}
	return nil
}

func controlFlowError(jobName string, token string, line string) error {
	return fmt.Errorf(
		"the validator step in %s uses shell control flow or chaining (%q in %q); "+
			"the gate must be a straight-line sequence of commands, or finding the validator line "+
			"does not establish that the shell reaches it",
		jobName, token, line,
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
