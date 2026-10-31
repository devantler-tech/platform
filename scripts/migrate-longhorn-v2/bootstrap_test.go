package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v5"
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
}

func newCapacityAPI(t *testing.T) (*capacityAPI, client, migrationPool) {
	t.Helper()
	base, _ := newPreparationAPI(t, "evacuating")
	base.s = evacuatedFixture()
	f := &capacityAPI{base: base, nodes: map[string]object{}, storage: map[string]object{}}
	for _, suffix := range []string{"0123456789abcdef", "1123456789abcdef"} {
		n := migrationNodeFixture("v1", suffix)
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
	return f, client{run: f.run, context: "admin@prod"}, pools["v1"]
}

func (f *capacityAPI) run(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	f.base.t.Helper()
	if program == "talosctl" {
		if len(args) == 5 && args[2] == "systeminformation" {
			return f.base.encode(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": "machine-uuid"}})
		}
		_, e := rawFixture()
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
				before, _ := json.Marshal(o)
				after, err := patch.Apply(before)
				if err != nil {
					return nil, err
				}
				var updated object
				if err = json.Unmarshal(after, &updated); err != nil {
					f.base.t.Fatal(err)
				}
				if a[1] == "nodes.longhorn.io" {
					updated["status"] = object{"conditions": readyConditions(), "diskStatus": object{"migration-v1": object{"diskUUID": "new-disk", "conditions": readyConditions()}}}
				}
				objects[a[2]] = updated
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
