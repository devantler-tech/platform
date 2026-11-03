package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const hostMemoryFixture = `MemTotal:       16384000 kB
MemAvailable:    6000000 kB
HugePages_Total:     317
HugePages_Free:      317
HugePages_Rsvd:        0
HugePages_Surp:        0
Hugepagesize:       2048 kB
PrivateIgnored: private-memory-canary
`

func TestHostMemoryCountersPreserveAnObservedAllocationShortfall(t *testing.T) {
	report, err := parseHostMemory([]byte("317\n"), []byte(hostMemoryFixture))
	if err != nil || report.PersistentPages != 317 || report.TotalPages != 317 || report.AvailableKiB != 6000000 || report.PageSizeKiB != 2048 {
		t.Fatal("actual allocation shortfall was lost:", report, err)
	}
	b, _ := json.Marshal(report)
	if strings.Contains(string(b), "private-memory-canary") {
		t.Fatal("native memory text escaped the counter projection")
	}
}

func TestHostMemoryObserverBindsCountersToNativeHostAndBoot(t *testing.T) {
	for _, scenario := range []string{"stable shortfall", "unassigned replacement", "missing registered disk", "API changed", "storage changed", "boot changed", "close failed", "memory read failed", "allocation changed"} {
		t.Run(scenario, func(t *testing.T) {
			node, storage, replicas := inventoryFixture()
			if scenario == "missing registered disk" {
				storage["spec"].(object)["disks"] = object{}
				storage["status"].(object)["diskStatus"] = object{}
			}
			if scenario == "unassigned replacement" {
				replicas[0]["spec"].(object)["nodeID"] = ""
				if _, err := projectInventory("prod-worker-1", node, storage, replicas); err == nil {
					t.Fatal("ordinary replica accounting accepted unresolved placement")
				}
			}
			nodeReads, storageReads, pageReads := 0, 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || !hostMemoryReadAllowed(program, args...) {
					t.Fatal("host observer escaped fixed read-only scope")
				}
				if program == "talosctl" {
					if args[1] == "get" {
						return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": "machine-uuid"}})
					}
					if args[2] == "/proc/meminfo" {
						if scenario == "memory read failed" {
							return nil, errors.New("private-provider-canary")
						}
						return []byte(hostMemoryFixture), nil
					}
					pageReads++
					if scenario == "allocation changed" && pageReads > 1 {
						return []byte("318\n"), nil
					}
					return []byte("317\n"), nil
				}
				api, kind, items := "v1", "NodeList", []any{node}
				switch args[3] {
				case "--raw=" + inventoryKubeNodes:
					nodeReads++
					if scenario == "API changed" && nodeReads > 1 {
						node["metadata"].(object)["uid"] = "replacement"
					}
				case "--raw=" + inventoryStorageNodes:
					storageReads++
					if scenario == "storage changed" && storageReads > 1 {
						storage["metadata"].(object)["uid"] = "replacement"
					}
					api, items = "longhorn.io/v1beta2", []any{storage}
				case "--raw=" + inventoryReplicas:
					api, kind, items = "longhorn.io/v1beta2", "ReplicaList", []any{replicas[0]}
				default:
					t.Fatal("unexpected query")
				}
				return json.Marshal(object{"apiVersion": api, "kind": kind, "metadata": object{"resourceVersion": "1"}, "items": items})
			}}
			if scenario == "unassigned replacement" {
				if _, _, err := c.inventoryProjection(t.Context(), "prod-worker-1"); err == nil {
					t.Fatal("replica observer lost its complete ownership boundary")
				}
			}
			reader := &replicaReaderFixture{changeBoot: scenario == "boot changed"}
			closed := false
			receipt, err := observeHostMemory(context.Background(), c, "prod-worker-1", func(_ context.Context, address string) (nativeReplicaReader, func() error, error) {
				if address != "10.0.99.4" {
					t.Fatal("host observer selected another endpoint")
				}
				return reader, func() error {
					closed = true
					if scenario == "close failed" {
						return errors.New("private-close-canary")
					}
					return nil
				}, nil
			})
			wantObservation := scenario == "stable shortfall" || scenario == "unassigned replacement"
			wantClosed := scenario != "missing registered disk"
			if closed != wantClosed || reader.listCalls != 0 || (err == nil) != wantObservation {
				t.Fatal("host counter observation lost its boundary:", err)
			}
			encoded, _ := json.Marshal(receipt)
			if strings.Contains(string(encoded), "canary") || strings.Contains(string(encoded), "10.0.99.4") || strings.Contains(string(encoded), "machine-uuid") {
				t.Fatal("counter receipt exposed private source text or identity")
			}
			if err != nil {
				if receipt.Schema != 0 || strings.Contains(inventoryFailureMessage(err), "canary") {
					t.Fatal("failed observer retained success or leaked provider text")
				}
				return
			}
			if receipt.Schema != 1 || !receipt.ObservationOnly || receipt.ResetClearance || receipt.Report.PersistentPages != 317 {
				t.Fatal("counter observation claimed migration clearance or lost actual values")
			}
		})
	}
}

