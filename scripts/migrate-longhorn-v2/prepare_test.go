package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type preparationAPI struct {
	t                 *testing.T
	node, storage, cm object
	s                 inventory
	resets, snapshots int
	resetError        bool
	postReset         bool
	staleSource       bool
	conflict          bool
	changeInventory   func(*inventory)
}

func newPreparationAPI(t *testing.T, phase string) (*preparationAPI, preparation) {
	t.Helper()
	j := journalFixture()
	j.Phase = phase
	n := nodeFixture()
	n["metadata"].(object)["annotations"].(object)[fenceAnnotation] = "longhorn-v2-prepare-prod-worker-1"
	n["spec"].(object)["unschedulable"] = true
	lh := resource(j.Target)
	lh["metadata"].(object)["uid"] = j.LonghornUID
	lh["spec"] = object{"disks": j.OriginalDisks, "allowScheduling": false, "evictionRequested": true}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	cm := resource("longhorn-v2-prepare-prod-worker-1")
	cm["data"] = object{"record": string(b)}
	s := evacuatedFixture()
	for i := 1; i <= 3; i++ {
		old, replacement := s.nodes[i]["metadata"].(object)["name"].(string), text(capacityFixture()[i], "metadata", "name")
		s.storageNodes[i-1]["metadata"].(object)["name"] = replacement
		for _, r := range s.replicas {
			if text(r, "spec", "nodeID") == old {
				r["spec"].(object)["nodeID"] = replacement
			}
		}
		for _, e := range s.engines {
			if text(e, "spec", "nodeID") == old {
				e["spec"].(object)["nodeID"] = replacement
			}
		}
	}
	s.nodes = capacityFixture()
	s.nodes[0] = n
	f := &preparationAPI{t: t, node: n, storage: lh, cm: cm, s: s}
	p := preparation{c: client{run: f.run, context: "admin@prod"}, target: j.Target, name: text(cm, "metadata", "name"), source: j.Source, owner: j.OwnerRun, j: j}
	p.pools, err = configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}

func (f *preparationAPI) encode(v any) ([]byte, error) {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	return b, nil
}

