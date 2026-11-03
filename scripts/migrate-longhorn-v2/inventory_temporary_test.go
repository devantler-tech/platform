package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/longhorninventory"
	"sigs.k8s.io/yaml"
)

func temporaryInventoryFixture() (object, object) {
	n := migrationNodeFixture("v1", "0123456789abcdef")
	n["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-123456789abc"
	lh := resource(text(n, "metadata", "name"))
	lh["spec"] = object{"allowScheduling": false, "evictionRequested": true, "disks": object{}}
	lh["status"] = object{"diskStatus": object{}}
	return n, lh
}

func TestInventoryAcceptsOnlyFencedTemporaryNodeIdentity(t *testing.T) {
	n, lh := temporaryInventoryFixture()
	projection, err := projectInventory(text(n, "metadata", "name"), n, lh, nil)
	if err != nil || projection.Registered || len(projection.Replicas) != 0 || projection.Digest == "" {
		t.Fatalf("fenced temporary worker cannot be inspected: %v", err)
	}
	for name, change := range map[string]func(object, object){
		"ordinary node": func(n, lh object) {
			n["metadata"].(object)["name"] = "autoscale-cx43-0123456789abcdef"
			lh["metadata"].(object)["name"] = text(n, "metadata", "name")
		},
		"wrong role": func(n, _ object) { n["metadata"].(object)["labels"].(object)[migrationPoolLabel] = "v2" },
		"wrong hardware": func(n, _ object) {
			n["metadata"].(object)["labels"].(object)["node.kubernetes.io/instance-type"] = "cx43"
		},
		"unfenced node":     func(_, lh object) { lh["spec"].(object)["allowScheduling"] = true },
		"not evacuating":    func(_, lh object) { lh["spec"].(object)["evictionRequested"] = false },
		"enabled storage":   func(_, lh object) { lh["metadata"].(object)["labels"] = object{migrationEnabledLabel: "true"} },
		"registered disk":   func(_, lh object) { lh["spec"].(object)["disks"] = object{"unexpected": object{}} },
		"stale disk status": func(_, lh object) { lh["status"].(object)["diskStatus"] = object{"unexpected": object{}} },
	} {
		t.Run(name, func(t *testing.T) {
			n, lh := temporaryInventoryFixture()
			change(n, lh)
			if _, err := projectInventory(text(n, "metadata", "name"), n, lh, nil); err == nil {
				t.Fatal("unproven temporary storage identity accepted")
			}
		})
	}
}

func TestInventoryTemporarySelectionsNeverAuthorizeWorkerReset(t *testing.T) {
	for _, selector := range []string{"temporary-v1-1", "temporary-v1-2", "temporary-v2-1"} {
		env := dispatchFixture()
		env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"] = "inventory-replicas", selector
		env["LONGHORN_V2_CONFIRM"] = "inventory-longhorn-replicas-" + selector
		getenv := func(k string) string { return env[k] }
		if err := authorizeInventory(getenv, selector); err != nil {
			t.Fatalf("read-only selection %s rejected: %v", selector, err)
		}
		if authorize(getenv, selector) == nil || authorizeRefresh(getenv) == nil || authorizeBenchmark(getenv, "v1") == nil {
			t.Fatal("temporary inventory widened mutation authorization")
		}
		for _, bad := range []string{"temporary-v1-0", "temporary-v1-3", "temporary-v2-2", "autoscale-longhorn-v1-0123456789abcdef", "10.0.99.4"} {
			env["LONGHORN_V2_NODE"], env["LONGHORN_V2_CONFIRM"] = bad, "inventory-longhorn-replicas-"+bad
			if authorizeInventory(getenv, bad) == nil {
				t.Fatalf("unreviewed inventory target %q accepted", bad)
			}
		}
	}
}

func temporaryInventoryRoster() []object {
	var nodes []object
	for i, role := range []string{"v1", "v1", "v2"} {
		n := migrationNodeFixture(role, fmt.Sprintf("%016x", i+1))
		n["metadata"].(object)["uid"] = fmt.Sprintf("node-%d", i)
		n["spec"].(object)["providerID"] = fmt.Sprintf("hcloud://%d", 100+i)
		n["status"].(object)["nodeInfo"].(object)["systemUUID"] = fmt.Sprintf("hardware-%d", i)
		n["status"].(object)["nodeInfo"].(object)["bootID"] = fmt.Sprintf("12345678-1234-4234-8234-%012x", i+1)
		nodes = append(nodes, n)
	}
	return nodes
}

func TestTemporaryInventorySelectionBindsTheCompleteReviewedRoster(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	nodes := temporaryInventoryRoster()
	slices.Reverse(nodes)
	first, digest, err := resolveTemporaryInventoryTarget("temporary-v1-1", nodes, pools)
	if err != nil || first != "autoscale-longhorn-v1-0000000000000001" || len(digest) != 64 {
		t.Fatalf("stable sorted target selection failed: %s %v", first, err)
	}
	second, _, err := resolveTemporaryInventoryTarget("temporary-v1-2", nodes, pools)
	if err != nil || second == first {
		t.Fatal("second V1 selection did not bind a distinct target")
	}
	_, _, err = resolveTemporaryInventoryTarget("temporary-v2-1", nodes, pools)
	if err != nil {
		t.Fatal(err)
	}
	nodes[0]["metadata"].(object)["uid"] = "replacement-other-pool-member"
	_, changed, err := resolveTemporaryInventoryTarget("temporary-v1-1", nodes, pools)
	if err != nil || changed == digest {
		t.Fatal("an unselected pool member changed without changing the observation commitment")
	}
	for name, change := range map[string]func([]object) []object{
		"missing node":  func(n []object) []object { return n[:2] },
		"extra node":    func(n []object) []object { return append(n, migrationNodeFixture("v1", "0000000000000004")) },
		"duplicate UID": func(n []object) []object { n[1]["metadata"].(object)["uid"] = text(n[0], "metadata", "uid"); return n },
		"duplicate provider": func(n []object) []object {
			n[1]["spec"].(object)["providerID"] = text(n[0], "spec", "providerID")
			return n
		},
		"duplicate hardware": func(n []object) []object {
			n[1]["status"].(object)["nodeInfo"].(object)["systemUUID"] = text(n[0], "status", "nodeInfo", "systemUUID")
			return n
		},
		"wrong pool label": func(n []object) []object {
			n[1]["metadata"].(object)["labels"].(object)[migrationPoolLabel] = "v2"
			return n
		},
		"malformed pool name": func(n []object) []object {
			n[1]["metadata"].(object)["name"] = "autoscale-longhorn-v1-invalid"
			return n
		},
		"unready member": func(n []object) []object { n[1]["status"].(object)["conditions"] = []any{}; return n },
		"missing boot":   func(n []object) []object { delete(n[1]["status"].(object)["nodeInfo"].(object), "bootID"); return n },
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resolveTemporaryInventoryTarget("temporary-v1-1", change(temporaryInventoryRoster()), pools); err == nil {
				t.Fatal("incomplete or ambiguous pool identity accepted")
			}
		})
	}
}

