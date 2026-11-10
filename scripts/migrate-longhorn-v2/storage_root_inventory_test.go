package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

type storageRootReaderFixture struct {
	temporaryNativeReader
	calls, boots            int
	change, partial, reboot bool
	blockCalls              int
	blockError              bool
	retainedCalls           int
	retainedObserved        bool
}

func (r *storageRootReaderFixture) ListBlockLayout(context.Context) (longhorninventory.BlockLayoutReport, error) {
	r.blockCalls++
	if r.blockError {
		return longhorninventory.BlockLayoutReport{}, errors.New("private-block-canary")
	}
	return longhorninventory.BlockLayoutReport{VolumeStatuses: 2, DiscoveredVolumes: 1, EphemeralPhase: "ready", RawPhase: "waiting", Digest: strings.Repeat("a", 64)}, nil
}

func (r *storageRootReaderFixture) ListRetainedBlockLayout(context.Context) (longhorninventory.BlockLayoutReport, error) {
	r.retainedCalls++
	return longhorninventory.BlockLayoutReport{
		VolumeStatuses: 2, DiscoveredVolumes: 2, EphemeralPhase: "ready", RawPhase: "ready",
		RawExtentObserved: r.retainedObserved, Digest: strings.Repeat("a", 64),
	}, nil
}

func (r *storageRootReaderFixture) ListStorageRoot(context.Context) (longhorninventory.Listing, error) {
	r.calls++
	entries := []longhorninventory.Entry{{Name: ".", Mode: uint32(os.ModeDir), IsDir: true}, {Name: "engine-binaries", Mode: uint32(os.ModeDir), IsDir: true}, {Name: "private-parent-canary", Mode: uint32(os.ModeDir), IsDir: true}}
	if r.change && r.calls > 1 {
		entries = append(entries, longhorninventory.Entry{Name: "new-parent-data", Mode: uint32(os.ModeDir), IsDir: true})
	}
	return longhorninventory.Listing{Complete: !r.partial, Entries: entries}, nil
}
func (r *storageRootReaderFixture) BootID(context.Context) (string, error) {
	r.boots++
	if r.reboot && r.boots > 1 {
		return "12345678-1234-4234-8234-000000000099", nil
	}
	return r.boot, nil
}

