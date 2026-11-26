package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestActualBudgetStagingChecksEveryPossibleReplicaPlacement(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    func(*inventory)
		wantError string
	}{
		{"all eligible disks have the margin", func(*inventory) {}, ""},
		{"second eligible disk is below the margin", func(s *inventory) {
			valueObject(s.storageNodes[1], "status", "diskStatus", "default-disk")["storageAvailable"] = float64(50 * v1FixtureGiB)
		}, "an eligible V1 disk cannot preserve the physical floor and margin"},
		{"a disk below the margin cannot fit the requested size", func(s *inventory) {
			valueObject(s.storageNodes[1], "status", "diskStatus", "default-disk")["storageAvailable"] = float64(50 * v1FixtureGiB)
			valueObject(s.storageNodes[1], "spec", "disks", "default-disk")["storageReserved"] = float64(24 * v1FixtureGiB)
		}, ""},
		{"a second disk has an unknown allocation ledger", func(s *inventory) {
			delete(valueObject(s.storageNodes[1], "status", "diskStatus", "default-disk"), "scheduledReplica")
		}, "the V1 disk allocation is unverified"},
		{"an unregistered filesystem disk could receive the clone", func(s *inventory) {
			node := resource("unregistered-storage")
			node["spec"] = object{"allowScheduling": true, "disks": object{"other": object{
				"diskType": "filesystem", "allowScheduling": true,
			}}}
			s.storageNodes = append(s.storageNodes, node)
		}, "an unqualified Longhorn disk could receive the V1 staging replica"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, p := newPreparationAPI(t, "draining")
			f.s = v1ReservationFixture(true)
			for i, node := range f.s.nodes {
				valueObject(node, "metadata")["uid"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
				valueObject(f.s.storageNodes[i], "metadata")["uid"] = fmt.Sprintf("00000000-0000-4000-8001-%012d", i+1)
			}
			test.change(&f.s)
			p.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "datauploads.velero.io" {
					return f.encode(object{"items": []object{}})
				}
				return f.run(ctx, input, program, args...)
			}
			err := verifyActualBudgetStagingCapacity(t.Context(), p.c)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != test.wantError {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestActualBudgetReservationOpensOnlyTheObservedShortfall(t *testing.T) {
	const gib = int64(1 << 30)
	const worker = "test-worker"
	const size = "129922760704"
	replica := object{"metadata": object{"name": "existing-replica"}, "spec": object{
		"nodeID": worker, "diskID": "native-disk", "dataEngine": "v1", "volumeSize": size,
	}}
	for _, tt := range []struct {
		name                string
		available, reserved int64
		wantReserve         int64
		wantFit             bool
	}{
		{"two GiB shortfall", 53 * gib, 19 * gib, 17 * gib, true},
		{"already has room", 53 * gib, 16 * gib, 16 * gib, true},
		{"already has room with zero reserve", 53 * gib, 0, 0, true},
		{"more than the qualified shortfall", 53 * gib, 20 * gib, 0, false},
		{"physical margin exhausted", 50 * gib, 19 * gib, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			disk := object{"diskUUID": "native-disk", "storageMaximum": float64(148 * gib),
				"storageAvailable": float64(tt.available), "storageScheduled": float64(121 * gib),
				"scheduledReplica": object{"existing-replica": float64(121 * gib)}}
			reserve, fit := actualBudgetRequiredReservation(disk, tt.reserved, []object{replica}, worker)
			if fit != tt.wantFit || reserve != tt.wantReserve {
				t.Fatalf("reserve=%d fit=%t, want %d %t", reserve, fit, tt.wantReserve, tt.wantFit)
			}
		})
	}
}

func TestActualBudgetReservationAdmitsOnlyAQualifiedProjectedPlacement(t *testing.T) {
	for _, mode := range []string{"fits already", "one safe adjustment", "unsafe eligible peer"} {
		t.Run(mode, func(t *testing.T) {
			s := v1ReservationFixture(true)
			for i, node := range s.nodes {
				valueObject(node, "metadata")["uid"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
				valueObject(s.storageNodes[i], "metadata")["uid"] = fmt.Sprintf("00000000-0000-4000-8001-%012d", i+1)
			}
			first := valueObject(s.storageNodes[0], "spec", "disks", "default-disk-fixture")
			first["storageReserved"] = float64(25 * v1FixtureGiB)
			if mode != "fits already" {
				valueObject(s.storageNodes[1], "status", "diskStatus", "default-disk")["storageAvailable"] = float64(50 * v1FixtureGiB)
				valueObject(s.storageNodes[2], "spec", "disks", "default-disk")["storageReserved"] = float64(26 * v1FixtureGiB)
			}
			if mode == "one safe adjustment" {
				valueObject(s.storageNodes[1], "spec", "disks", "default-disk")["storageReserved"] = float64(24 * v1FixtureGiB)
			}
			nodes, err := indexed(s.nodes)
			if err != nil {
				t.Fatal(err)
			}
			storage, err := indexed(s.storageNodes)
			if err != nil {
				t.Fatal(err)
			}
			selected, plan, err := planActualBudgetReservation(s, nodes, storage, strings.Repeat("a", 40), "1234")
			switch mode {
			case "fits already":
				if err != nil || selected != nil {
					t.Fatalf("existing capacity must remain unchanged: %v", err)
				}
			case "one safe adjustment":
				if err != nil || text(selected, "metadata", "name") != "prod-worker-1" ||
					plan.Original-plan.Temporary != 2*v1FixtureGiB || first["storageReserved"] != float64(25*v1FixtureGiB) {
					t.Fatalf("the qualified projection modified its observed source or did not bind the existing disk: %v", err)
				}
			case "unsafe eligible peer":
				if err == nil || selected != nil {
					t.Fatal("preparation admitted a peer whose physical margin cannot support the clone")
				}
			}
		})
	}
}

func TestActualBudgetReservationRetirementBindsFirstAndLatestAttempts(t *testing.T) {
	const run = "1234"
	const source = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const path = ".github/workflows/prepare-longhorn-v2-node.yaml"
	for _, mode := range []string{"completed later attempt", "active later attempt", "invalid original attempt"} {
		t.Run(mode, func(t *testing.T) {
			var reads []string
			c := client{run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				if program != "gh" || len(args) != 2 || args[0] != "api" {
					t.Fatal("reservation run preflight requested an unexpected command")
				}
				reads = append(reads, args[1])
				status, revision, attempt := "completed", source, 2
				switch args[1] {
				case "repos/devantler-tech/platform/actions/runs/" + run:
					if mode == "active later attempt" {
						status = "in_progress"
					}
				case "repos/devantler-tech/platform/actions/runs/" + run + "/attempts/1":
					attempt = 1
					if mode == "invalid original attempt" {
						revision = strings.Repeat("b", 40)
					}
				default:
					t.Fatal("reservation retirement requested an unrelated workflow receipt")
				}
				return json.Marshal(object{"id": 1234, "status": status, "head_sha": revision, "head_branch": "main",
					"event": "workflow_dispatch", "run_attempt": attempt, "path": path, "conclusion": "failure"})
			}}
			err := verifyActualBudgetReservationRun(t.Context(), c, actualBudgetScratchReservation{Run: run, Source: source})
			if (err == nil) != (mode == "completed later attempt") ||
				len(reads) != map[bool]int{true: 1, false: 2}[mode == "active later attempt"] {
				t.Fatalf("reservation run proof: reads=%d err=%v", len(reads), err)
			}
		})
	}
}

func TestActualBudgetReservationRequiresItsOwnMainConfirmation(t *testing.T) {
	env := dispatchFixture()
	delete(env, "LONGHORN_V2_NODE")
	for _, operation := range []string{prepareActualBudgetScratch, retireActualBudgetScratch} {
		confirmation := strings.Repeat("a", 64)
		if operation == retireActualBudgetScratch {
			confirmation = "2345"
		}
		env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = operation, operation+":"+confirmation
		getenv := func(key string) string { return env[key] }
		if value, err := authorizeActualBudgetScratch(getenv); err != nil || value != confirmation {
			t.Fatalf("valid protected %s confirmation was refused: %v", operation, err)
		}
		for key, replacement := range map[string]string{
			"GITHUB_RUN_ATTEMPT": "2", "LONGHORN_V2_NODE": "prod-worker-2", "LONGHORN_V2_DATABASE": "umami-db",
			"GITHUB_REF": "refs/heads/staging", "LONGHORN_V2_CONFIRM": operation + ":1234",
		} {
			original := env[key]
			env[key] = replacement
			if _, err := authorizeActualBudgetScratch(getenv); err == nil {
				t.Fatalf("accepted invalid %s for %s", key, operation)
			}
			env[key] = original
		}
	}
}

func TestActualBudgetReservationWritesItsOwnerAndReserveTogether(t *testing.T) {
	for _, outcome := range []string{"acknowledged", "lost reply", "disk replaced"} {
		t.Run(outcome, func(t *testing.T) {
			const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const disk = "default-disk"
			const reservation = "one reviewed reservation"
			before := object{"metadata": object{"name": "existing-worker", "namespace": "longhorn-system",
				"uid": "original-storage-uid", "resourceVersion": "100"},
				"status": object{"diskStatus": object{disk: object{"diskUUID": "original-disk"}}}}
			changed := object{"metadata": object{"name": "existing-worker", "namespace": "longhorn-system",
				"uid": "original-storage-uid", "resourceVersion": "101", "annotations": object{actualBudgetScratchOwner: reservation}},
				"spec":   object{"disks": object{disk: object{"storageReserved": float64(17 << 30)}}},
				"status": object{"diskStatus": object{disk: object{"diskUUID": "original-disk"}}}}
			if outcome == "disk replaced" {
				valueObject(changed, "status", "diskStatus", disk)["diskUUID"] = "replacement-disk"
			}
			writes, reads := 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, data []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(sha + "\trefs/heads/main\n"), nil
				}
				switch args[2] {
				case "patch":
					writes++
					var patch []object
					if err := json.Unmarshal(data, &patch); err != nil || len(patch) != 4 ||
						text(patch[0], "op") != "test" || text(patch[0], "path") != "/metadata/uid" || text(patch[0], "value") != "original-storage-uid" ||
						text(patch[1], "op") != "test" || text(patch[1], "path") != "/metadata/resourceVersion" || text(patch[1], "value") != "100" ||
						text(patch[2], "op") != "replace" || text(patch[2], "path") != "/spec/disks/default-disk/storageReserved" || value(patch[2], "value") != float64(17<<30) ||
						text(patch[3], "op") != "add" || text(patch[3], "path") != actualBudgetScratchPath || text(patch[3], "value") != reservation {
						t.Fatal("the reserve and its owner must be protected by one exact CAS")
					}
					if outcome == "lost reply" {
						return nil, errors.New("lost patch reply")
					}
					return nil, nil
				case "get":
					reads++
					return json.Marshal(changed)
				default:
					t.Fatal("the reservation escaped its exact API scope")
					return nil, nil
				}
			}}
			err := updateActualBudgetScratch(t.Context(), c, sha, before, disk, 17<<30,
				object{"op": "add", "path": actualBudgetScratchPath, "value": reservation}, reservation)
			if (err == nil) != (outcome == "acknowledged") || writes != 1 || (outcome == "lost reply" && reads != 0) {
				t.Fatalf("reservation result err=%v writes=%d reads=%d", err, writes, reads)
			}
		})
	}
}
