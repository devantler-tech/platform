package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

type unusedPreparationFixture struct {
	base                        *preparationAPI
	baseline                    object
	nativeCalls                 int
	layoutWrites                int
	layoutInstalled             bool
	layoutWriteUncertain        bool
	badNativeAfter              int
	missingBaseline, noRecovery bool
	afterReset                  func()
	dropPinWrites               bool
}

func TestUnusedStagingPreparationRequiresTheLargerNativeLayout(t *testing.T) {
	f, p := unusedPreparationAPI(t)
	p.staging = true
	if err := p.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.layoutWrites != 1 || f.base.resets != 1 || p.j.Phase != "prepared" {
		t.Fatal("larger capacity did not use one verified layout change and one guarded reset")
	}
	if err := p.prepare(context.Background()); err != nil || f.layoutWrites != 1 || f.base.resets != 1 {
		t.Fatal("completed staging preparation repeated a mutation", err)
	}
}

func TestUncertainStagingConfigurationRequiresNativeReadbackBeforeReset(t *testing.T) {
	f, p := unusedPreparationAPI(t)
	p.staging, f.layoutWriteUncertain = true, true
	if err := p.prepare(context.Background()); err == nil || f.layoutWrites != 1 || f.base.resets != 0 || p.j.Phase != "draining" {
		t.Fatal("an uncertain staging change authorized a reset", err)
	}
	if err := p.prepare(context.Background()); err != nil || f.layoutWrites != 1 || f.base.resets != 1 || p.j.Phase != "prepared" {
		t.Fatal("native staging readback did not recover the guarded reset", err)
	}
}

func TestStagingRejectsInsufficientNativePartitionSizeBeforeMutation(t *testing.T) {
	f, p := unusedPreparationAPI(t)
	p.staging = true
	p.reader = func(context.Context, string) (nativeReplicaReader, func() error, error) {
		r := &unusedParentReaderFixture{
			temporaryNativeReader: temporaryNativeReader{boot: text(f.base.node, "status", "nodeInfo", "bootID")},
			ephemeralSize:         190 << 30,
		}
		return r, func() error { return nil }, nil
	}
	if err := p.prepare(context.Background()); err == nil || f.base.cm != nil || f.layoutWrites != 0 || f.base.resets != 0 {
		t.Fatal("smaller native partition allowed a staging change", err)
	}
}