func (f *preparationAPI) run(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
	f.t.Helper()
	switch program {
	case "git":
		sha := journalFixture().Source
		if f.staleSource {
			sha = strings.Repeat("a", 40)
		}
		return []byte(sha + "\trefs/heads/main\n"), nil
	case "talosctl":
		if len(args) > 1 && args[1] == "reset" {
			want := []string{"--nodes=10.0.99.4", "reset", "--system-labels-to-wipe=EPHEMERAL", "--reboot", "--graceful=true", "--wait", "--timeout=15m"}
			if !reflect.DeepEqual(args, want) {
				f.t.Fatalf("reset scope widened: %v", args)
			}
			var j journal
			if json.Unmarshal([]byte(text(f.cm, "data", "record")), &j) != nil || j.Phase != "reset-requested" || f.snapshots < 2 {
				f.t.Fatal("reset preceded its durable intent or two complete observations")
			}
			f.resets++
			f.postReset = true
			if f.resetError {
				return nil, errors.New("accepted, response lost")
			}
			return nil, nil
		}
		if len(args) != 5 || args[1] != "get" || args[4] != "--output=json" {
			f.t.Fatalf("unexpected host read: %v", args)
		}
		var raw, ephemeral object
		switch args[2] {
		case "systeminformation":
			return f.encode(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": text(f.node, "status", "nodeInfo", "systemUUID")}})
		case "volumeconfig":
			raw, ephemeral = desiredConfigFixture()
		case "volumestatus":
			raw, ephemeral = rawFixture()
			if !f.postReset {
				ephemeral["spec"].(object)["uuid"] = journalFixture().EphemeralUUID
				ephemeral["spec"].(object)["size"] = float64(160 * 1024 * 1024 * 1024)
			}
		default:
			f.t.Fatalf("unexpected host resource: %v", args)
		}
		if args[3] == "r-longhorn" {
			return f.encode(raw)
		}
		return f.encode(ephemeral)
	case "kubectl":
		if len(args) < 3 || args[0] != "--context=admin@prod" || args[1] != "--request-timeout=20s" {
			f.t.Fatalf("unbound cluster request: %v", args)
		}
		args = args[2:]
		if args[0] == "drain" || args[0] == "wait" {
			if strings.Contains(strings.Join(args, " "), "--force") || strings.Contains(strings.Join(args, " "), "disable-eviction") {
				f.t.Fatal("drain bypassed disruption protections")
			}
			return nil, nil
		}
		if args[0] == "get" {
			if args[2] == "--output=json" {
				var items []object
				switch args[1] {
				case "nodes":
					f.snapshots++
					if f.snapshots == 2 && f.changeInventory != nil {
						f.changeInventory(&f.s)
					}
					items = f.s.nodes
				case "nodes.longhorn.io":
					items = f.s.storageNodes
				case "volumes.longhorn.io":
					items = f.s.volumes
				case "replicas.longhorn.io":
					items = f.s.replicas
				case "engines.longhorn.io":
					items = f.s.engines
				case "pods":
					items = f.s.pods
				default:
					f.t.Fatalf("unexpected inventory: %v", args)
				}
				if items == nil {
					items = []object{}
				}
				return f.encode(object{"items": items})
			}
			switch args[1] {
			case "settings.longhorn.io":
				setting := resource(args[2])
				setting["value"] = "100"
				if args[2] == "storage-minimal-available-percentage" {
					setting["value"] = "25"
				}
				return f.encode(setting)
			case "node":
				return f.encode(f.node)
			case "nodes.longhorn.io":
				return f.encode(f.storage)
			case "configmap":
				if f.cm == nil {
					return nil, nil
				}
				return f.encode(f.cm)
			}
		}
		if args[0] == "create" {
			if f.cm != nil || !reflect.DeepEqual(args, []string{"create", "--filename=/dev/stdin", "--output=name"}) {
				f.t.Fatal("journal create must be exclusive and file sourced")
			}
			if json.Unmarshal(input, &f.cm) != nil || text(f.cm, "kind") != "ConfigMap" || text(f.cm, "metadata", "namespace") != "longhorn-system" {
				f.t.Fatal("journal create exceeded its scope")
			}
			f.cm["metadata"].(object)["uid"] = "new-journal"
			f.cm["metadata"].(object)["resourceVersion"] = "1"
			return nil, nil
		}
		if args[0] == "patch" {
			if f.conflict {
				return nil, errors.New("resource version conflict")
			}
			var patches []object
			if json.Unmarshal(input, &patches) != nil || len(patches) < 3 {
				f.t.Fatal("conditional JSON patch required")
			}
			var o object
			switch args[1] {
			case "node":
				o = f.node
			case "nodes.longhorn.io":
				o = f.storage
			case "configmap":
				o = f.cm
			default:
				f.t.Fatalf("unexpected write: %v", args)
			}
			if !reflect.DeepEqual(patches[:2], identityTests(o)) {
				f.t.Fatal("write lost UID or resource-version binding")
			}
			for _, patch := range patches[2:] {
				parts := strings.Split(strings.TrimPrefix(text(patch, "path"), "/"), "/")
				parent := o
				for _, part := range parts[:len(parts)-1] {
					parent = parent[part].(object)
				}
				key := strings.ReplaceAll(parts[len(parts)-1], "~1", "/")
				if text(patch, "op") == "remove" {
					delete(parent, key)
				} else {
					parent[key] = patch["value"]
				}
			}
			return nil, nil
		}
	}
	f.t.Fatalf("unexpected external command: %s %v", program, args)
	return nil, errors.New("unexpected command")
}

func TestCompletePreparationCreatesItsJournalBeforeAnyStorageChange(t *testing.T) {
	f, p := newPreparationAPI(t, "evacuating")
	f.cm = nil
	f.node["spec"].(object)["unschedulable"] = false
	delete(f.node["metadata"].(object)["annotations"].(object), fenceAnnotation)
	f.storage["spec"].(object)["allowScheduling"] = true
	f.storage["spec"].(object)["evictionRequested"] = false
	if err := p.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.resets != 1 || p.j.Phase != "prepared" || value(f.node, "spec", "unschedulable") != false || text(f.node, "metadata", "annotations", fenceAnnotation) != "" {
		t.Fatal("complete preparation lost its journal or failed to release only its own fence")
	}
}

func TestResetRequiresFreshEvacuationAndRetainsItsIntent(t *testing.T) {
	f, p := newPreparationAPI(t, "draining")
	if err := p.drainAndReset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.resets != 1 || p.j.Phase != "reset-requested" {
		t.Fatal("expected exactly one journaled ephemeral-only reset")
	}
}

func TestUnsafeOrChangedStateNeverRequestsAReset(t *testing.T) {
	for name, mutate := range map[string]func(*preparationAPI){
		"source moved": func(f *preparationAPI) { f.staleSource = true },
		"CAS conflict": func(f *preparationAPI) { f.conflict = true },
		"foreign cordon": func(f *preparationAPI) {
			f.node["metadata"].(object)["annotations"].(object)[fenceAnnotation] = "someone-else"
		},
		"retained copy lost between observations": func(f *preparationAPI) {
			f.changeInventory = func(s *inventory) { s.replicas = s.replicas[:len(s.replicas)-1] }
		},
		"replica returns to reset target": func(f *preparationAPI) {
			f.changeInventory = func(s *inventory) { s.replicas[0]["spec"].(object)["nodeID"] = "prod-worker-1" }
		},
		"foreign storage fence": func(f *preparationAPI) { f.storage["spec"].(object)["allowScheduling"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			f, p := newPreparationAPI(t, "draining")
			mutate(f)
			if err := p.drainAndReset(context.Background()); err == nil || f.resets != 0 {
				t.Fatal("unsafe state reached the destructive operation")
			}
		})
	}
}

func TestRecoveryNeverReplaysAnUncertainReset(t *testing.T) {
	f, p := newPreparationAPI(t, "draining")
	f.resetError = true
	if err := p.drainAndReset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.resets != 1 || p.j.Phase != "prepared" || !reflect.DeepEqual(value(f.storage, "spec", "disks"), blockDisks()) {
		t.Fatal("recovery repeated reset or failed to prepare the verified raw disk")
	}
}

func TestUnacceptedResetLeavesFencesAndJournalForDiagnosis(t *testing.T) {
	f, p := newPreparationAPI(t, "reset-requested")
	f.storage["spec"].(object)["disks"] = object{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.prepare(ctx); err == nil || f.resets != 0 || value(f.node, "spec", "unschedulable") != true || p.j.Phase != "reset-requested" {
		t.Fatal("unknown reset outcome was replayed or its fences were removed")
	}
}

func TestPreparedReceiptDoesNotHideSubsequentDrift(t *testing.T) {
	f, p := newPreparationAPI(t, "prepared")
	f.storage["spec"].(object)["disks"] = blockDisks()
	f.storage["spec"].(object)["allowScheduling"] = true
	f.storage["spec"].(object)["evictionRequested"] = false
	f.node["spec"].(object)["unschedulable"] = false
	delete(f.node["metadata"].(object)["annotations"].(object), fenceAnnotation)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.prepare(ctx); err == nil || f.resets != 0 {
		t.Fatal("a historical prepared receipt hid the original unreconstructed partition")
	}
}

func TestForeignPostResetCordonCannotReenableStorageScheduling(t *testing.T) {
	f, p := newPreparationAPI(t, "reset-requested")
	f.postReset = true
	f.storage["spec"].(object)["disks"] = object{}
	f.node["metadata"].(object)["annotations"].(object)[fenceAnnotation] = "another-operation"
	if err := p.finish(context.Background()); err == nil || value(f.storage, "spec", "allowScheduling") != false {
		t.Fatal("a foreign cordon did not preserve the storage fence")
	}
}