type temporaryNativeReader struct {
	boot      string
	unmatched bool
}

func (r temporaryNativeReader) BootID(context.Context) (string, error) { return r.boot, nil }
func (r temporaryNativeReader) ListReplicas(context.Context) (longhorninventory.Listing, error) {
	entries := []longhorninventory.Entry{{Name: ".", Mode: uint32(os.ModeDir), IsDir: true}}
	if r.unmatched {
		entries = append(entries, longhorninventory.Entry{Name: "unexpected-private-data", Mode: uint32(os.ModeDir), IsDir: true})
	}
	return longhorninventory.Listing{Complete: true, Entries: entries}, nil
}

func TestTemporaryInventoryObservesDiskAndRejectsRosterChangesWithoutWrites(t *testing.T) {
	for _, scenario := range []string{"empty", "unexpected data", "other member replaced", "missing pool config", "registered replica"} {
		t.Run(scenario, func(t *testing.T) {
			nodes := temporaryInventoryRoster()
			pools, err := configuredMigrationPools(poolConfigFixture())
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			c := client{context: "admin@prod", inventoryPools: pools, run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || !inventoryReadAllowed(program, args...) {
					t.Fatal("temporary inventory escaped the fixed read-only command surface")
				}
				if program == "talosctl" {
					return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": text(nodes[0], "status", "nodeInfo", "systemUUID")}})
				}
				api, kind := "v1", "NodeList"
				items := []any{}
				switch args[3] {
				case "--raw=" + inventoryKubeNodes:
					reads++
					if scenario == "other member replaced" && reads == 2 {
						nodes[2]["metadata"].(object)["uid"] = "replacement"
					}
					for _, n := range nodes {
						items = append(items, n)
					}
				case "--raw=" + inventoryStorageNodes:
					api = "longhorn.io/v1beta2"
					_, lh := temporaryInventoryFixture()
					lh["metadata"].(object)["name"] = text(nodes[0], "metadata", "name")
					items = []any{lh}
				case "--raw=" + inventoryReplicas:
					api, kind = "longhorn.io/v1beta2", "ReplicaList"
					if scenario == "registered replica" {
						r := resource("unexpected-replica")
						r["spec"] = object{"nodeID": text(nodes[0], "metadata", "name")}
						items = []any{r}
					}
				default:
					t.Fatal("unreviewed API request")
				}
				return json.Marshal(object{"apiVersion": api, "kind": kind, "metadata": object{"resourceVersion": "123"}, "items": items})
			}}
			if scenario == "missing pool config" {
				c.inventoryPools = nil
			}
			receipt, err := observeReplicaInventory(context.Background(), c, "temporary-v1-1", func(_ context.Context, address string) (nativeReplicaReader, func() error, error) {
				addresses := value(nodes[0], "status", "addresses").([]any)
				if address != text(addresses[0].(object), "address") {
					t.Fatal("wrong temporary endpoint")
				}
				return temporaryNativeReader{boot: text(nodes[0], "status", "nodeInfo", "bootID"), unmatched: scenario == "unexpected data"}, func() error { return nil }, nil
			})
			if (err == nil) != (scenario == "empty") {
				t.Fatalf("unexpected %s result: %v", scenario, err)
			}
			if scenario == "empty" {
				if !receipt.Report.EmptyAndAccounted || receipt.Registered {
					t.Fatal("empty temporary disk not accounted")
				}
				encoded, _ := json.Marshal(receipt)
				if strings.Contains(string(encoded), "autoscale-") || strings.Contains(string(encoded), "10.0.") {
					t.Fatal("private identity entered public receipt")
				}
			}
		})
	}
}

