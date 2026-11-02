package main

import (
	"encoding/json"
	"testing"
)

const desiredPolicySpec = `{"failurePolicy":"Fail","evaluation":{"admission":{"enabled":true}}}`

func policyFixture(t *testing.T, change func(object)) []byte {
	t.Helper()
	v := object{"apiVersion": "policies.kyverno.io/v1", "kind": "ImageValidatingPolicy", "metadata": object{"name": "verify-app-images", "uid": "policy-1", "resourceVersion": "23", "generation": 3}, "status": object{"conditionStatus": object{"ready": true, "conditions": []any{object{"type": "WebhookConfigured", "status": "True"}, object{"type": "RBACPermissionsGranted", "status": "True"}}}}}
	var spec any
	if err := json.Unmarshal([]byte(desiredPolicySpec), &spec); err != nil {
		t.Fatal(err)
	}
	v["spec"] = spec
	if change != nil {
		change(v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPolicyBindsNativeReadyStatusAndReviewedSpec(t *testing.T) {
	if _, err := qualifyPolicy(policyFixture(t, nil), []byte(desiredPolicySpec)); err != nil {
		t.Fatalf("ready matching native policy rejected: %v", err)
	}
	for name, change := range map[string]func(object){
		"unready": func(v object) { v["status"].(object)["conditionStatus"].(object)["ready"] = false },
		"webhook unconfigured": func(v object) {
			v["status"].(object)["conditionStatus"].(object)["conditions"].([]any)[0].(object)["status"] = "False"
		},
		"permissions unknown": func(v object) {
			v["status"].(object)["conditionStatus"].(object)["conditions"].([]any)[1].(object)["status"] = "Unknown"
		},
		"spec changed":   func(v object) { v["spec"].(map[string]any)["failurePolicy"] = "Ignore" },
		"foreign policy": func(v object) { v["metadata"].(object)["name"] = "other" },
		"missing UID":    func(v object) { v["metadata"].(object)["uid"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := qualifyPolicy(policyFixture(t, change), []byte(desiredPolicySpec)); err == nil {
				t.Fatal("unqualified policy accepted")
			}
		})
	}
}

func TestNativeListCannotBePartialOrAmbiguous(t *testing.T) {
	valid := []byte(`{"apiVersion":"policies.kyverno.io/v1","kind":"PolicyExceptionList","metadata":{"resourceVersion":"15"},"items":[]}`)
	if items, err := completeList(valid, "policies.kyverno.io/v1", "PolicyExceptionList"); err != nil || len(items) != 0 {
		t.Fatalf("complete empty native inventory rejected: %v", err)
	}
	for _, raw := range []string{
		`{"metadata":{"resourceVersion":"15"},"items":[]}`,
		`{"apiVersion":"policies.kyverno.io/v1","kind":"PolicyExceptionList","metadata":{"resourceVersion":"15","continue":"later"},"items":[]}`,
		`{"apiVersion":"policies.kyverno.io/v1","kind":"PolicyExceptionList","metadata":{"resourceVersion":"15","remainingItemCount":1},"items":[]}`,
		`{"apiVersion":"policies.kyverno.io/v1","kind":"PolicyExceptionList","metadata":{"resourceVersion":"15"},"items":[{}],"items":[]}`,
	} {
		if _, err := completeList([]byte(raw), "policies.kyverno.io/v1", "PolicyExceptionList"); err == nil {
			t.Fatal("partial or ambiguous inventory accepted")
		}
	}
}
