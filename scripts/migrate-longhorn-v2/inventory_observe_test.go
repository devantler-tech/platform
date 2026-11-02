package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

type replicaReaderFixture struct {
	listCalls, bootCalls      int
	changeListing, changeBoot bool
}

func (r *replicaReaderFixture) BootID(context.Context) (string, error) {
	r.bootCalls++
	if r.changeBoot && r.bootCalls > 1 {
		return "12345678-1234-4234-8234-123456789abd", nil
	}
	return "12345678-1234-4234-8234-123456789abc", nil
}

func (r *replicaReaderFixture) ListReplicas(context.Context) (longhorninventory.Listing, error) {
	r.listCalls++
	name := "unreported-old-data"
	if r.changeListing && r.listCalls > 1 {
		name = "new-directory"
	}
	return longhorninventory.Listing{Complete: true, Entries: []longhorninventory.Entry{
		{Name: ".", Mode: uint32(os.ModeDir), IsDir: true}, {Name: name, Mode: uint32(os.ModeDir), IsDir: true},
	}}, nil
}

func TestInventoryBracketsNodeAndDiskObservationsWithoutPublishingPrivateNames(t *testing.T) {
	for _, scenario := range []string{"stable", "boot changed", "API changed", "disk changed", "close failed", "hardware changed", "client failed"} {
		t.Run(scenario, func(t *testing.T) {
			node, storage, replicas := inventoryFixture()
			nodeReads, hardwareReads := 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil {
					t.Fatal("inventory sent a mutation body")
				}
				if program == "talosctl" {
					hardwareReads++
					if strings.Join(args, " ") != "--nodes=10.0.99.4 get systeminformation systeminformation --output=json" {
						t.Fatal("inventory attempted a host write")
					}
					uuid := "machine-uuid"
					if scenario == "hardware changed" && hardwareReads > 1 {
						uuid = "replacement"
					}
					return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": uuid}})
				}
				if program != "kubectl" || len(args) < 6 || args[2] != "get" || args[4] != "--output=json" || args[5] != "--chunk-size=0" {
					t.Fatal("inventory escaped its read-only request scope")
				}
				var items []any
				switch args[3] {
				case "nodes":
					nodeReads++
					node["metadata"].(object)["resourceVersion"] = "heartbeat-" + string(rune('0'+nodeReads))
					if scenario == "API changed" && nodeReads > 1 {
						node["metadata"].(object)["uid"] = "replacement"
					}
					items = []any{node}
				case "nodes.longhorn.io":
					items = []any{storage}
				case "replicas.longhorn.io":
					items = []any{replicas[0]}
				default:
					t.Fatal("unreviewed resource requested")
				}
				return json.Marshal(object{"metadata": object{"resourceVersion": "123"}, "items": items})
			}}
			reader := &replicaReaderFixture{changeListing: scenario == "disk changed", changeBoot: scenario == "boot changed"}
			closed := false
			receipt, err := observeReplicaInventory(context.Background(), c, "prod-worker-1", func(_ context.Context, address string) (nativeReplicaReader, func() error, error) {
				if address != "10.0.99.4" {
					t.Fatal("factory was routed elsewhere")
				}
				if scenario == "client failed" {
					return nil, nil, errors.New("private-provider-detail")
				}
				return reader, func() error {
					closed = true
					if scenario == "close failed" {
						return errors.New("close failed")
					}
					return nil
				}, nil
			})
			if closed != (scenario != "client failed") || (err == nil) != (scenario == "stable") {
				t.Fatalf("identity/observation gate failed: %v", err)
			}
			if err != nil {
				expected := map[string]string{"boot changed": "boot-after", "API changed": "api-consistency", "disk changed": "listing-consistency", "close failed": "close", "hardware changed": "host-after", "client failed": "client"}
				message := inventoryFailureMessage(err)
				if !strings.Contains(message, "stage="+expected[scenario]) || strings.Contains(message, "private-provider-detail") {
					t.Fatal("hold lost its safe failure stage")
				}
				if receipt.Schema != 0 {
					t.Fatal("failed observation retained a success receipt")
				}
				return
			}
			encoded, _ := json.Marshal(receipt)
			if !receipt.ObservationOnly || receipt.Report.Unmatched != 1 || receipt.Report.Missing != 1 || receipt.Report.EmptyAndAccounted ||
				strings.Contains(string(encoded), "unreported-old-data") || strings.Contains(string(encoded), "missing-volume") || strings.Contains(string(encoded), "10.0.99.4") || strings.Contains(string(encoded), "node-uid") {
				t.Fatal("receipt omitted unresolved data or exposed private identity")
			}
		})
	}
}

func TestInventoryFailureMessageNeverPrintsAnUnknownProviderError(t *testing.T) {
	message := inventoryFailureMessage(errors.New("private-provider-detail"))
	if strings.Contains(message, "private-provider-detail") || !strings.Contains(message, "stage=unknown") {
		t.Fatal("public hold exposed an unclassified provider error")
	}
}

type delayedInventoryCancellation struct {
	context.Context
	expires time.Time
}

func (c delayedInventoryCancellation) Deadline() (time.Time, bool) { return c.expires, true }

func TestInventoryDoesNotStartAfterAnUnsignalledDeadline(t *testing.T) {
	ctx := delayedInventoryCancellation{Context: context.Background(), expires: time.Now().Add(-time.Second)}
	c := client{context: "admin@prod", run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("expired inventory started an API request")
		return nil, nil
	}}
	_, err := observeReplicaInventory(ctx, c, "prod-worker-1", func(context.Context, string) (nativeReplicaReader, func() error, error) {
		t.Fatal("expired inventory opened a native client")
		return nil, nil, nil
	})
	if err == nil {
		t.Fatal("expired inventory produced a success receipt")
	}
}
