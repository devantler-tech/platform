package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestProviderCNPGReplacementAdmissionIsScoped(t *testing.T) {
	const database = "customer-identity-db"
	const journalUID = "31915a8b-321e-4c0c-a6bc-cc91baf16b4b"
	const joinUID = "456a6700-9888-44f4-8e30-aeab136f0352"
	const source = "23494e7388b3b1a7fc27da52f9ef5799cfb6ef10"
	const prior = database + "-2:1234:" + source + ":" + journalUID
	for _, test := range []struct {
		operation, confirmation, scope string
		allowed                        bool
	}{
		{replaceProviderCNPGStandbyOperation, database + "-2:" + database + "-3", database, true},
		{replaceProviderCNPGStandbyOperation, database + "-2:" + database + "-2", database, false},
		{replaceProviderCNPGStandbyOperation, database + "-2:business-site-portal-db-3", database, false},
		{replaceProviderCNPGStandbyOperation, "wedding-db-2:wedding-db-3", "wedding-db", false},
		{resumeProviderCNPGStandbyOperation, prior, database, true},
		{resumeProviderCNPGStandbyOperation, prior, "business-site-portal-db", false},
		{recoverProviderCNPGStandbyOperation, prior + ":5678:" + source + ":" + joinUID, database, true},
		{recoverProviderCNPGStandbyOperation, prior + ":5678:" + source + ":foreign", database, false},
	} {
		t.Run(test.operation+"/"+test.confirmation+"/"+test.scope, func(t *testing.T) {
			env := dispatchFixture()
			env["LONGHORN_V2_OPERATION"] = test.operation
			env["LONGHORN_V2_NODE"] = ""
			env["LONGHORN_V2_DATABASE"] = test.scope
			env["LONGHORN_V2_CONFIRM"] = test.operation + ":" + test.confirmation
			got, err := authorizeProviderCNPGReplacement(func(key string) string { return env[key] }, test.operation)
			if (err == nil) != test.allowed || test.allowed && got.Database.Name != test.scope {
				t.Fatalf("provider standby admission: %v; want allowed=%t", err, test.allowed)
			}
		})
	}
}

func TestProviderCNPGReplacementWorkflowEntersReviewedConfigLoader(t *testing.T) {
	const child = "PLATFORM_TEST_PROVIDER_REPLACEMENT_MAIN"
	if os.Getenv(child) == "1" {
		os.Args = os.Args[:1]
		main()
		return
	}
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	for _, operation := range []string{replaceProviderCNPGStandbyOperation, resumeProviderCNPGStandbyOperation, recoverProviderCNPGStandbyOperation} {
		if !slices.Contains(choices, any(operation)) {
			t.Fatalf("the protected workflow cannot invoke %q", operation)
		}
	}
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = replaceProviderCNPGStandbyOperation
	env["LONGHORN_V2_NODE"] = ""
	env["LONGHORN_V2_DATABASE"] = "business-site-portal-db"
	env["LONGHORN_V2_CONFIRM"] = replaceProviderCNPGStandbyOperation + ":business-site-portal-db-2:business-site-portal-db-3"
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProviderCNPGReplacementWorkflowEntersReviewedConfigLoader$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), child+"=1")
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	output, err := cmd.CombinedOutput()
	if err == nil || strings.TrimSpace(string(output)) != "Reviewed capacity configuration is unreadable." {
		t.Fatalf("the confirmed request did not reach the configuration loader: %v: %s", err, output)
	}
}
