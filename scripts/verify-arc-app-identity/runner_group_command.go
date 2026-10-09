package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// The group command has its own exact workflow binding and verdict whitelist.
// Identity-only success cannot satisfy a runner-group capability proof.
func runRunnerGroup(args []string, root string, output io.Writer, runtime func(configuration) outcome) int {
	result := outcome("HOLD_INVOCATION")
	if len(args) == 1 && (args[0] == "--preflight-runner-group" || args[0] == "--verify-runner-group") {
		if args[0] == "--preflight-runner-group" || protectedRunnerGroupInvocation() {
			config, status := loadConfiguration(root)
			result = status
			if status == pass {
				if args[0] == "--preflight-runner-group" {
					result = "TRANSPORT_CONFIG_READY"
				} else {
					result = runtime(config)
				}
			}
		}
	}
	switch result {
	case "PASS_EXISTING", "PASS_DISPOSABLE", "HOLD_CAPABILITY", "HOLD_OWNERSHIP", "HOLD_INVOCATION", "HOLD_READER", "TRANSPORT_CONFIG_READY", holdTransport, holdEntry, failTransport, failIdentity, failAPI, failCleanup, failConfig:
	case pass:
		result = "HOLD_CAPABILITY"
	default:
		result = failAPI
	}
	_, _ = fmt.Fprintf(output, "ARC_RUNNER_GROUP_CAPABILITY=%s\n", result)
	if result == "PASS_EXISTING" || result == "PASS_DISPOSABLE" || result == "TRANSPORT_CONFIG_READY" {
		return 0
	}
	return 1
}

func protectedRunnerGroupInvocation() bool {
	for name, expected := range map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1",
		"ARC_RUNNER_GROUP_CONFIRM": "verify-arc-runner-group-capability",
		"GITHUB_WORKFLOW_REF":      "devantler-tech/platform/.github/workflows/verify-arc-runner-group-capability.yaml@refs/heads/main",
	} {
		if os.Getenv(name) != expected {
			return false
		}
	}
	if _, err := positiveID(os.Getenv("GITHUB_RUN_ID")); err != nil {
		return false
	}
	sha := os.Getenv("GITHUB_SHA")
	if len(sha) != 40 || sha != os.Getenv("GITHUB_WORKFLOW_SHA") {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}
