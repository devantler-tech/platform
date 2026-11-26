package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestCNPGArchiveProofRequiresAnArchiveSourceAndFirstAttemptMain(t *testing.T) {
	for _, tc := range []struct {
		name, field, value string
		allow              bool
	}{
		{name: "Umami archive", allow: true},
		{name: "rerun", field: "GITHUB_RUN_ATTEMPT", value: "2"},
		{name: "branch", field: "GITHUB_REF", value: "refs/heads/other"},
		{name: "worker selector", field: "LONGHORN_V2_NODE", value: "prod-worker-2"},
		{name: "foreign confirmation", field: "LONGHORN_V2_CONFIRM", value: "prove-longhorn-v2-database-archive-coroot-db"},
		{name: "unconfigured archive", field: "LONGHORN_V2_DATABASE", value: "substrate-db"},
		{name: "malformed run", field: "GITHUB_RUN_ID", value: "9999999999999999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := dispatchFixture()
			env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"] = "prove-database-v2-archive", ""
			env["LONGHORN_V2_DATABASE"], env["LONGHORN_V2_CONFIRM"] = "umami-db", "prove-longhorn-v2-database-archive-umami-db"
			if tc.field != "" {
				env[tc.field] = tc.value
			}
			if err := authorizeCNPGArchiveProof(func(key string) string { return env[key] }); (err == nil) != tc.allow {
				t.Fatalf("unexpected archive proof authorization: %v", err)
			}
		})
	}
}

func TestCNPGArchiveProofEntryPointUsesTheProtectedMainDispatch(t *testing.T) {
	const helper = "PLATFORM_TEST_CNPG_ARCHIVE_PROOF_MAIN"
	if os.Getenv(helper) == "1" {
		os.Args = os.Args[:1]
		main()
		return
	}
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	if !slices.Contains(choices, cnpgArchiveProofOperation) {
		t.Fatal("the protected workflow does not expose the database archive proof")
	}
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"] = cnpgArchiveProofOperation, ""
	env["LONGHORN_V2_DATABASE"], env["LONGHORN_V2_CONFIRM"] = "umami-db", "prove-longhorn-v2-database-archive-umami-db"
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCNPGArchiveProofEntryPointUsesTheProtectedMainDispatch$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), helper+"=1")
	for key, val := range env {
		cmd.Env = append(cmd.Env, key+"="+val)
	}
	// The empty working directory ensures the real executable stops without
	// using Kubernetes or Talos credentials after accepting the dispatch.
	output, err := cmd.CombinedOutput()
	if err == nil || strings.TrimSpace(string(output)) != "Reviewed capacity configuration is unreadable." {
		t.Fatalf("confirmed archive proof did not enter the configuration loader: %v: %s", err, output)
	}
}
