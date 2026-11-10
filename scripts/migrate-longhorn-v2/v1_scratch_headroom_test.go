package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestV1ScratchHeadroomQualifiesOneClaim catches a reservation change that
// exceeds the one-claim budget or unnecessarily changes an already-ready disk.
func TestV1ScratchHeadroomQualifiesOneClaim(t *testing.T) {
	s := v1ReservationFixture(true)
	plan, err := planV1ScratchHeadroom(s)
	if err != nil || !plan.write || plan.reserve != 29*v1FixtureGiB || plan.disk != "default-disk-fixture" {
		t.Fatalf("healthy three-copy fixture did not qualify exactly four GiB: %+v, %v", plan, err)
	}

	valueObject(s.storageNodes[0], "spec", "disks", "default-disk-fixture")["storageReserved"] = float64(29 * v1FixtureGiB)
	plan, err = planV1ScratchHeadroom(s)
	if err != nil || plan.write || plan.reserve != 29*v1FixtureGiB {
		t.Fatalf("existing scratch capacity required another write: %+v, %v", plan, err)
	}
}

// TestV1ScratchHeadroomRefusesUnprovenCapacity guards the physical and logical
// capacity on each peer as well as the target disk ledger.
func TestV1ScratchHeadroomRefusesUnprovenCapacity(t *testing.T) {
	for name, change := range map[string]func(*inventory){
		"degraded source": func(s *inventory) { *s = v1ReservationFixture(false) },
		"peer physical margin": func(s *inventory) {
			valueObject(s.storageNodes[1], "status", "diskStatus", "default-disk")["storageAvailable"] = float64(45 * v1FixtureGiB)
		},
		"peer logical margin": func(s *inventory) {
			valueObject(s.storageNodes[1], "spec", "disks", "default-disk")["storageReserved"] = float64(30 * v1FixtureGiB)
			valueObject(s.storageNodes[2], "spec", "disks", "default-disk")["storageReserved"] = float64(30 * v1FixtureGiB)
		},
		"peer reservation floor": func(s *inventory) {
			for _, node := range s.storageNodes[1:] {
				valueObject(node, "spec", "disks", "default-disk")["storageReserved"] = float64(30 * v1FixtureGiB)
				valueObject(node, "status", "diskStatus", "default-disk")["storageMaximum"] = float64(151 * v1FixtureGiB)
			}
		},
		"target ledger drift": func(s *inventory) {
			delete(valueObject(s.storageNodes[0], "status", "diskStatus", "default-disk-fixture", "scheduledReplica"), "retained-1")
		},
		"partially consumed existing allowance": func(s *inventory) {
			valueObject(s.storageNodes[0], "spec", "disks", "default-disk-fixture")["storageReserved"] = float64(31 * v1FixtureGiB)
		},
		"foreign cordon": func(s *inventory) {
			valueObject(s.nodes[1], "metadata")["annotations"] = object{fenceAnnotation: "foreign"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := v1ReservationFixture(true)
			change(&s)
			if plan, err := planV1ScratchHeadroom(s); err == nil || plan.write {
				t.Fatalf("unsafe scratch capacity accepted: %+v, %v", plan, err)
			}
		})
	}
}

// TestV1ScratchHeadroomWritesOnceAndProvesReadback exercises the production
// write boundary, including a response loss and replaced native identities.
func TestV1ScratchHeadroomWritesOnceAndProvesReadback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writes    int
		readbacks int
		wantError bool
	}{
		{name: "applied", writes: 1, readbacks: 1},
		{name: "already available"},
		{name: "uncertain response", writes: 1, wantError: true},
		{name: "replaced volume", writes: 1, readbacks: 1, wantError: true},
		{name: "replaced peer", writes: 1, readbacks: 1, wantError: true},
		{name: "stale source before patch", wantError: true},
		{name: "unexpected overprovisioning", wantError: true},
		{name: "V2 enabled", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newPreparationAPI(t, "draining")
			f.v2Off = tc.name != "V2 enabled"
			f.s = v1ReservationFixture(true)
			f.storage = f.s.storageNodes[0]
			if tc.name == "already available" {
				valueObject(f.storage, "spec", "disks", "default-disk-fixture")["storageReserved"] = float64(29 * v1FixtureGiB)
			}
			writes, readbacks, sourceChecks := 0, 0, 0
			p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					sourceChecks++
					if tc.name == "stale source before patch" && sourceChecks > 1 {
						return []byte(strings.Repeat("a", 40) + "\trefs/heads/main\n"), nil
					}
				}
				if program == "kubectl" && len(args) > 4 && args[2] == "get" && args[4] == "storage-over-provisioning-percentage" && tc.name == "unexpected overprovisioning" {
					setting := resource(args[4])
					setting["value"] = "200"
					return f.encode(setting)
				}
				if program == "kubectl" && len(args) > 4 && args[2] == "patch" {
					writes++
					var changes []object
					want := []object{
						{"op": "test", "path": "/metadata/uid", "value": text(f.storage, "metadata", "uid")},
						{"op": "test", "path": "/metadata/resourceVersion", "value": text(f.storage, "metadata", "resourceVersion")},
						{"op": "replace", "path": "/spec/disks/default-disk-fixture/storageReserved", "value": float64(29 * v1FixtureGiB)},
					}
					if !reflect.DeepEqual(args[2:5], []string{"patch", "nodes.longhorn.io", v1RepairWorker}) ||
						json.Unmarshal(input, &changes) != nil || !reflect.DeepEqual(changes, want) {
						t.Fatal("scratch operation did not write the single conditional reservation patch")
					}
					valueObject(f.storage, "spec", "disks", "default-disk-fixture")["storageReserved"] = float64(29 * v1FixtureGiB)
					if tc.name == "uncertain response" {
						return nil, errors.New("patch response lost")
					}
					if tc.name == "replaced volume" {
						valueObject(f.s.volumes[0], "metadata")["uid"] = "replacement-volume"
					}
					if tc.name == "replaced peer" {
						valueObject(f.s.storageNodes[1], "metadata")["uid"] = "replacement-peer"
					}
					return nil, nil
				}
				if writes > 0 && program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "nodes" {
					readbacks++
				}
				return f.run(ctx, input, program, args...)
			}
			err := reserveV1ScratchHeadroom(context.Background(), p.c, p.source)
			if (err != nil) != tc.wantError || writes != tc.writes || readbacks != tc.readbacks {
				t.Fatalf("scratch result err=%v writes=%d readbacks=%d", err, writes, readbacks)
			}
		})
	}
}

// TestV1ScratchHeadroomRequiresItsOwnFirstAttempt verifies dispatch isolation
// from the earlier replica repair, another worker, and workflow retries.
func TestV1ScratchHeadroomRequiresItsOwnFirstAttempt(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "reserve-v1-scratch"
	env["LONGHORN_V2_CONFIRM"] = "reserve-longhorn-v1-scratch"
	getenv := func(key string) string { return env[key] }
	if err := authorizeV1ScratchHeadroom(getenv, v1RepairWorker); err != nil {
		t.Fatal(err)
	}
	if authorizeV1ScratchHeadroom(getenv, "prod-worker-2") == nil || authorizeV1ReservationRepair(getenv, v1RepairWorker) == nil {
		t.Fatal("scratch dispatch authorized a different operation or worker")
	}
	env["GITHUB_RUN_ATTEMPT"] = "2"
	if authorizeV1ScratchHeadroom(getenv, v1RepairWorker) == nil {
		t.Fatal("scratch operation accepted a workflow rerun")
	}
}
