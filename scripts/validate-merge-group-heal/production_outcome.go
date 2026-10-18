package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The heal skips its restore deploy only on this one output (#4937). It may
// claim that production was untouched only when the queue preflight refused the
// entry and the composite never started, so the step that writes it is pinned:
// its position (straight after the composite, last in a job of exactly five
// steps), its unconditional run, the two outcomes it reads and its exact
// script, which also stands down only on a first attempt: a re-run of a failed
// deploy finds the PR already out of the queue and refuses, while an earlier
// attempt may have left its artifact in production. Anything
// looser could tell the heal to stand down after a deploy that did run.
const (
	productionOutcomeStepID = "production-outcome"
	productionDeployStepID  = "production-deploy"
	productionOutcomeOutput = "${{ steps.production-outcome.outputs.stopped_before_production }}"
	queuePreflightCommand   = "go run ./scripts/verify-merge-group-queued check"
	productionOutcomeScript = `if [[ "${GITHUB_RUN_ATTEMPT}" == '1' && "${PREFLIGHT_OUTCOME}" == 'failure' && "${PRODUCTION_OUTCOME}" == 'skipped' ]]; then
  printf 'stopped_before_production=true\n' >>"${GITHUB_OUTPUT}"
fi`
)

type outcomeWorkflow struct {
	Jobs map[string]struct {
		Outputs map[string]string `yaml:"outputs"`
		Steps   []struct {
			ID       string            `yaml:"id"`
			If       any               `yaml:"if"`
			Uses     string            `yaml:"uses"`
			Run      string            `yaml:"run"`
			Shell    string            `yaml:"shell"`
			Continue any               `yaml:"continue-on-error"`
			Env      map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// validateProductionOutcome checks the deploy job's stopped-before-production
// output against the step that alone may write it.
func validateProductionOutcome(workflow string) error {
	var parsed outcomeWorkflow
	if err := yaml.Unmarshal([]byte(workflow), &parsed); err != nil {
		return fmt.Errorf("parse workflow: %w", err)
	}
	deploy, ok := parsed.Jobs["deploy-prod"]
	if !ok {
		return errors.New("missing deploy-prod job")
	}
	if len(deploy.Outputs) != 1 || deploy.Outputs["stopped_before_production"] != productionOutcomeOutput {
		return errors.New("deploy job must export only the stopped-before-production outcome step's answer")
	}

	// The report vouches for every step before it, so the job holds exactly the
	// five it can vouch for: checkout, Go setup, preflight, composite, report. A
	// step added anywhere could touch production before a refused preflight.
	steps := deploy.Steps
	if len(steps) != 5 ||
		!strings.HasPrefix(steps[0].Uses, "actions/checkout@") || steps[0].ID != "" || steps[0].If != nil ||
		!strings.HasPrefix(steps[1].Uses, "actions/setup-go@") || steps[1].ID != "" || steps[1].If != nil {
		return errors.New("deploy job must be exactly checkout, Go setup, queue preflight, production composite and outcome report")
	}
	outcome, composite := steps[4], steps[3]
	if outcome.ID != productionOutcomeStepID {
		return errors.New("the production-outcome step must be the deploy job's last step")
	}
	if composite.Uses != deployCompositePath || composite.ID != productionDeployStepID {
		return errors.New("the production-outcome step must directly follow the identified production composite")
	}
	if err := validateOutcomePreflight(workflow); err != nil {
		return err
	}
	for i, step := range steps {
		if i != len(steps)-1 && step.ID == productionOutcomeStepID {
			return errors.New("only one step may be the production-outcome step")
		}
		if i != len(steps)-2 && step.Uses == deployCompositePath {
			return errors.New("the deploy job may reach production only once")
		}
	}

	if condition, ok := outcome.If.(string); !ok || condition != "always()" {
		return errors.New("the production-outcome step must run unconditionally with always()")
	}
	if outcome.Continue != nil || outcome.Uses != "" || outcome.Shell != "" {
		return errors.New("the production-outcome step must be a plain run step that cannot suppress a failure")
	}
	if len(outcome.Env) != 2 ||
		outcome.Env["PREFLIGHT_OUTCOME"] != "${{ steps.queue-preflight.outcome }}" ||
		outcome.Env["PRODUCTION_OUTCOME"] != "${{ steps."+productionDeployStepID+".outcome }}" {
		return errors.New("the production-outcome step must read exactly the preflight and composite outcomes")
	}
	if strings.TrimSpace(outcome.Run) != productionOutcomeScript {
		return errors.New("the production-outcome step must run exactly the pinned script")
	}
	return nil
}

// The report vouches that nothing before the composite reached production, so
// everything that shapes those steps is checked against what is allowed, never
// against a list of what is forbidden: the workflow's top level and the deploy
// job may hold only the keys they hold today (no `env`, `defaults`,
// `strategy`, `container`, `services` or `continue-on-error`, any of which
// could change what the earlier steps run), checkout and Go setup only their
// current inputs, and the preflight only its name, id, exact environment and
// exact command. Anything else could let an earlier step change production and
// then fail, which the report would read as a refusal and the heal would skip.
var (
	allowedWorkflowKeys  = []string{"name", "on", "permissions", "concurrency", "jobs"}
	allowedDeployJobKeys = []string{"name", "needs", "if", "runs-on", "environment", "concurrency", "permissions", "outputs", "steps"}
	allowedCheckoutWith  = []string{"persist-credentials", "fetch-depth"}
	allowedSetupGoWith   = []string{"go-version-file", "cache"}
	queuePreflightEnv    = map[string]string{
		"GH_TOKEN":      "${{ github.token }}",
		"QUEUE_RECEIPT": "${{ needs.changes.outputs.queue_receipt }}",
	}
)

// onlyKeys reports the first key of m that allowed does not list.
func onlyKeys(what string, m map[string]any, allowed ...string) error {
	for key := range m {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("%s may not set %q", what, key)
		}
	}
	return nil
}

// validateOutcomePreflight checks everything that shapes the steps before the
// composite against the allow-lists above.
func validateOutcomePreflight(workflow string) error {
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(workflow), &parsed); err != nil {
		return fmt.Errorf("parse workflow: %w", err)
	}
	if err := onlyKeys("the workflow", parsed, allowedWorkflowKeys...); err != nil {
		return err
	}
	jobs, _ := parsed["jobs"].(map[string]any)
	deploy, _ := jobs["deploy-prod"].(map[string]any)
	if err := onlyKeys("the deploy job", deploy, allowedDeployJobKeys...); err != nil {
		return err
	}
	rawSteps, _ := deploy["steps"].([]any)
	steps := make([]map[string]any, 0, len(rawSteps))
	for _, raw := range rawSteps {
		step, ok := raw.(map[string]any)
		if !ok {
			return errors.New("every deploy step must be a mapping")
		}
		steps = append(steps, step)
	}
	if len(steps) != 5 {
		return errors.New("deploy job must be exactly checkout, Go setup, queue preflight, production composite and outcome report")
	}

	for i, inputs := range [][]string{allowedCheckoutWith, allowedSetupGoWith} {
		if err := onlyKeys(fmt.Sprintf("deploy step %d", i+1), steps[i], "name", "uses", "with"); err != nil {
			return err
		}
		with, _ := steps[i]["with"].(map[string]any)
		if err := onlyKeys(fmt.Sprintf("deploy step %d's inputs", i+1), with, inputs...); err != nil {
			return err
		}
	}

	preflight := steps[2]
	if err := onlyKeys("the queue preflight", preflight, "name", "id", "env", "run"); err != nil {
		return err
	}
	if id, _ := preflight["id"].(string); id != "queue-preflight" {
		return errors.New("the production composite must directly follow the queue preflight")
	}
	if run, _ := preflight["run"].(string); strings.TrimSpace(run) != queuePreflightCommand {
		return errors.New("the queue preflight must run exactly " + queuePreflightCommand)
	}
	env, ok := preflight["env"].(map[string]any)
	if !ok || len(env) != len(queuePreflightEnv) {
		return errors.New("the queue preflight must set exactly its pinned environment")
	}
	for name, want := range queuePreflightEnv {
		if got, _ := env[name].(string); got != want {
			return fmt.Errorf("the queue preflight's %s must be %s", name, want)
		}
	}
	return nil
}
