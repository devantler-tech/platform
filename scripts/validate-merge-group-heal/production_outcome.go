package main

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// The heal skips its restore deploy only on this one output (#4937). It may
// claim that production was untouched only when the queue preflight refused the
// entry and the composite never started, so the step that writes it is pinned:
// its position (straight after the composite, last in the job), its
// unconditional run, the two outcomes it reads and its exact script. Anything
// looser could tell the heal to stand down after a deploy that did run.
const (
	productionOutcomeStepID = "production-outcome"
	productionDeployStepID  = "production-deploy"
	productionOutcomeOutput = "${{ steps.production-outcome.outputs.stopped_before_production }}"
	productionOutcomeScript = `if [[ "${PREFLIGHT_OUTCOME}" == 'failure' && "${PRODUCTION_OUTCOME}" == 'skipped' ]]; then
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

	steps := deploy.Steps
	if len(steps) < 3 {
		return errors.New("deploy job is missing its preflight, composite or outcome step")
	}
	outcome, composite, preflight := steps[len(steps)-1], steps[len(steps)-2], steps[len(steps)-3]
	if outcome.ID != productionOutcomeStepID {
		return errors.New("the production-outcome step must be the deploy job's last step")
	}
	if composite.Uses != deployCompositePath || composite.ID != productionDeployStepID {
		return errors.New("the production-outcome step must directly follow the identified production composite")
	}
	if preflight.ID != "queue-preflight" {
		return errors.New("the production composite must directly follow the queue preflight")
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
