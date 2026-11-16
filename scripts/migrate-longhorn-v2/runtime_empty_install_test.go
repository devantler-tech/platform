package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRuntimeInventoryDistinguishesEmptyStorageFromMissingEvidence(t *testing.T) {
	for _, fault := range []string{"", "missing items", "partial list", "empty managers"} {
		t.Run(fault, func(t *testing.T) {
			c, _ := runtimeObservationFixture(t, "overlap")
			delegate := c.run
			c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				raw, err := delegate(ctx, input, program, args...)
				if err != nil || program != "kubectl" {
					return raw, err
				}
				var o object
				if json.Unmarshal(raw, &o) != nil {
					t.Fatal("invalid fixture")
				}
				kind := text(o, "kind")
				if kind == "VolumeList" || kind == "EngineList" || kind == "ReplicaList" || fault == "empty managers" && kind == "InstanceManagerList" {
					o["items"] = []any{}
					if fault == "missing items" {
						delete(o, "items")
					}
					if fault == "partial list" {
						valueObject(o, "metadata")["continue"] = "more"
					}
				}
				return json.Marshal(o)
			}
			receipt, err := observeRuntimeInventory(t.Context(), c, strings.Repeat("a", 40))
			if fault == "" {
				if err != nil || receipt.V1Volumes != 0 || receipt.V2Volumes != 0 || receipt.InstanceManagers == 0 || receipt.DeclaredOverlap != "FIT" {
					t.Fatalf("complete empty storage was not observed: %+v %v", receipt, err)
				}
			} else if err == nil {
				t.Fatal("missing or partial inventory was accepted")
			}
		})
	}
}
