package main

import "regexp"

// pauseDiagnosticDispatchAllowed grants no recovery execution or continuation:
// only the separately confirmed first diagnostic dispatch on reviewed main.
func pauseDiagnosticDispatchAllowed(env func(string) string) bool {
	return env("GITHUB_WORKFLOW_REF") == "devantler-tech/platform/.github/workflows/diagnose-wedding-pause.yaml@refs/heads/main" &&
		env("GITHUB_REPOSITORY") == "devantler-tech/platform" &&
		env("GITHUB_REF") == "refs/heads/main" &&
		env("GITHUB_EVENT_NAME") == "workflow_dispatch" &&
		env("GITHUB_RUN_ATTEMPT") == "1" &&
		env("WEDDING_PAUSE_DIAGNOSTIC_CONFIRM") == "dry-run-retained-wedding-pause" &&
		env("WEDDING_REPAIR_CONFIRM") == "" && env("WEDDING_REPAIR_RESUME_FENCED") == "" &&
		regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(env("GITHUB_SHA"))
}
