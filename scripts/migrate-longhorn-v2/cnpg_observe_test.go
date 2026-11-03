package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestCNPGObservationReadsSelectedDatabaseWithoutProductionWrites exercises the command seam.
func TestCNPGObservationReadsSelectedDatabaseWithoutProductionWrites(t *testing.T) {
	fixture := cnpgObservationFixture(t)
	source := strings.Repeat("a", 40)
	physicalReads, sourceReads := 0, 0
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if len(input) != 0 {
			t.Fatal("read-only observation supplied a mutation body")
		}
		if program == "git" {
			sourceReads++
			return []byte(source + "\trefs/heads/main\n"), nil
		}
		if program != "kubectl" || len(args) < 3 {
			t.Fatal("unexpected executable")
		}
		if args[2] == "exec" {
			physicalReads++
			if args[len(args)-1] != "--command="+postgresStateSQL {
				t.Fatal("arbitrary SQL admitted")
			}
			return json.Marshal(fixture.States[args[3]])
		}
		if args[2] != "get" {
			t.Fatal("production write attempted")
		}
		switch args[3] {
		case "cluster.postgresql.cnpg.io":
			return json.Marshal(fixture.Cluster)
		case "pods":
			return json.Marshal(object{"metadata": object{"resourceVersion": "10"}, "items": fixture.Pods})
		case "pod":
			for _, pod := range fixture.Pods {
				if text(pod, "metadata", "name") == args[4] {
					return json.Marshal(pod)
				}
			}
		}
		return nil, errors.New("unexpected read")
	}}
	observed, err := observeCNPGMigration(context.Background(), c, "umami-db", source)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.States) != 3 || physicalReads != 6 || sourceReads != 2 {
		t.Fatalf("incomplete native bracket: states=%d SQL=%d source=%d", len(observed.States), physicalReads, sourceReads)
	}
}

// TestCNPGObservationNeverAcceptsAnUnboundNativeResource tests response scope binding.
func TestCNPGObservationNeverAcceptsAnUnboundNativeResource(t *testing.T) {
	for _, changedField := range []string{"name", "namespace", "generation"} {
		t.Run(changedField, func(t *testing.T) {
			fixture := cnpgObservationFixture(t)
			metadata := fixture.Cluster["metadata"].(object)
			metadata["generation"] = float64(1)
			if changedField == "generation" {
				delete(metadata, changedField)
			} else {
				metadata[changedField] = "other"
			}
			c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
				if len(args) > 3 && args[2] == "get" && args[3] == "cluster.postgresql.cnpg.io" {
					return json.Marshal(fixture.Cluster)
				}
				return json.Marshal(object{"items": fixture.Pods})
			}}
			if _, err := c.readCNPGObservation(context.Background(), cnpgSource{"umami", "umami-db"}); err == nil {
				t.Fatal("unbound database response accepted")
			}
		})
	}
}

// TestCNPGObservationRejectsUnknownTargetsBeforeExecuting prevents input-selected commands.
func TestCNPGObservationRejectsUnknownTargetsBeforeExecuting(t *testing.T) {
	c := client{run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("unknown target reached executable")
		return nil, nil
	}}
	if _, err := observeCNPGMigration(context.Background(), c, "other-database", strings.Repeat("a", 40)); err == nil {
		t.Fatal("unknown database admitted")
	}
}
