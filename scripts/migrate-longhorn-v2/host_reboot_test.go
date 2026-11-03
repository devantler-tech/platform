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
	afterDatabaseRead          func()
	afterScheduling            func()
	previousLive               bool
	managers                   []object
	drainPolicy                string
	evictUnfilteredRuntime     bool
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
	f := &hostRebootFixture{base: b, desired: "1024", drainPolicy: "block-if-contains-last-replica"}
	im := resource("instance-manager-host-fixture")
	im["metadata"].(object)["namespace"] = "longhorn-system"
	im["spec"] = object{"nodeID": "prod-worker-1", "dataEngine": "v1"}
	im["status"] = object{"currentState": "running"}
	f.managers = []object{im}
	pod := resource(text(im, "metadata", "name"))
	pod["metadata"].(object)["namespace"] = "longhorn-system"
	pod["metadata"].(object)["labels"] = object{"longhorn.io/component": "instance-manager"}
	pod["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "InstanceManager", "name": text(im, "metadata", "name"), "uid": text(im, "metadata", "uid"), "controller": true}}
	pod["spec"] = object{"nodeName": "prod-worker-1"}
	pod["status"] = object{"phase": "Running", "conditions": readyConditions()}
	b.s.pods = append(b.s.pods, pod)
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
		if args[3] != "proc.sys.vm.nr_hugepages" {
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
		if len(args) > 4 && args[2] == "get" && args[3] == "settings.longhorn.io" && args[4] == "node-drain-policy" {
			setting := resource("node-drain-policy")
			setting["metadata"].(object)["namespace"] = "longhorn-system"
			setting["value"] = f.drainPolicy
			return json.Marshal(setting)
		}
		if args[2] == "get" && args[3] == "instancemanagers.longhorn.io" {
			return json.Marshal(object{"items": f.managers})
		}
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
			if f.drains > 0 && f.afterDatabaseRead != nil {
				f.afterDatabaseRead()
			}
			return json.Marshal(object{"items": f.databases})
		}
		if args[2] == "drain" {
			f.drains++
			filtered := false
			for _, arg := range args {
				filtered = filtered || arg == "--pod-selector=longhorn.io/component!=instance-manager"
			}
			if f.evictUnfilteredRuntime && !filtered {
				for _, replica := range f.base.s.replicas {
					if text(replica, "spec", "nodeID") == "prod-worker-1" {
						replica["status"].(object)["currentState"] = "stopped"
					}
				}
			}
			if f.afterDrain != nil {
				f.afterDrain()
			}
			if f.drainError {
				return nil, errors.New("eviction blocked by disruption budget")
			}
		}
	}
	wasCordoned := value(f.base.node, "spec", "unschedulable") == true
	output, err := f.base.run(ctx, input, program, args...)
	if err == nil && program == "kubectl" && len(args) > 3 && args[2] == "patch" && args[3] == "node" && f.rebooted && wasCordoned && value(f.base.node, "spec", "unschedulable") == false && f.afterScheduling != nil {
		f.afterScheduling()
	}
	return output, err
}