func unusedBaselineFixture(t *testing.T, s inventory) object {
	t.Helper()
	machines, err := benchmarkMachines(s, "v1")
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := parseFIO(fioFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	record := benchmarkRecord{Version: 2, Phase: "v1", Source: strings.Repeat("1", 40), Run: "5678", Image: fioImage, Workload: benchmarkWorkload(), Namespace: "longhorn-benchmark-v1-5678", NamespaceUID: "scratch-uid", Machines: machines, Measurements: metrics, Cleaned: true, CompletedAt: "2026-10-29T18:00:00Z"}
	compute, managers, probe := benchmarkComputeFixture()
	record.CPU, err = benchmarkCompute(compute, managers, probe, "v1")
	if err != nil {
		t.Fatal(err)
	}
	record.Resources = map[string]benchmarkResource{
		"Namespace": {Name: record.Namespace, UID: "scratch-uid"}, "Pod": {Name: "fio", Namespace: record.Namespace, UID: "pod"},
		"NetworkPolicy": {Name: "fio-default-deny", Namespace: record.Namespace, UID: "policy"}, "PersistentVolumeClaim": {Name: "fio-data", Namespace: record.Namespace, UID: "claim"},
		"StorageClass": {Name: "lh-benchmark-v1-5678", UID: "class"}, "PersistentVolume": {Name: "scratch-pv", UID: "pv"}, "LonghornVolume": {Name: "scratch-volume", Namespace: "longhorn-system", UID: "volume"},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	cm := resource("longhorn-v2-benchmark-v1")
	cm["data"] = object{"record": string(encoded)}
	return cm
}

func unusedPreparationAPI(t *testing.T) (*unusedPreparationFixture, temporaryPreparation) {
	t.Helper()
	base, old := newPreparationAPI(t, "evacuating")
	base.s = benchmarkInventoryFixture()
	base.s.nodes = append(base.s.nodes, capacityFixture()[3:]...)
	for i, n := range base.s.nodes {
		n["metadata"].(object)["uid"] = "uid-" + text(n, "metadata", "name")
		if value(n, "metadata", "annotations") == nil {
			n["metadata"].(object)["annotations"] = object{}
		}
		if value(n, "spec") == nil {
			n["spec"] = object{}
		}
		n["spec"].(object)["providerID"] = fmt.Sprintf("hcloud://%d", 500+i)
		n["status"].(object)["nodeInfo"] = object{"systemUUID": "hardware-" + text(n, "metadata", "name"), "bootID": fmt.Sprintf("12345678-1234-4234-8234-%012x", i+1), "osImage": "Talos fixture", "architecture": "amd64"}
		n["status"].(object)["addresses"] = []any{object{"type": "InternalIP", "address": fmt.Sprintf("10.0.99.%d", 4+i)}}
		if i >= 3 && i < 6 {
			delete(n["metadata"].(object)["annotations"].(object), scaleDownDisabled)
			delete(n["metadata"].(object)["annotations"].(object), capacityOwnerAnnotation)
			lh := resource(text(n, "metadata", "name"))
			lh["spec"] = object{"allowScheduling": false, "evictionRequested": true, "disks": object{}}
			lh["status"] = object{"diskStatus": object{}}
			base.s.storageNodes = append(base.s.storageNodes, lh)
		}
	}
	base.node, base.storage, base.cm = base.s.nodes[3], base.s.storageNodes[3], nil
	base.v2Off = true
	f := &unusedPreparationFixture{base: base}
	f.baseline = unusedBaselineFixture(t, base.s)
	p := temporaryPreparation{preparation: preparation{target: text(base.node, "metadata", "name"), name: "longhorn-v2-prepare-temporary-v1-1", temporarySelection: "temporary-v1-1", source: old.source, owner: old.owner, pools: old.pools}}
	p.c = client{context: "admin@prod", run: f.run, inventoryPools: old.pools}
	p.reader = func(_ context.Context, address string) (nativeReplicaReader, func() error, error) {
		f.nativeCalls++
		if address != "10.0.99.7" {
			t.Fatal("temporary inspection selected another endpoint")
		}
		mode := "cache directories only"
		if f.badNativeAfter > 0 && f.nativeCalls >= f.badNativeAfter {
			mode = "foreign directory"
		}
		return &unusedParentReaderFixture{temporaryNativeReader: temporaryNativeReader{boot: text(base.node, "status", "nodeInfo", "bootID")}, mode: mode}, func() error { return nil }, nil
	}
	return f, p
}

func (f *unusedPreparationFixture) run(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	f.base.t.Helper()
	if program == "gh" {
		return json.Marshal(object{"status": "completed", "head_sha": strings.Repeat("1", 40), "head_branch": "main", "event": "workflow_dispatch", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"})
	}
	if program == "kubectl" && len(args) > 3 {
		if args[2] == "get" && strings.HasPrefix(args[3], "--raw=") {
			api, kind := "v1", "NodeList"
			var items []object
			switch args[3] {
			case "--raw=" + inventoryKubeNodes:
				items = f.base.s.nodes
			case "--raw=" + inventoryStorageNodes:
				api = "longhorn.io/v1beta2"
				items = f.base.s.storageNodes
			case "--raw=" + inventoryReplicas:
				api, kind = "longhorn.io/v1beta2", "ReplicaList"
				items = f.base.s.replicas
			default:
				f.base.t.Fatal("unexpected raw request")
			}
			return json.Marshal(object{"apiVersion": api, "kind": kind, "metadata": object{"resourceVersion": "1"}, "items": items})
		}
		if len(args) > 4 && args[2] == "get" && args[3] == "configmap" && args[4] == "longhorn-v2-benchmark-v1" {
			if f.missingBaseline {
				return nil, nil
			}
			return json.Marshal(f.baseline)
		}
		if len(args) > 4 && (args[2] == "get" || args[2] == "patch") && args[3] == "node" {
			if args[2] == "patch" && f.dropPinWrites {
				var patch []object
				if json.Unmarshal(input, &patch) != nil {
					f.base.t.Fatal("malformed test patch")
				}
				for _, change := range patch {
					if text(change, "path") == "/metadata/annotations" && text(valueObject(change, "value"), capacityOwnerAnnotation) == capacityOwnerValue {
						return nil, nil
					}
				}
			}
			for _, node := range f.base.s.nodes {
				if text(node, "metadata", "name") == args[4] {
					original := f.base.node
					f.base.node = node
					defer func() { f.base.node = original }()
					return f.base.run(ctx, input, program, args...)
				}
			}
			f.base.t.Fatal("unknown node request")
		}
	}
	if program == "talosctl" && len(args) > 1 && args[1] == "patch" {
		want := []string{"--nodes=10.0.99.7", "patch", "machineconfig", "--mode=no-reboot", "--patch=" + stagingLayoutPatch}
		var j journal
		if !reflect.DeepEqual(args, want) || json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &j) != nil ||
			!j.StagingLayout || j.Phase != "draining" || f.nativeCalls < 2 || value(f.base.node, "spec", "unschedulable") != true {
			f.base.t.Fatal("staging patch lacked the exact target, journal, cordon and fresh native proof")
		}
		f.layoutWrites++
		f.layoutInstalled = true
		if f.layoutWriteUncertain {
			return nil, errors.New("staging configuration accepted but response lost")
		}
		return nil, nil
	}
	if f.layoutInstalled && program == "talosctl" && len(args) == 5 && args[2] == "volumeconfig" {
		raw, ephemeral := desiredConfigFixture()
		rawPartition := valueObject(raw, "spec", "provisioning", "partitionSpec")
		rawPartition["minSize"], rawPartition["maxSize"] = float64(144<<30), float64(144<<30)
		valueObject(ephemeral, "spec", "provisioning", "partitionSpec")["maxSize"] = float64(152 << 30)
		if args[3] == "EPHEMERAL" {
			return json.Marshal(ephemeral)
		}
		return json.Marshal(raw)
	}
	if program == "talosctl" && len(args) > 1 && args[1] == "reset" {
		want := []string{"--nodes=10.0.99.7", "reset", "--system-labels-to-wipe=EPHEMERAL", "--reboot", "--graceful=true", "--wait", "--timeout=15m"}
		var j journal
		if !reflect.DeepEqual(args, want) || json.Unmarshal([]byte(text(f.base.cm, "data", "record")), &j) != nil || j.Schema != 2 || j.Phase != "reset-requested" || f.nativeCalls < 3 {
			f.base.t.Fatal("reset lacked its separate journal, fresh physical proof or exact EPHEMERAL scope")
		}
		f.base.resets++
		if !f.noRecovery {
			f.base.postReset = true
			f.base.node["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-000000000099"
		}
		if f.afterReset != nil {
			f.afterReset()
		}
		if f.base.resetError {
			return nil, errors.New("accepted, response lost")
		}
		return nil, nil
	}
	data, err := f.base.run(ctx, input, program, args...)
	if err == nil && f.base.postReset && program == "talosctl" && len(args) == 5 && args[2] == "volumestatus" &&
		(args[3] == "EPHEMERAL" || f.layoutInstalled && args[3] == "r-longhorn") {
		var volume object
		if json.Unmarshal(data, &volume) != nil {
			f.base.t.Fatal("malformed fixture volume")
		}
		size := float64(128 << 30)
		if args[3] == "r-longhorn" {
			size = float64(144 << 30)
		}
		volume["spec"].(object)["size"] = size
		return json.Marshal(volume)
	}
	return data, err
}

func TestUnusedTemporaryPreparationPreservesV1AndLeavesRawStorageFenced(t *testing.T) {
	f, p := unusedPreparationAPI(t)
	before, _ := json.Marshal(f.base.s.storageNodes[:3])
	if err := p.prepare(context.Background()); err != nil {
		t.Fatalf("empty temporary worker could not be prepared: %v (phase=%s native=%d resets=%d snapshots=%d)", err, p.j.Phase, f.nativeCalls, f.base.resets, f.base.snapshots)
	}
	after, _ := json.Marshal(f.base.s.storageNodes[:3])
	if f.base.resets != 1 || p.j.Phase != "prepared" || !reflect.DeepEqual(before, after) || value(f.base.storage, "spec", "allowScheduling") != false || value(f.base.storage, "spec", "evictionRequested") != true || len(valueObject(f.base.storage, "spec", "disks")) != 0 || value(f.base.node, "spec", "unschedulable") == true || text(f.base.node, "metadata", "annotations", fenceAnnotation) != "" {
		t.Fatal("temporary preparation changed V1 storage, enabled raw storage, or failed to recover its node")
	}
	if err := p.prepare(context.Background()); err != nil || f.base.resets != 1 {
		t.Fatal("prepared recovery replayed a reset:", err)
	}
}

type unusedParentReaderFixture struct {
	temporaryNativeReader
	rootCalls     int
	mode          string
	ephemeralSize uint64
}

func (*unusedParentReaderFixture) ListReplicas(context.Context) (longhorninventory.Listing, error) {
	return longhorninventory.Listing{}, errors.New("missing replica path is not an emptiness proof")
}

func (r *unusedParentReaderFixture) ListStorageRoot(context.Context) (longhorninventory.Listing, error) {
	r.rootCalls++
	entries := []longhorninventory.Entry{{Name: ".", Mode: uint32(os.ModeDir), IsDir: true}}
	for _, name := range []string{"engine-binaries", "metadata", "logs", "unix-domain-socket"} {
		entries = append(entries, longhorninventory.Entry{Name: name, Mode: uint32(os.ModeDir), IsDir: true})
	}
	switch r.mode {
	case "replicas", "backing-images", "foreign directory":
		entries = append(entries, longhorninventory.Entry{Name: r.mode, Mode: uint32(os.ModeDir), IsDir: true})
	case "file":
		entries = append(entries, longhorninventory.Entry{Name: "unexpected", Mode: 0o600})
	case "changed":
		if r.rootCalls > 1 {
			entries = append(entries, longhorninventory.Entry{Name: "changed", Mode: uint32(os.ModeDir), IsDir: true})
		}
	case "error":
		return longhorninventory.Listing{Complete: true, Entries: entries}, errors.New("private-parent-error")
	}
	return longhorninventory.Listing{Complete: r.mode != "partial", Entries: entries}, nil
}

func (r *unusedParentReaderFixture) ListBlockLayout(context.Context) (longhorninventory.BlockLayoutReport, error) {
	size := r.ephemeralSize
	if size == 0 {
		size = 270 << 30
	}
	return longhorninventory.BlockLayoutReport{VolumeStatuses: 2, DiscoveredVolumes: 1, EphemeralPhase: "ready", EphemeralSizeBytes: size, RawPresent: true, RawPhase: "waiting", Digest: strings.Repeat("a", 64)}, nil
}

func TestUnusedPreparationQualifiesStableParentAbsenceWithoutAReplicaPath(t *testing.T) {
	for _, mode := range []string{"cache directories only", "replicas", "backing-images", "foreign directory", "file", "partial", "changed", "error"} {
		t.Run(mode, func(t *testing.T) {
			f, p := unusedPreparationAPI(t)
			var readers []*unusedParentReaderFixture
			p.reader = func(context.Context, string) (nativeReplicaReader, func() error, error) {
				f.nativeCalls++
				r := &unusedParentReaderFixture{temporaryNativeReader: temporaryNativeReader{boot: text(f.base.node, "status", "nodeInfo", "bootID")}, mode: mode}
				readers = append(readers, r)
				return r, func() error { return nil }, nil
			}
			err := p.prepare(context.Background())
			if mode == "cache directories only" {
				if err != nil || f.base.resets != 1 || len(readers) < 3 {
					t.Fatalf("positive stable parent absence did not reach the guarded reset: err=%v resets=%d observations=%d", err, f.base.resets, len(readers))
				}
				observations := 0
				for _, r := range readers {
					if r.rootCalls != 0 && r.rootCalls != 2 {
						t.Fatal("reset skipped an independent parent observation")
					}
					if r.rootCalls == 2 {
						observations++
					}
				}
				if observations < 3 {
					t.Fatal("reset lacked initial and two post-drain parent observations")
				}
			} else if err == nil || f.base.resets != 0 || strings.Contains(err.Error(), "private-parent-error") {
				t.Fatal("unqualified parent inventory allowed reset or exposed provider details")
			}
		})
	}
}

func TestUnusedTemporaryPreparationHoldsBeforeResetOnLostEvidence(t *testing.T) {
	for _, scenario := range []string{"missing baseline", "native data", "native data after drain", "foreign pin", "unready peer", "source changed", "layout changed"} {
		t.Run(scenario, func(t *testing.T) {
			f, p := unusedPreparationAPI(t)
			switch scenario {
			case "missing baseline":
				f.missingBaseline = true
			case "native data":
				f.badNativeAfter = 1
			case "native data after drain":
				f.badNativeAfter = 2
			case "foreign pin":
				f.base.s.nodes[4]["metadata"].(object)["annotations"].(object)[capacityOwnerAnnotation] = "foreign"
			case "unready peer":
				f.base.s.nodes[5]["status"].(object)["conditions"] = []any{}
			case "source changed":
				f.base.staleSource = true
			case "layout changed":
				f.base.staleLayout = true
			}
			if p.prepare(context.Background()) == nil || f.base.resets != 0 {
				t.Fatal("lost evidence authorized a temporary reset")
			}
		})
	}
}

func TestUncertainTemporaryResetIsNeverReplayed(t *testing.T) {
	f, p := unusedPreparationAPI(t)
	f.base.resetError, f.noRecovery = true, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.afterReset = cancel
	if p.prepare(ctx) == nil || f.base.resets != 1 || p.j.Phase != "reset-requested" {
		t.Fatal("uncertain reset lost its durable intent")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if p.prepare(ctx2) == nil || f.base.resets != 1 {
		t.Fatal("recovery replayed the uncertain destructive request")
	}
}

func TestUnusedTemporaryCapacityHoldPrecedesJournalAndCordon(t *testing.T) {
	for _, kind := range []string{"logical", "actual"} {
		t.Run(kind, func(t *testing.T) {
			f, p := unusedPreparationAPI(t)
			if kind == "logical" {
				// Below the individual disk size, but above its allocation budget.
				f.base.s.volumes[0]["spec"].(object)["size"] = fmt.Sprint(87 << 30)
			} else {
				f.base.s.volumes[0]["status"].(object)["actualSize"] = float64(63 << 30)
			}
			if p.prepare(context.Background()) == nil || f.base.cm != nil || f.base.resets != 0 || value(f.base.node, "spec", "unschedulable") == true {
				t.Fatal("insufficient capacity was accepted or preparation mutated the worker before holding")
			}
		})
	}
}

func TestUnusedTemporaryCapacityIsRecheckedInBothPostDrainObservations(t *testing.T) {
	for _, growthAt := range []int{1, 2} {
		t.Run(fmt.Sprint(growthAt), func(t *testing.T) {
			f, p := unusedPreparationAPI(t)
			run := p.c.run
			drained, observations := false, 0
			p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "kubectl" && len(args) > 3 {
					if drained && len(args) == 5 && args[2] == "get" && args[3] == "nodes" && args[4] == "--output=json" {
						observations++
						if observations == growthAt {
							f.base.s.volumes[0]["status"].(object)["actualSize"] = float64(63 << 30)
						}
					}
				}
				out, err := run(ctx, input, program, args...)
				if err == nil && program == "kubectl" && len(args) > 2 && args[2] == "drain" {
					drained = true
				}
				return out, err
			}
			if p.prepare(context.Background()) == nil || !drained || observations != growthAt || f.base.resets != 0 || p.j.Phase == "reset-requested" || p.j.Phase == "prepared" {
				t.Fatal("post-drain storage growth reached reset intent or was not observed")
			}
		})
	}
}

func TestTemporaryPreparationRebindsIdentityAndStorageBeforeResumingWrites(t *testing.T) {
	for _, scenario := range []string{"storage enabled", "node replaced", "unexpected reboot"} {
		t.Run(scenario, func(t *testing.T) {
			f, p := unusedPreparationAPI(t)
			if err := p.beginUnused(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "storage enabled":
				f.base.storage["spec"].(object)["allowScheduling"] = true
			case "node replaced":
				f.base.node["metadata"].(object)["uid"] = "replacement"
			case "unexpected reboot":
				f.base.node["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-000000000099"
			}
			if p.prepare(context.Background()) == nil || f.base.resets != 0 || value(f.base.node, "spec", "unschedulable") == true || text(f.base.node, "metadata", "annotations", capacityOwnerAnnotation) != "" {
				t.Fatal("changed storage or node identity reached a preparation write")
			}
		})
	}
}

func TestTemporaryCordonRequiresObservedPoolProtection(t *testing.T) {
	f, p := unusedPreparationAPI(t)
	f.dropPinWrites = true
	if p.prepare(context.Background()) == nil || f.base.resets != 0 || value(f.base.node, "spec", "unschedulable") == true {
		t.Fatal("an acknowledgement without live scale-down protection reached the worker cordon")
	}
}
