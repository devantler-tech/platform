package main

import (
	"encoding/json"
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
		{providerV2SwitchoverOperation, database + "-1:" + database + "-3", database, true},
		{providerV2SwitchoverOperation, database + "-1:business-site-portal-db-3", database, false},
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
	for _, operation := range []string{replaceProviderCNPGStandbyOperation, resumeProviderCNPGStandbyOperation, recoverProviderCNPGStandbyOperation, providerV2SwitchoverOperation} {
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

func TestProviderCNPGSwitchoverRecordRequiresRetainedOriginalAndTwoV2Standbys(t *testing.T) {
	database := providerCNPGStandbySource("customer-identity-db")
	old := benchmarkResource{Name: database.Name + "-1", Namespace: database.Namespace, UID: "31915a8b-321e-4c0c-a6bc-cc91baf16b4b"}
	target := benchmarkResource{Name: database.Name + "-3", Namespace: database.Namespace, UID: "456a6700-9888-44f4-8e30-aeab136f0352"}
	for _, fault := range []string{"", "missing retained disk", "foreign claim", "missing second V2 member"} {
		t.Run(fault, func(t *testing.T) {
			claim := benchmarkResource{Name: old.Name, Namespace: database.Namespace, UID: "97e7e642-1578-4521-b1db-118522a470d1"}
			original := providerCNPGOriginal{
				Claim:      claim,
				PV:         benchmarkResource{Name: "pvc-" + claim.UID, UID: "cbefa334-ec63-4eb7-abcd-f4ddbb265325"},
				Attachment: benchmarkResource{Name: "csi-original", UID: "bcc16393-bb96-4208-95d1-1422ac960811"},
				Handle:     "916230", Node: "worker",
			}
			other := database.Name + "-4"
			record := umamiV2PromotionRecord{
				Source: "23494e7388b3b1a7fc27da52f9ef5799cfb6ef10", Run: "42", Target: target.Name,
				Witness: umamiV2Promotion{
					Database: benchmarkResource{Name: database.Name, Namespace: database.Namespace}, Previous: old,
					OriginalClaim: original.Claim, OldPV: original.PV, OriginalProvider: &original,
					Generation: 17, PreviousTargetSince: "2026-11-27T06:00:00Z",
					Standbys: map[string]cnpgRecoveredStorage{
						target.Name: {Pod: target}, other: {Pod: benchmarkResource{Name: other, Namespace: database.Namespace}},
					},
					PreviousState: postgresState{SystemID: "7560127483245078101", Timeline: "7", FlushLSN: "0/400"},
				},
			}
			switch fault {
			case "missing retained disk":
				record.Witness.OriginalProvider = nil
			case "foreign claim":
				original.Claim.Name = database.Name + "-4"
			case "missing second V2 member":
				delete(record.Witness.Standbys, other)
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			journal := object{"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": object{"name": cnpgSwitchoverJournalName(database), "namespace": umamiV2SwitchoverNamespace,
					"resourceVersion": "12", "uid": "c6339ec2-6239-4e3f-85fd-fa778d6d761d",
					"labels": object{"app.kubernetes.io/managed-by": "github-actions", "storage.platform.devantler.tech/recovery-run": record.Run}},
				"data": object{"record": string(data)}}
			if _, err := bindCNPGPromotionRecord(database, journal, old.Name, target.Name); (err == nil) != (fault == "") {
				t.Fatalf("provider writer promotion journal: %v", err)
			}
		})
	}
}