func TestHostRebootRestoresSchedulingBeforeWaitingForTheStorageController(t *testing.T) {
	f, h := hostRebootAPI(t)
	originalStates := map[string]string{}
	for _, replica := range f.base.s.replicas {
		originalStates[text(replica, "metadata", "name")] = text(replica, "status", "currentState")
	}
	f.afterReboot = func() {
		f.managers[0]["status"].(object)["currentState"] = "starting"
		for _, volume := range f.base.s.volumes {
			if text(volume, "status", "state") == "attached" {
				volume["status"].(object)["robustness"] = "degraded"
			}
		}
		for _, replica := range f.base.s.replicas {
			if text(replica, "spec", "nodeID") == h.target {
				replica["status"].(object)["currentState"] = "stopped"
			}
		}
	}
	f.afterScheduling = func() {
		var record hostRebootRecord
		if json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &record) != nil || record.Phase != "returning" || record.RecoveredBootID == "" || text(f.base.node, "metadata", "annotations", fenceAnnotation) != h.name() {
			t.Fatal("scheduling resumed without durable recovered-host proof and retained maintenance ownership")
		}
		f.managers[0]["status"].(object)["currentState"] = "running"
		for _, volume := range f.base.s.volumes {
			if text(volume, "status", "state") == "attached" {
				volume["status"].(object)["robustness"] = "healthy"
			}
		}
		for _, replica := range f.base.s.replicas {
			if text(replica, "spec", "nodeID") == h.target {
				replica["status"].(object)["currentState"] = originalStates[text(replica, "metadata", "name")]
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := h.run(ctx); err != nil || f.reboots != 1 {
		t.Fatal("storage runtime could not restart after positively verified host recovery:", err)
	}
}

func TestHostRebootDoesNotResumeSchedulingWithoutTwoIntactPeerCopies(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.afterReboot = func() {
		for _, replica := range f.base.s.replicas {
			if text(replica, "spec", "nodeID") == "prod-worker-2" {
				replica["status"].(object)["currentState"] = "stopped"
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if h.run(ctx) == nil || f.reboots != 1 || value(f.base.node, "spec", "unschedulable") != true {
		t.Fatal("scheduling resumed without two intact peer copies")
	}
}

func TestReturningHostStillRequiresTwoLivePeersAndFinalHealthyVolumes(t *testing.T) {
	f, h := hostRebootAPI(t)
	for _, volume := range f.base.s.volumes {
		if text(volume, "status", "state") == "attached" {
			volume["status"].(object)["robustness"] = "degraded"
		}
	}
	if err := verifyHealthyCopies(f.base.s, 2, h.target); err != nil {
		t.Fatal("two proven peer copies were rejected during return to scheduling:", err)
	}
	if verifyRedundancy(f.base.s) == nil {
		t.Fatal("degraded volumes satisfied the final full-redundancy acceptance gate")
	}
	for _, replica := range f.base.s.replicas {
		if text(replica, "spec", "nodeID") == "prod-worker-2" {
			replica["status"].(object)["currentState"] = "stopped"
		}
	}
	if verifyHealthyCopies(f.base.s, 2, h.target) == nil {
		t.Fatal("the degraded label substituted for an intact live peer replica")
	}
}

func TestReturningHostKeepsItsMarkerUntilFullRecoveryWithoutRepeatingTheReboot(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.afterReboot = func() { f.managers[0]["status"].(object)["currentState"] = "starting" }
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := h.run(ctx)
	var record hostRebootRecord
	if !errors.Is(err, context.DeadlineExceeded) || json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &record) != nil || record.Phase != "returning" || f.reboots != 1 || value(f.base.node, "spec", "unschedulable") != false || text(f.base.node, "metadata", "annotations", fenceAnnotation) != h.name() {
		t.Fatal("unfinished storage recovery lost its durable ownership or was accepted:", err)
	}
	f.managers[0]["status"].(object)["currentState"] = "running"
	if err = h.run(t.Context()); err != nil || f.reboots != 1 || text(f.base.node, "metadata", "annotations", fenceAnnotation) != "" {
		t.Fatal("resuming storage recovery repeated the reboot or failed to finish:", err)
	}
}

func TestHostRebootDrainsApplicationsWithoutEvictingProvenStorageRuntime(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.evictUnfilteredRuntime = true
	if err := h.run(t.Context()); err != nil || f.reboots != 1 {
		t.Fatal("ordinary instance-manager eviction prevented the controlled reboot:", err)
	}
}

func TestHostRebootRefusesUnknownDrainPolicyOrExcludedWorkloads(t *testing.T) {
	for name, change := range map[string]func(*hostRebootFixture){
		"unknown policy":      func(f *hostRebootFixture) { f.drainPolicy = "always-allow" },
		"missing policy":      func(f *hostRebootFixture) { f.drainPolicy = "" },
		"unproven controller": func(f *hostRebootFixture) { f.managers[0]["metadata"].(object)["uid"] = "replacement" },
		"missing runtime":     func(f *hostRebootFixture) { f.managers = nil },
		"unready runtime":     func(f *hostRebootFixture) { f.managers[0]["status"].(object)["currentState"] = "starting" },
		"unrelated excluded workload": func(f *hostRebootFixture) {
			pod := resource("unrelated-workload")
			pod["metadata"].(object)["namespace"] = "application"
			pod["metadata"].(object)["labels"] = object{"longhorn.io/component": "instance-manager"}
			pod["spec"] = object{"nodeName": "prod-worker-1"}
			f.base.s.pods = append(f.base.s.pods, pod)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			change(f)
			if h.run(t.Context()) == nil || f.drains != 0 || f.reboots != 0 {
				t.Fatal("unproven storage runtime or policy reached maintenance")
			}
		})
	}
}

func TestHostRebootRebindsTheExcludedRuntimeAfterDrain(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.afterDrain = func() { f.managers[0]["metadata"].(object)["uid"] = "replacement" }
	if h.run(t.Context()) == nil || f.drains != 1 || f.reboots != 0 {
		t.Fatal("changed excluded runtime reached the reboot request")
	}
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

func TestHostRebootWaitsForTheOriginalDatabaseQuorumAfterDrain(t *testing.T) {
	f, h := hostRebootAPI(t)
	reads := 0
	f.afterDrain = func() {
		f.databases[0]["status"].(object)["readyInstances"] = float64(2)
		f.afterDatabaseRead = func() {
			reads++
			if reads == 2 {
				f.databases[0]["status"].(object)["readyInstances"] = float64(3)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := h.run(ctx); err != nil {
		t.Fatal("ordinary asynchronous database replacement aborted the guarded reboot:", err)
	}
	if reads < 2 || f.reboots != 1 || value(f.base.node, "spec", "unschedulable") == true {
		t.Fatal("database recovery was not observed before reboot and scheduling recovery")
	}
}

func setHostDrainAttachmentTransition(f *hostRebootFixture, state string) func() {
	volume := f.base.s.volumes[0]
	name := text(volume, "metadata", "name")
	volume["status"].(object)["state"] = state
	volume["status"].(object)["robustness"] = "unknown"
	var engine object
	for _, candidate := range f.base.s.engines {
		if text(candidate, "spec", "volumeName") == name {
			engine = candidate
			break
		}
	}
	if engine == nil {
		f.base.t.Fatal("fixture has no active engine")
	}
	modes := value(engine, "status", "replicaModeMap")
	engine["status"].(object)["currentState"] = "starting"
	if state == "detaching" {
		engine["status"].(object)["currentState"] = "stopping"
	}
	engine["status"].(object)["replicaModeMap"] = object{}
	return func() {
		volume["status"].(object)["state"] = "attached"
		volume["status"].(object)["robustness"] = "healthy"
		engine["status"].(object)["currentState"] = "running"
		engine["status"].(object)["replicaModeMap"] = modes
	}
}

func TestHostRebootWaitsForAProvenPostDrainAttachmentTransition(t *testing.T) {
	for _, state := range []string{"attaching", "detaching"} {
		t.Run(state, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			reads := 0
			f.afterDrain = func() {
				restore := setHostDrainAttachmentTransition(f, state)
				f.afterDatabaseRead = func() {
					reads++
					if reads == 2 {
						restore()
					}
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			if err := h.run(ctx); err != nil || reads < 2 || f.reboots != 1 {
				t.Fatal("normal asynchronous attachment prevented the guarded reboot:", err)
			}
		})
	}
}

func TestHostDrainTransitionDoesNotHideOtherStorageOrPeerFailures(t *testing.T) {
	for _, mode := range []string{"lost other copy", "engine error", "replica error", "replica mode error", "faulted volume", "unreadable peer"} {
		t.Run(mode, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			reads := 0
			f.afterDatabaseRead = func() { reads++ }
			f.afterDrain = func() {
				setHostDrainAttachmentTransition(f, "attaching")
				switch mode {
				case "lost other copy":
					other := text(f.base.s.volumes[1], "metadata", "name")
					for _, replica := range f.base.s.replicas {
						if text(replica, "spec", "volumeName") == other {
							replica["spec"].(object)["healthyAt"] = ""
							break
						}
					}
				case "engine error":
					f.base.s.engines[0]["status"].(object)["currentState"] = "error"
				case "replica error":
					f.base.s.replicas[0]["status"].(object)["currentState"] = "error"
				case "replica mode error":
					f.base.s.engines[0]["status"].(object)["replicaModeMap"] = object{text(f.base.s.replicas[0], "metadata", "name"): "ERR"}
				case "faulted volume":
					f.base.s.volumes[0]["status"].(object)["robustness"] = "faulted"
				case "unreadable peer":
					f.peerJournal = resource("longhorn-v2-host-memory-prod-worker-2")
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := h.run(ctx)
			if !errors.Is(err, errReadinessTerminal) || f.reboots != 0 || (mode == "unreadable peer" && reads != 1) || (mode != "unreadable peer" && reads != 0) {
				t.Fatal("attachment retry hid a different failed safety check:", err)
			}
		})
	}
}

func TestHostRebootPreservesItsCordonWhenDrainRecoveryTimesOut(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.afterDrain = func() { f.databases[0]["status"].(object)["readyInstances"] = float64(2) }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := h.run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || f.drains != 1 || f.reboots != 0 || value(f.base.node, "spec", "unschedulable") != true {
		t.Fatal("unproven post-drain recovery did not stop at its deadline with maintenance retained:", err)
	}
}

func TestHostRebootDrainRecoveryDoesNotRetryChangedIdentityOrLostQuorum(t *testing.T) {
	for name, change := range map[string]func(object){
		"cluster identity": func(db object) { db["metadata"].(object)["uid"] = "replacement" },
		"system identity":  func(db object) { db["status"].(object)["systemID"] = "987654321" },
		"lost quorum":      func(db object) { db["status"].(object)["readyInstances"] = float64(1) },
	} {
		t.Run(name, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			reads := 0
			f.afterDrain = func() {
				f.databases[0]["status"].(object)["readyInstances"] = float64(2)
				change(f.databases[0])
			}
			f.afterDatabaseRead = func() { reads++ }
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := h.run(ctx)
			if !errors.Is(err, errReadinessTerminal) || reads != 1 || f.reboots != 0 {
				t.Fatal("an invalid recovery state was retried or allowed a reboot:", err)
			}
		})
	}
}

func TestHostRebootDrainRecoveryRejectsAnUnreadablePeerDuringDatabaseStartup(t *testing.T) {
	f, h := hostRebootAPI(t)
	f.afterDrain = func() {
		f.databases[0]["status"].(object)["readyInstances"] = float64(2)
		f.peerJournal = resource("longhorn-v2-host-memory-prod-worker-2")
		f.peerJournal["metadata"].(object)["namespace"] = "longhorn-system"
		f.peerJournal["data"] = object{"record": "invalid"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.run(ctx); !errors.Is(err, errReadinessTerminal) || f.reboots != 0 {
		t.Fatal("database startup hid an unreadable peer maintenance record:", err)
	}
}

func TestHostRebootRecoveryProbeGetsTheDeclaredWindowAndParentDeadline(t *testing.T) {
	for _, parentLimit := range []time.Duration{0, 2 * time.Minute} {
		t.Run(parentLimit.String(), func(t *testing.T) {
			f, h := hostRebootAPI(t)
			ctx := context.Background()
			if parentLimit > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, parentLimit)
				defer cancel()
			}
			want := 20 * time.Minute
			if parentLimit > 0 {
				want = parentLimit
			}
			observed := false
			inspect := h.inspect
			h.inspect = func(ctx context.Context) (hostMemoryReport, error) {
				if f.rebooted {
					deadline, bounded := ctx.Deadline()
					remaining := time.Until(deadline)
					if !bounded || remaining > want || remaining < want-time.Minute {
						t.Fatalf("recovery probe got an incorrect deadline: remaining=%v want=%v bounded=%v", remaining, want, bounded)
					}
					observed = true
				}
				return inspect(ctx)
			}
			if err := h.run(ctx); err != nil || !observed {
				t.Fatal("recovery never completed its bounded native observation:", err)
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
	for _, phase := range []string{"draining", "reboot-requested", "returning"} {
		t.Run(phase, func(t *testing.T) {
			f, h := hostRebootAPI(t)
			if _, err := h.initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, r, err := h.read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			peer, storage := f.base.s.nodes[1], f.base.s.storageNodes[1]
			r.Target, r.Phase = "prod-worker-2", phase
			r.NodeUID, r.ProviderID, r.MachineUUID = text(peer, "metadata", "uid"), text(peer, "spec", "providerID"), text(peer, "status", "nodeInfo", "systemUUID")
			r.OriginalBootID, r.LonghornUID, r.OriginalDiskUUID = text(peer, "status", "nodeInfo", "bootID"), text(storage, "metadata", "uid"), hostRegisteredDiskUUID(storage)
			if phase == "returning" {
				r.RecoveredBootID = "12345678-1234-4234-8234-000000000099"
			}
			if r.validate("prod-worker-2") != nil {
				t.Fatal("peer fixture did not represent a valid pending request")
			}
			encoded, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			f.peerJournal = resource("longhorn-v2-host-memory-prod-worker-2")
			f.peerJournal["metadata"].(object)["namespace"] = "longhorn-system"
			f.peerJournal["data"] = object{"record": string(encoded)}
			f.base.cm = nil
			if h.run(context.Background()) == nil || f.reboots != 0 || f.drains != 0 || value(f.base.node, "spec", "unschedulable") == true {
				t.Fatal("another worker was disrupted before the earlier reboot was proven complete")
			}
			if f.base.cm != nil {
				t.Fatal("a refused peer dispatch left a new journal that blocks the original worker's recovery")
			}
		})
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
