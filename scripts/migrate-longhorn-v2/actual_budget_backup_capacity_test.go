package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

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
						text(patch[0], "path") != "/metadata/uid" || text(patch[0], "value") != "original-storage-uid" ||
						text(patch[1], "path") != "/metadata/resourceVersion" || text(patch[1], "value") != "100" ||
						text(patch[2], "path") != "/spec/disks/default-disk/storageReserved" || value(patch[2], "value") != float64(17<<30) ||
						text(patch[3], "path") != actualBudgetScratchPath || text(patch[3], "value") != reservation {
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
