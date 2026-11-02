package main

import (
	"context"
	"errors"
	"testing"
)

func TestProviderBaselineGuardsTheOperationAndPinsTheObservation(t *testing.T) {
	for _, scenario := range []string{"disabled", "missing-capability", "unknown-before", "active-before", "changed-after", "unknown-after", "operation-failed"} {
		t.Run(scenario, func(t *testing.T) {
			_, b := providerFixture()
			reads, operations := 0, 0
			reader := func(_ context.Context, actual binding, wantProxy bool) (providerIdentity, error) {
				reads++
				if actual != b || wantProxy {
					t.Fatal("operation was not bound to the disabled provider profile")
				}
				if (scenario == "unknown-before" && reads == 1) || (scenario == "unknown-after" && reads == 2) {
					return providerIdentity{}, errors.New("provider-document")
				}
				if scenario == "active-before" && reads == 1 {
					return providerIdentity{}, errProviderMode
				}
				id := int64(7)
				if scenario == "changed-after" && reads == 2 {
					id = 8
				}
				return providerIdentity{ID: id, NetworkID: 9}, nil
			}
			if scenario == "missing-capability" {
				reader = nil
			}
			err := withProviderBaseline(context.Background(), b, reader, func() error {
				operations++
				if scenario == "operation-failed" {
					return errors.New("source-admission-policy")
				}
				return nil
			})
			wantOperations := 1
			if scenario == "missing-capability" || scenario == "unknown-before" || scenario == "active-before" {
				wantOperations = 0
			}
			if operations != wantOperations || (err == nil) != (scenario == "disabled") {
				t.Fatalf("provider disagreement or missing proof permitted an operation or success: operations=%d error=%v", operations, err)
			}
			if scenario == "disabled" && reads != 2 {
				t.Fatal("operation lacked provider readback on both sides")
			}
			if scenario == "operation-failed" && (err == nil || err.Error() != "source-admission-policy") {
				t.Fatal("a successful provider observation hid the operation failure")
			}
		})
	}
}
