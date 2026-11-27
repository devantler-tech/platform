package main

import (
	"slices"
	"testing"
)

func TestWeddingStandbyOperationsRequireExactProtectedConfirmations(t *testing.T) {
	const source = "39504b81e7cf68033f608ababf2e77983e88c42c"
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	resumeIDs := ":100:" + source + ":" + freshTestUID
	for _, row := range []struct {
		operation, operands string
	}{
		{"replace-wedding-v2-standby", "wedding-db-1:wedding-db-4"},
		{"resume-retained-wedding-v2-standby", "wedding-db-1" + resumeIDs},
		{"recover-wedding-standby-name", "wedding-db-1" + resumeIDs + ":200:" + source + ":" + freshTestUID},
	} {
		t.Run(row.operation, func(t *testing.T) {
			if !slices.Contains(choices, any(row.operation)) {
				t.Fatal("the production workflow does not admit the selected Wedding operation")
			}
			for _, fault := range []struct{ key, value string }{
				{}, {"GITHUB_RUN_ATTEMPT", "2"}, {"GITHUB_REF", "refs/heads/other"},
				{"LONGHORN_V2_DATABASE", "umami-db"}, {"LONGHORN_V2_NODE", "prod-worker-2"},
				{"LONGHORN_V2_CONFIRM", row.operation + ":umami-db-1:wedding-db-4"},
				{"LONGHORN_V2_OPERATION", umamiStandbyReplacementOperation},
			} {
				env := dispatchFixture()
				env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"] = row.operation, ""
				env["LONGHORN_V2_DATABASE"], env["LONGHORN_V2_CONFIRM"] = "wedding-db", row.operation+":"+row.operands
				if fault.key != "" {
					env[fault.key] = fault.value
				}
				confirmed, err := authorizeWeddingStandbyOperation(func(key string) string { return env[key] }, row.operation)
				if fault.key == "" && (err != nil || confirmed.Original != "wedding-db-1") {
					t.Fatalf("the exact Wedding grant was rejected: %v", err)
				}
				if fault.key != "" && err == nil {
					t.Fatalf("an invalid %s authorized a Wedding write", fault.key)
				}
			}
		})
	}
}
