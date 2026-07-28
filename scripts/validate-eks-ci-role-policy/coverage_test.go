package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowsDir is where this repository's GitHub Actions live, relative to this
// package.
const workflowsDir = "../../.github/workflows"

// validatorInvocation is the command that actually runs this gate. A workflow
// only counts as covering a trigger if it runs THIS, not merely if it mentions
// the gate by name.
const validatorInvocation = "go run ./scripts/validate-eks-ci-role-policy"

// workflow is the narrow slice of Actions schema this contract needs.
//
// It is parsed as YAML rather than string-matched on purpose: the workflow
// files describe this very trigger in their comments, so a text search would
// pass on prose alone and keep passing after the trigger itself was deleted.
type workflow struct {
	On   triggerSet `yaml:"on"`
	Jobs map[string]struct {
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// triggerSet is the `on:` block. GitHub accepts three spellings — a mapping
// (`on: {push: {...}}`), a sequence (`on: [push]`) and a bare scalar
// (`on: push`) — and every workflow in this repository currently uses the
// mapping. Decoding all three anyway keeps this guard failing for the ONE
// reason it exists: a genuinely missing push-to-main gate. A strict mapping-only
// decode would instead abort with a parse error the day someone adds an
// unrelated workflow in shorthand, which reads like the contract broke.
//
// The sequence and scalar forms carry no branch filter, so they yield a trigger
// with no branches — correctly not counting as main coverage on their own.
type triggerSet map[string]triggerSpec

func (t *triggerSet) UnmarshalYAML(value *yaml.Node) error {
	*t = make(triggerSet)
	switch value.Kind {
	case yaml.MappingNode:
		return value.Decode((*map[string]triggerSpec)(t))
	case yaml.SequenceNode:
		var names []string
		if err := value.Decode(&names); err != nil {
			return err
		}
		for _, name := range names {
			(*t)[name] = triggerSpec{}
		}
	case yaml.ScalarNode:
		var name string
		if err := value.Decode(&name); err != nil {
			return err
		}
		(*t)[name] = triggerSpec{}
	}
	return nil
}

// triggerSpec captures a trigger's branch filter. `on:` values are heterogenous
// (`merge_group:` is null, `push:` is a mapping), so unmarshalling is lenient
// and a null trigger simply yields no branches.
type triggerSpec struct {
	Branches []string `yaml:"branches"`
}

func (t *triggerSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return nil
	}
	type rawTrigger triggerSpec
	return value.Decode((*rawTrigger)(t))
}

// runsValidator reports whether any job in the workflow actually executes the
// gate.
func (w workflow) runsValidator() bool {
	for _, job := range w.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, validatorInvocation) {
				return true
			}
		}
	}
	return false
}

// coversPushToMain reports whether the workflow runs the gate on a direct push
// to main.
func (w workflow) coversPushToMain() bool {
	push, ok := w.On["push"]
	if !ok {
		return false
	}
	onMain := false
	for _, branch := range push.Branches {
		if branch == "main" {
			onMain = true
			break
		}
	}
	return onMain && w.runsValidator()
}

func loadWorkflows(t *testing.T) map[string]workflow {
	t.Helper()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	loaded := make(map[string]workflow)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		contents, readErr := os.ReadFile(filepath.Join(workflowsDir, name)) //nolint:gosec // Fixed repository path.
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		var parsed workflow
		if err := yaml.Unmarshal(contents, &parsed); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		loaded[name] = parsed
	}
	if len(loaded) == 0 {
		t.Fatal("no workflows parsed — the guard would pass vacuously")
	}
	return loaded
}

// TestAuthorizationGateRunsOnPushToMain pins the coverage gap that let
// f89efff4 break main invisibly.
//
// ci.yaml gates on `pull_request` and `merge_group`; cd.yaml is
// `workflow_dispatch`. A direct push to main fires none of them, so main's own
// checks stayed green while the authorization surface was broken, and the
// failure surfaced only on unrelated PRs (whose CI builds the merge result and
// therefore inherits main).
func TestAuthorizationGateRunsOnPushToMain(t *testing.T) {
	workflows := loadWorkflows(t)

	covering := make([]string, 0, 1)
	for name, parsed := range workflows {
		if parsed.coversPushToMain() {
			covering = append(covering, name)
		}
	}

	if len(covering) == 0 {
		t.Fatalf("no workflow runs %q on push to main — a direct push to main can "+
			"break the authorization surface with every check on main green. "+
			"Restore a push-triggered workflow that runs the gate.", validatorInvocation)
	}
}

// TestAuthorizationGateGuardIsNotVacuous proves the guard above can actually
// fail. A coverage assertion that cannot go RED is worse than none: it reports
// the hole as closed forever.
//
// The negative controls perturb exactly the two conditions the guard depends
// on, so each must flip it to false on its own.
func TestAuthorizationGateGuardIsNotVacuous(t *testing.T) {
	covering := workflow{
		On: triggerSet{"push": {Branches: []string{"main"}}},
		Jobs: map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		}{
			"gate": {Steps: []struct {
				Run string `yaml:"run"`
			}{{Run: validatorInvocation + " ."}}},
		},
	}
	if !covering.coversPushToMain() {
		t.Fatal("positive control failed: a push-to-main workflow that runs the gate must count")
	}

	t.Run("wrong branch does not count", func(t *testing.T) {
		perturbed := covering
		perturbed.On = triggerSet{"push": {Branches: []string{"release"}}}
		if perturbed.coversPushToMain() {
			t.Fatal("a push trigger on another branch must not satisfy main coverage")
		}
	})

	t.Run("wrong trigger does not count", func(t *testing.T) {
		perturbed := covering
		perturbed.On = triggerSet{"pull_request": {Branches: []string{"main"}}}
		if perturbed.coversPushToMain() {
			t.Fatal("a pull_request trigger must not satisfy push-to-main coverage — " +
				"that is precisely the gap f89efff4 slipped through")
		}
	})

	t.Run("naming the gate without running it does not count", func(t *testing.T) {
		perturbed := covering
		perturbed.Jobs = map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		}{
			"gate": {Steps: []struct {
				Run string `yaml:"run"`
			}{{Run: "echo validate-eks-ci-role-policy"}}},
		}
		if perturbed.coversPushToMain() {
			t.Fatal("merely mentioning the validator must not count as running it")
		}
	})
}
