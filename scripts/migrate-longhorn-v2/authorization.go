package main

import (
	"errors"
	"regexp"
)

func authorizeWorkflow(getenv func(string) string, confirmation string) error {
	for key, expected := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/prepare-longhorn-v2-node.yaml@refs/heads/main",
		"LONGHORN_V2_CONFIRM": confirmation,
	} {
		if getenv(key) != expected {
			return errors.New("the confirmed first-attempt main workflow is required")
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(getenv("GITHUB_SHA")) || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(getenv("GITHUB_RUN_ID")) {
		return errors.New("immutable source and workflow-run identities are required")
	}
	return nil
}

func authorizeRefresh(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != "refresh-clients" {
		return errors.New("the explicit worker client refresh operation is required")
	}
	return authorizeWorkflow(getenv, "refresh-longhorn-v2-clients")
}

func authorizeBenchmark(getenv func(string) string, phase string) error {
	if (phase != "v1" && phase != "v2") || getenv("LONGHORN_V2_OPERATION") != "benchmark-"+phase {
		return errors.New("the explicit storage benchmark operation is required")
	}
	return authorizeWorkflow(getenv, "benchmark-longhorn-storage")
}
