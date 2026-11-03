package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type hostRebootFixture struct {
	badSettingScope            bool
	peerJournal                object
	base                       *preparationAPI
	databases                  []object
	reboots, drains            int
	rebooted, lost, noRecovery bool
	desired                    string
	drainError                 bool
	afterDrain, afterReboot    func()
	previousLive               bool
}

func hostRebootAPI(t *testing.T) (*hostRebootFixture, hostMemoryReboot) {
	t.Helper()
	b, old := newPreparationAPI(t, "draining")
	b.s = benchmarkInventoryFixture()
	b.s.nodes = append(b.s.nodes, capacityFixture()[3:]...)
	for i, n := range b.s.nodes {
		n["metadata"].(object)["annotations"] = object{}
		n["metadata"].(object)["uid"] = fmt.Sprintf("node-%d", i)
		if value(n, "spec") == nil {
			n["spec"] = object{}
		}
		n["spec"].(object)["unschedulable"] = false
		n["spec"].(object)["providerID"] = fmt.Sprintf("hcloud://%d", 500+i)
		n["status"].(object)["nodeInfo"] = object{"systemUUID": fmt.Sprintf("hardware-%d", i), "bootID": fmt.Sprintf("12345678-1234-4234-8234-%012x", i+1), "osImage": "Talos fixture", "architecture": "amd64"}
		n["status"].(object)["addresses"] = []any{object{"type": "InternalIP", "address": fmt.Sprintf("10.0.99.%d", i+4)}}
	}
	b.node, b.storage, b.cm, b.v2Off = b.s.nodes[0], b.s.storageNodes[0], nil, true
	for _, storage := range b.s.storageNodes {
		storage["metadata"].(object)["namespace"] = "longhorn-system"
		storage["spec"].(object)["disks"] = object{"data": object{"path": "/var/lib/longhorn", "diskType": "filesystem", "allowScheduling": true, "evictionRequested": false}}
		status := value(storage, "status", "diskStatus", "data").(object)
		status["diskPath"], status["diskType"] = "/var/lib/longhorn", "filesystem"
	}
	for _, replica := range b.s.replicas {
		replica["spec"].(object)["diskPath"] = "/var/lib/longhorn"
	}
	b.node["status"].(object)["capacity"] = object{"hugepages-2Mi": "0"}
	b.node["status"].(object)["allocatable"] = object{"hugepages-2Mi": "0", "memory": "14980Mi"}
	f := &hostRebootFixture{base: b, desired: "1024"}
	for i, selected := range []struct{ namespace, name string }{{"observability", "coroot-db"}, {"umami", "umami-db"}, {"wedding-app", "wedding-db"}} {
		db := resource(selected.name)
		db["metadata"].(object)["namespace"] = selected.namespace
		db["spec"] = object{"instances": float64(3)}
		db["status"] = object{"readyInstances": float64(3), "systemID": fmt.Sprintf("1234567%d", i), "phase": "Cluster in healthy state"}
		f.databases = append(f.databases, db)
	}
	h := hostMemoryReboot{c: client{context: "admin@prod", run: f.run}, target: "prod-worker-1", source: old.source, owner: old.owner, pools: old.pools}
	if err := verifyCapacityRoster(b.s.nodes, h.pools, false); err != nil {
		t.Fatal("invalid initial capacity fixture:", err)
	}
	if err := verifyRedundancy(b.s); err != nil {
		t.Fatal("invalid initial redundancy fixture:", err)
	}
	h.inspect = func(context.Context) (hostMemoryReport, error) {
		pages := uint64(1)
		if f.rebooted {
			pages = 1024
		}
		return hostMemoryReport{PersistentPages: pages, TotalPages: pages, FreePages: pages, PageSizeKiB: 2048, TotalKiB: 16 << 20, AvailableKiB: 7 << 20}, nil
	}
	return f, h
}