func TestHostMemoryCountersRejectMissingMalformedOrAmbiguousObservations(t *testing.T) {
	for _, test := range []struct{ name, pages, memory string }{
		{"missing allocation", "", hostMemoryFixture},
		{"partial allocation", "31", hostMemoryFixture},
		{"negative", "-1\n", hostMemoryFixture},
		{"allocation overflow", "999999999999999999999999\n", hostMemoryFixture},
		{"missing memory", "317\n", "MemTotal: 16384000 kB\n"},
		{"duplicate", "317\n", hostMemoryFixture + "HugePages_Total: 317\n"},
		{"wrong units", "317\n", strings.Replace(hostMemoryFixture, "2048 kB", "2048 MB", 1)},
		{"impossible available", "317\n", strings.Replace(hostMemoryFixture, "6000000", "99999999", 1)},
		{"impossible free", "317\n", strings.Replace(hostMemoryFixture, "HugePages_Free:      317", "HugePages_Free:      999", 1)},
		{"missing final newline", "317\n", strings.TrimSuffix(hostMemoryFixture, "\n")},
		{"oversized", "317\n", hostMemoryFixture + strings.Repeat("x", 16384)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseHostMemory([]byte(test.pages), []byte(test.memory)); err == nil {
				t.Fatal("invalid native counters were accepted")
			}
		})
	}
}

func TestHostMemoryObservationHasDistinctAuthorizationAndFixedReadScope(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"], env["LONGHORN_V2_CONFIRM"] = "inspect-host-memory", "prod-worker-1", "inspect-longhorn-v2-host-memory-prod-worker-1"
	getenv := func(key string) string { return env[key] }
	if authorizeHostMemory(getenv, "prod-worker-1") != nil {
		t.Fatal("the confirmed fixed host observation was refused")
	}
	if authorize(getenv, "prod-worker-1") == nil || authorizeRefresh(getenv) == nil || authorizeInventory(getenv, "prod-worker-1") == nil {
		t.Fatal("a read-only confirmation authorized another operation")
	}
	for _, target := range []string{"temporary-v1-1", "prod-worker-4", "--help"} {
		env["LONGHORN_V2_NODE"] = target
		env["LONGHORN_V2_CONFIRM"] = "inspect-longhorn-v2-host-memory-" + target
		if authorizeHostMemory(getenv, target) == nil {
			t.Fatal("observation escaped permanent worker selection")
		}
	}
	for _, args := range [][]string{
		{"--nodes=10.0.99.4", "service", "kubelet", "restart"},
		{"--nodes=10.0.99.4", "read", "/etc/private-canary"},
		{"--nodes=192.0.2.1", "read", "/proc/meminfo"},
		{"--nodes=10.0.99.4", "read", "/proc/meminfo", "--nodes=192.0.2.1"},
	} {
		if hostMemoryReadAllowed("talosctl", args...) {
			t.Fatal("host command wrapper authorized a write, path, or peer outside its fixed scope")
		}
	}
}
