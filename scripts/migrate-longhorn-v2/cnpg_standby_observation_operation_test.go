package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestUmamiStandbyObservationRequiresAnExactFirstAttemptMainDispatch(t *testing.T) {
	for _, tc := range []struct {
		field, value string
	}{
		{},
		{field: "GITHUB_RUN_ATTEMPT", value: "2"},
		{field: "GITHUB_REF", value: "refs/heads/other"},
		{field: "LONGHORN_V2_NODE", value: "prod-worker-2"},
		{field: "LONGHORN_V2_DATABASE", value: "coroot-db"},
		{field: "LONGHORN_V2_CONFIRM", value: "observe-umami-v2-standby:umami-db-1:umami-db-1"},
		{field: "LONGHORN_V2_CONFIRM", value: "observe-umami-v2-standby:umami-db-01:umami-db-4"},
		{field: "LONGHORN_V2_CONFIRM", value: "observe-umami-v2-standby:umami-db-1:coroot-db-4"},
		{field: "LONGHORN_V2_CONFIRM", value: "observe-umami-v2-standby:umami-db-1:umami-db-4:extra"},
	} {
		env := umamiStandbyDispatch()
		if tc.field != "" {
			env[tc.field] = tc.value
		}
		target, spare, err := authorizeUmamiStandbyObservation(func(key string) string { return env[key] })
		if tc.field == "" {
			if err != nil || target != "umami-db-1" || spare != "umami-db-4" {
				t.Fatalf("confirmed identities were rejected: %v", err)
			}
		} else if err == nil || target != "" || spare != "" {
			t.Fatalf("invalid %s=%q was authorized", tc.field, tc.value)
		}
	}
}

func TestUmamiStandbyObservationEntryPointUsesTheProtectedMainDispatch(t *testing.T) {
	const helper = "PLATFORM_TEST_UMAMI_STANDBY_OBSERVATION_MAIN"
	if os.Getenv(helper) == "1" {
		os.Args = os.Args[:1]
		main()
		return
	}
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	if !slices.Contains(choices, umamiStandbyObservationOperation) {
		t.Fatal("the production workflow cannot run the standby observation")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUmamiStandbyObservationEntryPointUsesTheProtectedMainDispatch$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), helper+"=1")
	for key, val := range umamiStandbyDispatch() {
		cmd.Env = append(cmd.Env, key+"="+val)
	}
	// The executable must accept the request before stopping at the missing
	// config, with no production client or credentials.
	output, err := cmd.CombinedOutput()
	if err == nil || strings.TrimSpace(string(output)) != "Reviewed capacity configuration is unreadable." {
		t.Fatalf("confirmed observation did not enter the configuration loader: %v: %s", err, output)
	}
}

func umamiStandbyDispatch() map[string]string {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"] = umamiStandbyObservationOperation, ""
	env["LONGHORN_V2_DATABASE"], env["LONGHORN_V2_CONFIRM"] = "umami-db", "observe-umami-v2-standby:umami-db-1:umami-db-4"
	return env
}