func TestRootObserverBracketsNativeParentMetadataAndNeverGrantsResetClearance(t *testing.T) {
	for _, scenario := range []string{"complete", "partial", "root changed", "peer changed", "boot changed", "close failed", "layout complete", "layout read failed", "layout retained", "layout retained missing proof"} {
		t.Run(scenario, func(t *testing.T) {
			nodes := temporaryInventoryRoster()
			target, selected := "temporary-v1-1", nodes[0]
			retained := strings.HasPrefix(scenario, "layout retained")
			if retained {
				target, selected = "temporary-v2-1", nodes[2]
			}
			pools, err := configuredMigrationPools(poolConfigFixture())
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			c := client{context: "admin@prod", inventoryPools: pools, run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || !inventoryReadAllowed(program, args...) {
					t.Fatal("parent observer escaped fixed read scope")
				}
				if program == "talosctl" {
					return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": text(selected, "status", "nodeInfo", "systemUUID")}})
				}
				api, kind := "v1", "NodeList"
				items := []any{}
				switch args[3] {
				case "--raw=" + inventoryKubeNodes:
					reads++
					if scenario == "peer changed" && reads > 1 {
						nodes[2]["metadata"].(object)["uid"] = "replacement"
					}
					for _, n := range nodes {
						items = append(items, n)
					}
				case "--raw=" + inventoryStorageNodes:
					api = "longhorn.io/v1beta2"
					_, lh := temporaryInventoryFixture()
					lh["metadata"].(object)["name"] = text(selected, "metadata", "name")
					items = []any{lh}
				case "--raw=" + inventoryReplicas:
					api, kind = "longhorn.io/v1beta2", "ReplicaList"
				default:
					t.Fatal("unexpected API query")
				}
				return json.Marshal(object{"apiVersion": api, "kind": kind, "metadata": object{"resourceVersion": "1"}, "items": items})
			}}
			reader := &storageRootReaderFixture{
				temporaryNativeReader: temporaryNativeReader{boot: text(selected, "status", "nodeInfo", "bootID")},
				change:                scenario == "root changed", partial: scenario == "partial", reboot: scenario == "boot changed",
				blockError: scenario == "layout read failed", retainedObserved: scenario == "layout retained",
			}
			closed := false
			observe := observeStorageRoot
			if strings.HasPrefix(scenario, "layout ") {
				observe = observeStorageLayout
			}
			receipt, err := observe(context.Background(), c, target, func(_ context.Context, address string) (nativeReplicaReader, func() error, error) {
				projection, _, projectionErr := c.inventoryProjection(context.Background(), target)
				if projectionErr != nil || address != projection.Address {
					t.Fatal("parent observer selected another endpoint")
				}
				return reader, func() error {
					closed = true
					if scenario == "close failed" {
						return errors.New("private-close-canary")
					}
					return nil
				}, nil
			})
			if !closed || (err == nil) != (scenario == "complete" || scenario == "layout complete" || scenario == "layout retained") {
				t.Fatal("incomplete or changed parent metadata was accepted:", err)
			}
			if retained != (reader.retainedCalls == 1) || (retained && reader.blockCalls != 0) {
				t.Fatal("retained extent observation escaped its fixed worker")
			}
			encoded, _ := json.Marshal(receipt)
			if strings.Contains(string(encoded), "private-parent-canary") || strings.Contains(string(encoded), "private-close-canary") {
				t.Fatal("private names reached the receipt")
			}
			if err != nil {
				if receipt.Schema != 0 {
					t.Fatal("failed observation retained a success receipt")
				}
				return
			}
			if scenario == "layout complete" {
				if reader.blockCalls != 1 || receipt.Schema != 2 || receipt.BlockLayout == nil || receipt.BlockLayout.RawPhase != "waiting" {
					t.Fatal("layout mode omitted native block facts")
				}
			} else if retained {
				if receipt.Schema != 2 || receipt.BlockLayout == nil || !receipt.BlockLayout.RawExtentObserved {
					t.Fatal("retained layout omitted its native extent proof")
				}
			} else if reader.blockCalls != 0 || receipt.Schema != 1 || receipt.BlockLayout != nil {
				t.Fatal("legacy root mode expanded its read scope")
			}
			if !receipt.ObservationOnly || receipt.ResetClearance || receipt.Report.ReplicaEntryPresent || !receipt.Report.EngineBinariesPresent || receipt.Report.OtherEntries != 1 {
				t.Fatal("parent metadata lost its scope or claimed reset clearance")
			}
		})
	}
}

func TestParentInventoryRequiresItsOwnReadOnlyConfirmation(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"], env["LONGHORN_V2_CONFIRM"] = "inventory-storage-root", "temporary-v1-1", "inventory-longhorn-storage-root-temporary-v1-1"
	getenv := func(key string) string { return env[key] }
	if err := authorizeStorageRoot(getenv, "temporary-v1-1"); err != nil {
		t.Fatal("confirmed parent observation was rejected:", err)
	}
	if authorize(getenv, "temporary-v1-1") == nil || authorizeInventory(getenv, "temporary-v1-1") == nil || authorizeRefresh(getenv) == nil {
		t.Fatal("parent observation authorized another operation")
	}
	env["LONGHORN_V2_CONFIRM"] = "inventory-longhorn-replicas-temporary-v1-1"
	if authorizeStorageRoot(getenv, "temporary-v1-1") == nil {
		t.Fatal("replica confirmation admitted a different read scope")
	}
}

func TestBlockLayoutObservationCannotUseOtherConfirmationsOrTargetPermanentWorkers(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"], env["LONGHORN_V2_CONFIRM"] = "inventory-storage-layout", "temporary-v1-1", "inventory-longhorn-storage-layout-temporary-v1-1"
	getenv := func(key string) string { return env[key] }
	if authorizeStorageLayout(getenv, "temporary-v1-1") != nil {
		t.Fatal("fixed layout observation refused")
	}
	if authorizeStorageRoot(getenv, "temporary-v1-1") == nil || authorizeInventory(getenv, "temporary-v1-1") == nil || authorizeRefresh(getenv) == nil || authorize(getenv, "temporary-v1-1") == nil {
		t.Fatal("layout confirmation granted another operation")
	}
	env["LONGHORN_V2_NODE"], env["LONGHORN_V2_CONFIRM"] = "prod-worker-1", "inventory-longhorn-storage-layout-prod-worker-1"
	if authorizeStorageLayout(getenv, "prod-worker-1") == nil {
		t.Fatal("layout observation escaped its temporary pool")
	}
}