func (f *hostRebootFixture) run(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	f.base.t.Helper()
	if program == "talosctl" && len(args) >= 2 && args[1] == "reboot" {
		want := []string{"--nodes=10.0.99.4", "reboot", "--wait", "--timeout=15m"}
		var record object
		if !reflect.DeepEqual(args, want) || json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &record) != nil || text(record, "phase") != "reboot-requested" || f.drains != 1 || value(f.base.node, "spec", "unschedulable") != true {
			f.base.t.Fatal("reboot escaped its drained, journaled, exact-node scope")
		}
		f.reboots++
		if !f.noRecovery {
			f.rebooted = true
			f.base.node["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-000000000099"
			f.base.node["status"].(object)["capacity"] = object{"hugepages-2Mi": "2Gi"}
			f.base.node["status"].(object)["allocatable"] = object{"hugepages-2Mi": "2Gi", "memory": "12932Mi"}
		}
		if f.afterReboot != nil {
			f.afterReboot()
		}
		if f.lost {
			return nil, errors.New("response lost after request")
		}
		return nil, nil
	}
	if program == "talosctl" && len(args) == 5 && args[1] == "get" && args[2] == "kernelparamspec" {
		if args[3] != "sysctl.vm.nr_hugepages" {
			f.base.t.Fatal("host configuration read escaped the single public parameter")
		}
		return json.Marshal(object{"metadata": object{"namespace": "runtime", "id": args[3]}, "spec": object{"value": f.desired}})
	}
	if program == "gh" {
		var record object
		if json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &record) != nil {
			f.base.t.Fatal("previous run was requested without a readable journal")
		}
		status := "completed"
		if f.previousLive {
			status = "in_progress"
		}
		return json.Marshal(object{"status": status, "head_sha": text(record, "source"), "head_branch": "main", "event": "workflow_dispatch", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"})
	}
	if program == "kubectl" && len(args) > 3 {
		if len(args) > 4 && args[2] == "get" && args[3] == "settings.longhorn.io" && args[4] == "v2-data-engine" {
			data, err := f.base.run(ctx, input, program, args...)
			if err != nil {
				return nil, err
			}
			var setting object
			if json.Unmarshal(data, &setting) != nil {
				f.base.t.Fatal("invalid setting fixture")
			}
			setting["metadata"].(object)["namespace"] = "longhorn-system"
			if f.badSettingScope {
				setting["metadata"].(object)["namespace"] = "unrelated"
			}
			return json.Marshal(setting)
		}
		if len(args) > 4 && args[2] == "get" && args[3] == "configmap" && args[4] != "longhorn-v2-host-memory-prod-worker-1" {
			if args[4] == "longhorn-v2-host-memory-prod-worker-2" && f.peerJournal != nil {
				return json.Marshal(f.peerJournal)
			}
			return nil, nil
		}
		if args[2] == "get" && args[3] == "clusters.postgresql.cnpg.io" {
			return json.Marshal(object{"items": f.databases})
		}
		if args[2] == "drain" {
			f.drains++
			if f.afterDrain != nil {
				f.afterDrain()
			}
			if f.drainError {
				return nil, errors.New("eviction blocked by disruption budget")
			}
		}
	}
	return f.base.run(ctx, input, program, args...)
}

func TestHostRebootRequiresPositiveRecoveryAndPreservesEveryDisk(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("response lost ", lost), func(t *testing.T) {
			f, h := hostRebootAPI(t)
			f.lost = lost
			before, _ := json.Marshal(f.base.s.storageNodes)
			if err := h.run(context.Background()); err != nil {
				t.Fatal("guarded reboot did not recover:", err)
			}
			after, _ := json.Marshal(f.base.s.storageNodes)
			if f.reboots != 1 || f.base.resets != 0 || !reflect.DeepEqual(before, after) || value(f.base.node, "spec", "unschedulable") == true || text(f.base.node, "metadata", "annotations", fenceAnnotation) != "" {
				t.Fatal("reboot changed disks, replayed a reset or failed to release only its cordon")
			}
			if err := h.run(context.Background()); err != nil || f.reboots != 1 {
				t.Fatal("completed recovery replayed the reboot:", err)
			}
		})
	}
}

func TestHostRebootHoldsBeforeRequestOnLostSafetyEvidence(t *testing.T) {
	for _, mode := range []string{"source", "desired pages", "foreign cordon", "database", "replica", "drain", "identity after drain", "source after drain", "V2 flag", "setting scope", "storage scope after drain"} {
		t.Run(mode, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			switch mode {
			case "source":
				f.base.staleSource = true
			case "desired pages":
				f.desired = "1"
			case "foreign cordon":
				f.base.node["spec"].(object)["unschedulable"] = true
				f.base.node["metadata"].(object)["annotations"].(object)[fenceAnnotation] = "foreign"
			case "database":
				f.databases[0]["status"].(object)["readyInstances"] = float64(2)
			case "replica":
				f.base.s.replicas[0]["spec"].(object)["failedAt"] = time.Now().UTC().Format(time.RFC3339)
			case "drain":
				f.drainError = true
			case "identity after drain":
				f.afterDrain = func() { f.base.node["metadata"].(object)["uid"] = "replacement" }
			case "source after drain":
				f.afterDrain = func() { f.base.staleSource = true }
			case "V2 flag":
				f.base.v2Off = false
			case "setting scope":
				f.badSettingScope = true
			case "storage scope after drain":
				f.afterDrain = func() { f.base.storage["metadata"].(object)["namespace"] = "unrelated" }
			}
			if h.run(context.Background()) == nil || f.reboots != 0 || f.base.resets != 0 {
				t.Fatal("lost safety evidence authorized a host request")
			}
		})
	}
}

