package main

import (
	"context"
	"strings"
	"testing"
)

// TestRecoveryPolicyIsProvenBeforeAnyDatabasePodExists checks the empty-namespace gate.
func TestRecoveryPolicyIsProvenBeforeAnyDatabasePodExists(t *testing.T) {
	c, input, _ := recoveryObservationFixture(t, nil)
	input.Pod = nil
	bound, err := c.observeRecoveryPolicyPreparation(context.Background(), input)
	if err != nil || bound.Revision != 817 || bound.PolicyUID != text(input.Policy, "metadata", "uid") {
		t.Fatalf("complete pre-creation policy refused: %v", err)
	}
}

// TestRecoveryPolicyPreparationRefusesChangedNativeOwnership preserves the original binding.
func TestRecoveryPolicyPreparationRefusesChangedNativeOwnership(t *testing.T) {
	for _, mutation := range []string{"agent restart", "policy replaced", "node reboot"} {
		t.Run(mutation, func(t *testing.T) {
			c, input, _ := recoveryObservationFixture(t, func(key string, count int, o object) {
				if count != 2 {
					return
				}
				if mutation == "agent restart" && strings.HasPrefix(key, "pod/cilium-") {
					value(o, "status", "containerStatuses").([]any)[0].(object)["restartCount"] = float64(1)
				}
				if mutation == "policy replaced" && strings.HasPrefix(key, "ciliumnetworkpolicies.") {
					o["metadata"].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
				}
				if mutation == "node reboot" && strings.HasPrefix(key, "node/") {
					value(o, "status", "nodeInfo").(object)["bootID"] = "99999999-9999-4999-8999-999999999999"
				}
			})
			input.Pod = nil
			if bound, err := c.observeRecoveryPolicyPreparation(context.Background(), input); err == nil || bound != (recoveryPolicyPreparation{}) {
				t.Fatal("changed pre-creation policy environment accepted")
			}
		})
	}
}