func TestTemporaryInventoryNativeWorkflowGuardPreservesResetBoundary(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/prepare-longhorn-v2-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Name, Run string } }
	}
	if err = yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	script := ""
	for _, step := range workflow.Jobs["prepare"].Steps {
		if step.Name == "Require confirmed first attempt on main" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("native pre-credential guard is missing")
	}
	for _, test := range []struct {
		operation, node, ref, attempt, confirm string
		allowed                                bool
	}{
		{"inventory-storage-layout", "temporary-v1-1", "refs/heads/main", "1", "inventory-longhorn-storage-layout-temporary-v1-1", true},
		{"inventory-storage-layout", "temporary-v2-1", "refs/heads/main", "1", "inventory-longhorn-storage-layout-temporary-v2-1", true},
		{"inventory-storage-layout", "prod-worker-1", "refs/heads/main", "1", "inventory-longhorn-storage-layout-prod-worker-1", false},
		{"inventory-storage-layout", "temporary-v1-1", "refs/heads/main", "1", "inventory-longhorn-storage-root-temporary-v1-1", false},
		{"inventory-storage-layout", "temporary-v1-1", "refs/heads/main", "2", "inventory-longhorn-storage-layout-temporary-v1-1", false},
		{"inventory-storage-layout", "temporary-v1-1", "refs/heads/feature", "1", "inventory-longhorn-storage-layout-temporary-v1-1", false},
		{"inspect-host-memory", "prod-worker-1", "refs/heads/main", "1", "inspect-longhorn-v2-host-memory-prod-worker-1", true},
		{"inspect-host-memory", "temporary-v1-1", "refs/heads/main", "1", "inspect-longhorn-v2-host-memory-temporary-v1-1", false},
		{"inspect-host-memory", "prod-worker-1", "refs/heads/feature", "1", "inspect-longhorn-v2-host-memory-prod-worker-1", false},
		{"inspect-host-memory", "prod-worker-1", "refs/heads/main", "2", "inspect-longhorn-v2-host-memory-prod-worker-1", false},
		{"inspect-host-memory", "prod-worker-1", "refs/heads/main", "1", "refresh-longhorn-v2-clients", false},
		{"inventory-storage-root", "temporary-v1-1", "refs/heads/main", "1", "inventory-longhorn-storage-root-temporary-v1-1", true},
		{"inventory-storage-root", "temporary-v2-1", "refs/heads/main", "1", "inventory-longhorn-storage-root-temporary-v2-1", true},
		{"inventory-storage-root", "prod-worker-1", "refs/heads/main", "1", "inventory-longhorn-storage-root-prod-worker-1", true},
		{"inventory-storage-root", "temporary-v1-1", "refs/heads/main", "1", "inventory-longhorn-replicas-temporary-v1-1", false},
		{"inventory-storage-root", "temporary-v1-1", "refs/heads/main", "2", "inventory-longhorn-storage-root-temporary-v1-1", false},
		{"inventory-storage-root", "temporary-v1-1", "refs/heads/feature", "1", "inventory-longhorn-storage-root-temporary-v1-1", false},
		{"inventory-replicas", "temporary-v1-1", "refs/heads/main", "1", "inventory-longhorn-replicas-temporary-v1-1", true},
		{"inventory-replicas", "temporary-v1-2", "refs/heads/main", "1", "inventory-longhorn-replicas-temporary-v1-2", true},
		{"inventory-replicas", "temporary-v2-1", "refs/heads/main", "1", "inventory-longhorn-replicas-temporary-v2-1", true},
		{"prepare-worker", "prod-worker-1", "refs/heads/main", "1", "prepare-longhorn-v2-prod-worker-1", true},
		{"prepare-worker", "temporary-v1-1", "refs/heads/main", "1", "prepare-longhorn-v2-temporary-v1-1", false},
		{"inventory-replicas", "temporary-v1-3", "refs/heads/main", "1", "inventory-longhorn-replicas-temporary-v1-3", false},
		{"inventory-replicas", "temporary-v1-1", "refs/heads/feature", "1", "inventory-longhorn-replicas-temporary-v1-1", false},
		{"inventory-replicas", "temporary-v1-1", "refs/heads/main", "2", "inventory-longhorn-replicas-temporary-v1-1", false},
		{"inventory-replicas", "temporary-v1-1", "refs/heads/main", "1", "prepare-longhorn-v2-temporary-v1-1", false},
	} {
		command := exec.Command("bash", "-c", script)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "DISPATCH_REF=" + test.ref, "GITHUB_RUN_ATTEMPT=" + test.attempt,
			"LONGHORN_V2_OPERATION=" + test.operation, "LONGHORN_V2_NODE=" + test.node, "LONGHORN_V2_CONFIRM=" + test.confirm}
		if err := command.Run(); (err == nil) != test.allowed {
			t.Fatalf("native guard for %s/%s/%s/%s: %v", test.operation, test.node, test.ref, test.attempt, err)
		}
	}
}
