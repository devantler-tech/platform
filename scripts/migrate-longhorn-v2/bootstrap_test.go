package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

func TestCapacityProtectionDoesNotBorrowForeignAnnotations(t *testing.T) {
	n := migrationNodeFixture("v1", "0123456789abcdef")
	if err := verifyCapacityPin(n, false); err != nil {
		t.Fatal(err)
	}
	if err := verifyCapacityPin(n, true); err == nil {
		t.Fatal("missing live scale-down protection accepted")
	}
	a := n["metadata"].(object)["annotations"].(object)
	a[scaleDownDisabled] = "true"
	if err := verifyCapacityPin(n, false); err == nil {
		t.Fatal("foreign scale-down protection was adopted")
	}
	a[capacityOwnerAnnotation] = capacityOwnerValue
	if err := verifyCapacityPin(n, true); err != nil {
		t.Fatal(err)
	}
	a[scaleDownDisabled] = "false"
	if err := verifyCapacityPin(n, false); err == nil {
		t.Fatal("drifted owned pin was treated as unused capacity")
	}
}

func TestTemporaryFilesystemMustBeEncryptedAndLargeEnough(t *testing.T) {
	_, ephemeral := rawFixture()
	ephemeral["spec"].(object)["size"] = float64(280 * 1024 * 1024 * 1024)
	disks, err := migrationFilesystem(ephemeral)
	if err != nil || value(disks["migration-v1"].(object), "storageReserved") != float64(84*1024*1024*1024) {
		t.Fatalf("encrypted filesystem capacity rejected or system headroom lost: %v", err)
	}
	ephemeral["spec"].(object)["size"] = float64(40 * 1024 * 1024 * 1024)
	if _, err := migrationFilesystem(ephemeral); err == nil {
		t.Fatal("small root filesystem could become migration storage")
	}
	ephemeral["spec"].(object)["size"] = float64(280 * 1024 * 1024 * 1024)
	delete(ephemeral["spec"].(object), "encryptionProvider")
	if _, err := migrationFilesystem(ephemeral); err == nil {
		t.Fatal("unencrypted temporary capacity accepted")
	}
}

type capacityAPI struct {
	base           *preparationAPI
	nodes, storage map[string]object
	failPin        string
	storageWrites  int
	afterPins      func(*capacityAPI)
	afterStorage   func(*capacityAPI)
}

func newCapacityAPI(t *testing.T) (*capacityAPI, client, migrationPool) {
	t.Helper()
	return newCapacityAPIFor(t, "v1", "0123456789abcdef", "1123456789abcdef")
}

func newCapacityAPIFor(t *testing.T, role string, suffixes ...string) (*capacityAPI, client, migrationPool) {
	t.Helper()
	base, _ := newPreparationAPI(t, "evacuating")
	base.s = evacuatedFixture()
	f := &capacityAPI{base: base, nodes: map[string]object{}, storage: map[string]object{}}
	for _, suffix := range suffixes {
		n := migrationNodeFixture(role, suffix)
		name := text(n, "metadata", "name")
		f.nodes[name] = n
		lh := resource(name)
		lh["spec"] = object{"disks": object{}, "allowScheduling": false, "evictionRequested": true}
		f.storage[name] = lh
		base.s.nodes = append(base.s.nodes, n)
		base.s.storageNodes = append(base.s.storageNodes, lh)
	}
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	return f, client{run: f.run, context: "admin@prod"}, pools[role]
}

