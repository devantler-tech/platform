package main

import "testing"

func TestOnlyTheSingleReviewedPolicySuppliesTheExpectedSpec(t *testing.T) {
	valid := []byte("apiVersion: policies.kyverno.io/v1\nkind: ImageValidatingPolicy\nmetadata:\n  name: verify-app-images\nspec:\n  failurePolicy: Fail\n")
	if _, err := reviewedSpec(valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{[]byte("kind: Secret\n"), append(append([]byte{}, valid...), []byte("---\nkind: ImageValidatingPolicy\n")...)} {
		if _, err := reviewedSpec(raw); err == nil {
			t.Fatal("foreign or multiple source documents accepted")
		}
	}
}
