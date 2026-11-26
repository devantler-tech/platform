package main

import (
	"context"
	"testing"
)

func recoveryObjectStoreFixture(t *testing.T) object {
	t.Helper()
	c, r, _ := recoveryPlanningFixture(t)
	plan, err := planCNPGRecovery(context.Background(), c, r)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Resources[2]
}

// TestRecoveryObjectStoreAdmissionPreservesOnlyTheOriginalArchive forbids a writer or retention policy.
func TestRecoveryObjectStoreAdmissionPreservesOnlyTheOriginalArchive(t *testing.T) {
	proposed := recoveryObjectStoreFixture(t)
	admitted, err := cloneRecoverySpec(proposed)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := matchRecoveryObjectStoreAdmission(proposed, admitted)
	if err != nil || !recoveryJSONEqual(actual, value(proposed, "spec")) {
		t.Fatalf("exact restore store refused: %v", err)
	}
}

// TestRecoveryObjectStoreAdmissionRefusesRetentionOrUnexpectedServerDefaults guards all admitted fields.
func TestRecoveryObjectStoreAdmissionRefusesRetentionOrUnexpectedServerDefaults(t *testing.T) {
	for name, change := range map[string]func(object){
		"retention": func(o object) { o["spec"].(object)["retentionPolicy"] = "1d" },
		"sidecar environment": func(o object) {
			o["spec"].(object)["instanceSidecarConfiguration"] = object{"env": []any{object{"name": "AWS_ENDPOINT_URL", "value": "https://foreign.invalid"}}}
		},
		"unknown field": func(o object) { o["spec"].(object)["unknown"] = true },
		"different archive": func(o object) {
			value(o, "spec", "configuration").(object)["destinationPath"] = "s3://foreign/cnpg/umami-db"
		},
		"different endpoint": func(o object) { value(o, "spec", "configuration").(object)["endpointURL"] = "https://foreign.invalid" },
		"different credential": func(o object) {
			value(o, "spec", "configuration", "s3Credentials", "accessKeyId").(object)["name"] = "foreign"
		},
		"wrong namespace": func(o object) { o["metadata"].(object)["namespace"] = "umami" },
		"deleting":        func(o object) { o["metadata"].(object)["deletionTimestamp"] = "2026-11-03T22:00:00Z" },
		"owner":           func(o object) { o["metadata"].(object)["ownerReferences"] = []any{object{"uid": "foreign"}} },
		"run": func(o object) {
			value(o, "metadata", "labels").(object)["storage.platform.devantler.tech/recovery-run"] = "5678"
		},
	} {
		t.Run(name, func(t *testing.T) {
			proposed := recoveryObjectStoreFixture(t)
			admitted, err := cloneRecoverySpec(proposed)
			if err != nil {
				t.Fatal(err)
			}
			change(admitted)
			if got, err := matchRecoveryObjectStoreAdmission(proposed, admitted); err == nil || got != nil {
				t.Fatal("unsafe admitted store accepted")
			}
		})
	}
}

// TestRecoveryObjectStoreCannotProposeItsOwnUnsafeExpectation rejects identical unsafe input and output.
func TestRecoveryObjectStoreCannotProposeItsOwnUnsafeExpectation(t *testing.T) {
	proposed := recoveryObjectStoreFixture(t)
	proposed["spec"].(object)["retentionPolicy"] = "1d"
	if _, err := matchRecoveryObjectStoreAdmission(proposed, proposed); err == nil {
		t.Fatal("caller-selected retention accepted")
	}
}