func (f *capacityAPI) run(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	f.base.t.Helper()
	if program == "talosctl" {
		if len(args) == 5 && args[2] == "systeminformation" {
			return f.base.encode(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": "machine-uuid"}})
		}
		raw, e := rawFixture()
		if len(args) == 5 && args[3] == "r-longhorn" {
			return f.base.encode(raw)
		}
		e["spec"].(object)["size"] = float64(280 * 1024 * 1024 * 1024)
		return f.base.encode(e)
	}
	if program == "kubectl" && len(args) > 4 {
		a := args[2:]
		var objects map[string]object
		if a[1] == "node" {
			objects = f.nodes
		}
		if a[1] == "nodes.longhorn.io" {
			objects = f.storage
		}
		if o := objects[a[2]]; o != nil {
			if a[0] == "get" {
				return f.base.encode(o)
			}
			if a[0] == "patch" {
				if a[1] == "node" && a[2] == f.failPin {
					return nil, errors.New("pin CAS conflicted")
				}
				if a[1] == "nodes.longhorn.io" {
					for _, n := range f.nodes {
						if err := verifyCapacityPin(n, true); err != nil {
							f.base.t.Fatal("storage enabled before every pool node was pinned")
						}
					}
					f.storageWrites++
				}
				patch, err := jsonpatch.DecodePatch(input)
				if err != nil {
					f.base.t.Fatal(err)
				}
				before, err := json.Marshal(o)
				if err != nil {
					f.base.t.Fatal(err)
				}
				after, err := patch.Apply(before)
				if err != nil {
					return nil, err
				}
				var updated object
				if err = json.Unmarshal(after, &updated); err != nil {
					f.base.t.Fatal(err)
				}
				if a[1] == "nodes.longhorn.io" {
					statuses := object{}
					for disk := range valueObject(updated, "spec", "disks") {
						statuses[disk] = object{"diskUUID": "new-disk", "conditions": schedulableConditions()}
					}
					updated["status"] = object{"conditions": schedulableConditions(), "diskStatus": statuses}
				}
				objects[a[2]] = updated
				if a[1] == "nodes.longhorn.io" && f.afterStorage != nil && f.storageWrites == len(f.storage) {
					change := f.afterStorage
					f.afterStorage = nil
					change(f)
				}
				if a[1] == "node" && f.afterPins != nil {
					allPinned := true
					for _, node := range f.nodes {
						allPinned = allPinned && verifyCapacityPin(node, true) == nil
					}
					if allPinned {
						change := f.afterPins
						f.afterPins = nil
						change(f)
					}
				}
				return nil, nil
			}
		}
	}
	return f.base.run(ctx, input, program, args...)
}

func TestCapacityActivationPinsTheWholePoolBeforeStorage(t *testing.T) {
	f, c, pool := newCapacityAPI(t)
	if err := bootstrapCapacity(context.Background(), c, journalFixture().Source, pool); err != nil {
		t.Fatal(err)
	}
	if f.storageWrites != 2 {
		t.Fatal("both verified capacity nodes must be activated")
	}
}

func TestCapacityActivationDoesNotProceedThroughFailedProtection(t *testing.T) {
	f, c, pool := newCapacityAPI(t)
	f.failPin = "autoscale-longhorn-v1-1123456789abcdef"
	if err := bootstrapCapacity(context.Background(), c, journalFixture().Source, pool); err == nil || f.storageWrites != 0 {
		t.Fatal("partial pinning activated replica storage")
	}
}

func TestCapacityActivationRechecksTheWholePinnedPoolBeforeStorage(t *testing.T) {
	f, c, pool := newCapacityAPI(t)
	f.afterPins = func(f *capacityAPI) {
		f.nodes["autoscale-longhorn-v1-1123456789abcdef"]["spec"].(object)["providerID"] = "hcloud://99"
	}
	if err := bootstrapCapacity(context.Background(), c, journalFixture().Source, pool); err == nil || f.storageWrites != 0 {
		t.Fatal("hardware drift in the second node allowed the first disk to be enabled")
	}
}

func TestCapacityActivationSucceedsOnlyOnCurrentProtectedSchedulableCapacity(t *testing.T) {
	// The poll runs until the injected fault's grace ends, never counting time before the writes.
	const pollGrace = 200 * time.Millisecond
	onlyDisk := func(lh object) object {
		for _, disk := range valueObject(lh, "status", "diskStatus") {
			return disk.(map[string]any)
		}
		t.Fatal("fixture has no disk status")
		return nil
	}
	faults := map[string]func(n, lh object){
		"missing disk status": func(_, lh object) { lh["status"].(object)["diskStatus"] = object{} },
		"extra disk status":   func(_, lh object) { valueObject(lh, "status", "diskStatus")["other"] = object{"diskUUID": "x"} },
		"empty disk UUID":     func(_, lh object) { onlyDisk(lh)["diskUUID"] = "" },
		"failed disk": func(_, lh object) {
			onlyDisk(lh)["conditions"] = []any{object{"type": "Ready", "status": "False"}, object{"type": "Schedulable", "status": "True"}}
		},
		"unschedulable disk": func(_, lh object) { onlyDisk(lh)["conditions"] = readyConditions() },
		"unschedulable node": func(_, lh object) { lh["status"].(object)["conditions"] = readyConditions() },
		"replaced storage":   func(_, lh object) { lh["metadata"].(object)["uid"] = "other" },
		"replaced node":      func(n, _ object) { n["metadata"].(object)["uid"] = "other" },
		"changed provider":   func(n, _ object) { n["spec"].(object)["providerID"] = "hcloud://99" },
		"changed boot":       func(n, _ object) { n["status"].(object)["nodeInfo"].(object)["bootID"] = "other" },
		"lost protection":    func(n, _ object) { delete(n["metadata"].(object)["annotations"].(object), scaleDownDisabled) },
	}
	for _, engine := range []struct {
		role     string
		suffixes []string
	}{
		{"v1", []string{"0123456789abcdef", "1123456789abcdef"}},
		{"v2", []string{"0123456789abcdef"}},
	} {
		t.Run(engine.role+"/control", func(t *testing.T) {
			f, c, pool := newCapacityAPIFor(t, engine.role, engine.suffixes...)
			if err := bootstrapCapacity(context.Background(), c, journalFixture().Source, pool); err != nil || f.storageWrites != len(engine.suffixes) {
				t.Fatalf("complete capacity was refused after %d writes: %v", f.storageWrites, err)
			}
		})
		for name, fault := range faults {
			t.Run(engine.role+"/"+name, func(t *testing.T) {
				f, c, pool := newCapacityAPIFor(t, engine.role, engine.suffixes...)
				node := "autoscale-longhorn-" + engine.role + "-0123456789abcdef"
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.afterStorage = func(f *capacityAPI) {
					fault(f.nodes[node], f.storage[node])
					time.AfterFunc(pollGrace, cancel)
				}
				if err := bootstrapCapacity(ctx, c, journalFixture().Source, pool); err == nil || f.storageWrites != len(engine.suffixes) {
					t.Fatalf("activation after %d writes reported %v", f.storageWrites, err)
				}
			})
		}
	}
	t.Run("stale source", func(t *testing.T) {
		f, c, pool := newCapacityAPI(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.afterStorage = func(f *capacityAPI) {
			f.base.staleSource = true
			time.AfterFunc(pollGrace, cancel)
		}
		if err := bootstrapCapacity(ctx, c, journalFixture().Source, pool); err == nil || f.storageWrites != 2 {
			t.Fatalf("activation with a stale source after %d writes reported %v", f.storageWrites, err)
		}
	})
}

func TestCapacityActivationCannotUseStaleSource(t *testing.T) {
	f, c, pool := newCapacityAPI(t)
	f.base.staleSource = true
	if err := bootstrapCapacity(context.Background(), c, journalFixture().Source, pool); err == nil || f.storageWrites != 0 {
		t.Fatal("stale source activated storage")
	}
	for _, node := range f.nodes {
		if text(node, "metadata", "annotations", capacityOwnerAnnotation) != "" {
			t.Fatal("stale source claimed a node pin")
		}
	}
}
