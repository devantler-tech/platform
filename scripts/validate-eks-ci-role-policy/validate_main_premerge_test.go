package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const mainPushOnly = "github.event_name == 'push' && github.ref == 'refs/heads/main'"

var premergeReaders = []string{
	"validate-eks-authorization", "validate-shared-publish-pin",
	"validate-talos-kubernetes-compatibility", "validate-rgd-templates",
	"validate-rgd-template-wiring", "validate-scanner-versions",
	"validate-ghcr-fanout-component-gate",
}

func explicitPermissions(value any) (map[string]string, bool) {
	input, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	result := map[string]string{}
	for scope, raw := range input {
		level, ok := raw.(string)
		if !ok || level != "read" && level != "write" && level != "none" {
			return nil, false
		}
		result[scope] = level
	}
	return result, true
}

func premergeMainProblems(w workflow) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if len(w.On) != 2 {
		add("only push/main and workflow_dispatch may trigger validation")
	}
	if _, exists := w.On["workflow_dispatch"]; !exists {
		add("manual branch validation entry point is absent")
	}
	push, exists := w.On["push"]
	if !exists || len(push.Branches) != 1 || push.Branches[0] != "main" || len(push.BranchesIgnore)+len(push.Paths)+len(push.PathsIgnore)+len(push.Tags)+len(push.TagsIgnore) != 0 {
		add("main pushes must remain unfiltered and main-only")
	}
	global, ok := explicitPermissions(w.Permissions)
	if !ok || len(global) != 0 {
		add("workflow permissions must remain explicitly empty")
	}
	if w.Concurrency.Group != "validate-main-${{ github.run_id }}" || w.Concurrency.CancelInProgress {
		add("each subject must retain its non-cancelling validation run")
	}
	for name, parsed := range w.Jobs {
		permissions, ok := explicitPermissions(parsed.Permissions)
		if !ok {
			add("%s permission boundary is not explicit", name)
			continue
		}
		writer := false
		for _, level := range permissions {
			writer = writer || level == "write"
		}
		if writer && normalizeCondition(parsed.If) != mainPushOnly {
			add("%s write permissions must be fenced to push/main", name)
		}
	}
	for name, scope := range map[string]string{"kubescape-baseline": "security-events", "observe-prod-convergence": "actions"} {
		parsed, exists := w.Jobs[name]
		permissions, ok := explicitPermissions(parsed.Permissions)
		if !exists || !ok || permissions[scope] != "write" || normalizeCondition(parsed.If) != mainPushOnly {
			add("%s must retain its writer permission and whole-job push/main fence", name)
		}
	}
	for _, name := range premergeReaders {
		parsed, exists := w.Jobs[name]
		if !exists {
			add("%s original reader job is absent", name)
			continue
		}
		permissions, ok := explicitPermissions(parsed.Permissions)
		if !ok || permissions["contents"] != "read" {
			add("%s must explicitly read its checkout", name)
		}
		for _, level := range permissions {
			if level == "write" {
				add("%s reader acquired write permission", name)
			}
		}
		if normalizeCondition(parsed.If) != "" || errorIsTolerated(parsed.ContinueOnError) {
			add("%s must run unconditionally and report failures", name)
		}
		needs, ok := parsed.needsOf()
		if !ok || len(needs) != 0 {
			add("%s reader must not depend on a skipped writer", name)
		}
		checkout := false
		for _, step := range parsed.Steps {
			if !strings.HasPrefix(step.Uses, "actions/checkout@") {
				continue
			}
			checkout = true
			if step.With.Ref != "" || normalizeCondition(step.If) != "" || errorIsTolerated(step.ContinueOnError) {
				add("%s must check out the triggering revision", name)
			}
		}
		if !checkout {
			add("%s must check out the triggering revision", name)
		}
	}
	return problems
}

func TestValidateMainCanExerciseBranchWithoutWriters(t *testing.T) {
	if problems := premergeMainProblems(loadWorkflows(t)["validate-main.yaml"]); len(problems) != 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

func TestValidateMainPremergeBoundaryRejectsRealWorkflowMutations(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(workflowsDir, "validate-main.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*workflow){
		"remove dispatch":      func(w *workflow) { delete(w.On, "workflow_dispatch") },
		"remove scanner fence": func(w *workflow) { j := w.Jobs["kubescape-baseline"]; j.If = ""; w.Jobs["kubescape-baseline"] = j },
		"remove redeploy fence": func(w *workflow) {
			j := w.Jobs["observe-prod-convergence"]
			j.If = ""
			w.Jobs["observe-prod-convergence"] = j
		},
		"ref-only writer fence": func(w *workflow) {
			j := w.Jobs["observe-prod-convergence"]
			j.If = "github.ref == 'refs/heads/main'"
			w.Jobs["observe-prod-convergence"] = j
		},
		"event-only writer fence": func(w *workflow) {
			j := w.Jobs["observe-prod-convergence"]
			j.If = "github.event_name == 'push'"
			w.Jobs["observe-prod-convergence"] = j
		},
		"permit manual writer": func(w *workflow) {
			j := w.Jobs["observe-prod-convergence"]
			j.If += " || github.event_name == 'workflow_dispatch'"
			w.Jobs["observe-prod-convergence"] = j
		},
		"step-only writer fence": func(w *workflow) {
			j := w.Jobs["observe-prod-convergence"]
			j.Steps[0].If = j.If
			j.If = ""
			w.Jobs["observe-prod-convergence"] = j
		},
		"privileged pull-request trigger": func(w *workflow) { w.On["pull_request_target"] = triggerSpec{} },
		"path-filter main pushes":         func(w *workflow) { p := w.On["push"]; p.Paths = []string{"docs/**"}; w.On["push"] = p },
		"reader write permission": func(w *workflow) {
			j := w.Jobs["validate-shared-publish-pin"]
			j.Permissions.(map[string]any)["packages"] = "write"
			w.Jobs["validate-shared-publish-pin"] = j
		},
		"suppress reader": func(w *workflow) {
			j := w.Jobs["validate-shared-publish-pin"]
			j.If = "false"
			w.Jobs["validate-shared-publish-pin"] = j
		},
		"reader tolerates errors": func(w *workflow) {
			j := w.Jobs["validate-shared-publish-pin"]
			j.ContinueOnError = true
			w.Jobs["validate-shared-publish-pin"] = j
		},
		"reader depends on skipped writer": func(w *workflow) {
			j := w.Jobs["validate-shared-publish-pin"]
			j.Needs = "observe-prod-convergence"
			w.Jobs["validate-shared-publish-pin"] = j
		},
		"reader checks out main": func(w *workflow) {
			j := w.Jobs["validate-shared-publish-pin"]
			j.Steps[0].With.Ref = "main"
			w.Jobs["validate-shared-publish-pin"] = j
		},
		"add unrestricted writer": func(w *workflow) { w.Jobs["other-writer"] = job{Permissions: map[string]any{"contents": "write"}} },
		"missing original reader": func(w *workflow) { delete(w.Jobs, "validate-scanner-versions") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var parsed workflow
			if err := yaml.Unmarshal(data, &parsed); err != nil {
				t.Fatal(err)
			}
			if problems := premergeMainProblems(parsed); len(problems) != 0 {
				t.Fatal("real baseline is not a valid boundary", problems)
			}
			before, err := yaml.Marshal(parsed)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&parsed)
			after, err := yaml.Marshal(parsed)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) == string(after) {
				t.Fatal("mutation did not apply")
			}
			if problems := premergeMainProblems(parsed); len(problems) == 0 {
				t.Fatal("unsafe workflow mutation accepted")
			}
		})
	}
}