func TestHostRebootRejectsReplacedRegisteredDiskEvenWhenReplicasAreHealthy(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.afterReboot = func() {
		value(f.base.storage, "status", "diskStatus", "data").(object)["diskUUID"] = "replacement-disk"
		for _, replica := range f.base.s.replicas {
			if text(replica, "spec", "nodeID") == h.target {
				replica["spec"].(object)["diskID"] = "replacement-disk"
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if h.run(ctx) == nil || f.reboots != 1 || value(f.base.node, "spec", "unschedulable") != true {
		t.Fatal("replacement registered storage qualified as the original recovered disk")
	}
}

func TestUncertainHostRebootCannotBeRepeated(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.lost, f.noRecovery = true, true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if h.run(ctx) == nil || f.reboots != 1 {
		t.Fatal("uncertain reboot was accepted or lost its durable intent")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if h.run(ctx) == nil || f.reboots != 1 {
		t.Fatal("recovery repeated an uncertain reboot")
	}
}

func TestHostRebootCannotStartWhileAnotherWorkerHasAnUnfinishedJournal(t *testing.T) {
	f, h := hostRebootAPI(t)
	if _, err := h.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, r, err := h.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	peer, storage := f.base.s.nodes[1], f.base.s.storageNodes[1]
	r.Target, r.Phase = "prod-worker-2", "reboot-requested"
	r.NodeUID, r.ProviderID, r.MachineUUID = text(peer, "metadata", "uid"), text(peer, "spec", "providerID"), text(peer, "status", "nodeInfo", "systemUUID")
	r.OriginalBootID, r.LonghornUID, r.OriginalDiskUUID = text(peer, "status", "nodeInfo", "bootID"), text(storage, "metadata", "uid"), hostRegisteredDiskUUID(storage)
	if r.validate("prod-worker-2") != nil {
		t.Fatal("peer fixture did not represent a valid pending request")
	}
	encoded, _ := json.Marshal(r)
	f.peerJournal = resource("longhorn-v2-host-memory-prod-worker-2")
	f.peerJournal["metadata"].(object)["namespace"] = "longhorn-system"
	f.peerJournal["data"] = object{"record": string(encoded)}
	if h.run(context.Background()) == nil || f.reboots != 0 || f.drains != 0 || value(f.base.node, "spec", "unschedulable") == true {
		t.Fatal("another worker was disrupted before the earlier reboot was proven complete")
	}
}

func TestHostRebootRejectsAJournalOutsideItsFixedNameOrNamespace(t *testing.T) {
	for _, field := range []string{"name", "namespace"} {
		t.Run(field, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			if _, err := h.initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.base.cm["metadata"].(object)[field] = "unrelated"
			if h.run(context.Background()) == nil || f.reboots != 0 || f.drains != 0 || value(f.base.node, "spec", "unschedulable") == true {
				t.Fatal("an API response outside the fixed journal scope authorized maintenance")
			}
		})
	}
}

func TestHostRebootRecoveryAdoptsOnlyATerminalOwnerWithoutAnotherRequest(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.noRecovery = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if h.run(ctx) == nil || f.reboots != 1 {
		t.Fatal("initial uncertain request was not preserved")
	}
	h.owner, f.previousLive = "5678", true
	if h.run(context.Background()) == nil || f.reboots != 1 {
		t.Fatal("recovery stole a live run's journal")
	}
	f.previousLive, f.rebooted = false, true
	f.base.node["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-000000000099"
	f.base.node["status"].(object)["capacity"] = object{"hugepages-2Mi": "2Gi"}
	f.base.node["status"].(object)["allocatable"] = object{"hugepages-2Mi": "2Gi", "memory": "12932Mi"}
	if err := h.run(context.Background()); err != nil || f.reboots != 1 || f.drains != 1 || value(f.base.node, "spec", "unschedulable") == true {
		t.Fatal("terminal-owner recovery replayed the request or failed positive recovery:", err)
	}
}

func TestHostRebootAuthorizationIsDistinctAndFixed(t *testing.T) {
	for _, mode := range []string{"confirmed", "read confirmation", "reset confirmation", "temporary", "fourth worker", "feature", "rerun", "other operation"} {
		t.Run(mode, func(t *testing.T) {
			env := dispatchFixture()
			env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"], env["LONGHORN_V2_CONFIRM"] = "reboot-host-for-hugepages", "prod-worker-1", "reboot-longhorn-v2-host-prod-worker-1"
			switch mode {
			case "read confirmation":
				env["LONGHORN_V2_CONFIRM"] = "inspect-longhorn-v2-host-memory-prod-worker-1"
			case "reset confirmation":
				env["LONGHORN_V2_CONFIRM"] = "prepare-longhorn-v2-prod-worker-1"
			case "temporary":
				env["LONGHORN_V2_NODE"] = "temporary-v1-1"
				env["LONGHORN_V2_CONFIRM"] = "reboot-longhorn-v2-host-temporary-v1-1"
			case "fourth worker":
				env["LONGHORN_V2_NODE"] = "prod-worker-4"
				env["LONGHORN_V2_CONFIRM"] = "reboot-longhorn-v2-host-prod-worker-4"
			case "feature":
				env["GITHUB_REF"] = "refs/heads/feature"
			case "rerun":
				env["GITHUB_RUN_ATTEMPT"] = "2"
			case "other operation":
				env["LONGHORN_V2_OPERATION"] = "inspect-host-memory"
			}
			getenv := func(key string) string { return env[key] }
			if (authorizeHostReboot(getenv, env["LONGHORN_V2_NODE"]) == nil) != (mode == "confirmed") {
				t.Fatal("reboot authorization crossed its operation, confirmation, selection or attempt boundary")
			}
		})
	}
}
