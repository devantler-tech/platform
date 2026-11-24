package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestBackupStagingOnlyReservesTheMeasuredTenGiBShortfall(t *testing.T) {
	for _, mode := range []string{"ready", "spare stays reserved", "thin physical margin", "unknown allocation", "replacement worker"} {
		t.Run(mode, func(t *testing.T) {
			state := v1ReservationFixture(true)
			for i, node := range state.storageNodes {
				name := "default-disk"
				if i == 0 {
					name = "default-disk-fixture"
				}
				valueObject(node, "status", "diskStatus", name)["storageAvailable"] = float64(60 * v1FixtureGiB)
			}
			third := valueObject(state.storageNodes[2], "spec", "disks", "default-disk")
			switch mode {
			case "spare stays reserved":
				third["storageReserved"] = float64(19 * v1FixtureGiB)
			case "thin physical margin":
				valueObject(state.storageNodes[2], "status", "diskStatus", "default-disk")["storageAvailable"] = float64(51 * v1FixtureGiB)
			case "unknown allocation":
				delete(valueObject(state.storageNodes[2], "status", "diskStatus", "default-disk"), "scheduledReplica")
			case "replacement worker":
				valueObject(state.nodes[2], "metadata")["uid"] = ""
			}
			plan, err := planV1BackupHeadroom(state)
			if mode != "ready" && mode != "spare stays reserved" {
				if err == nil {
					t.Fatal("incomplete or insufficient staging capacity was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for i, disk := range plan.disks {
				before := disk.maximum - disk.scheduled - disk.currentReserve
				after := disk.maximum - disk.scheduled - disk.reserve
				if after != max(before, int64(10*v1FixtureGiB)) || disk.reserve > disk.currentReserve ||
					i == 2 && mode == "spare stays reserved" && disk.reserve != disk.currentReserve {
					t.Fatal("the plan adjusted more than the measured staging shortfall")
				}
			}
		})
	}
}

func TestBackupHeadroomStopsOnActivityOrUnconfirmedWrite(t *testing.T) {
	for _, mode := range []string{"complete", "active backup", "active upload", "lost patch reply", "disk replaced after write"} {
		t.Run(mode, func(t *testing.T) {
			f, preparation := newPreparationAPI(t, "draining")
			f.v2Off = true
			f.s = v1ReservationFixture(true)
			for _, node := range f.s.storageNodes[1:] {
				valueObject(node, "spec", "disks", "default-disk")["storageReserved"] = float64(28 * v1FixtureGiB)
			}
			expected, err := planV1BackupHeadroom(f.s)
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			preparation.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "backups.velero.io" {
					phase := "Completed"
					if mode == "active backup" {
						phase = "InProgress"
					}
					backup := resource("daily-backup")
					valueObject(backup, "metadata")["namespace"] = "velero"
					backup["status"] = object{"phase": phase}
					return f.encode(object{"items": []object{backup}})
				}
				if program == "kubectl" && len(args) > 3 && args[2] == "get" && args[3] == "datauploads.velero.io" {
					uploads := []object{}
					if mode == "active upload" {
						upload := resource("backup-upload")
						valueObject(upload, "metadata")["namespace"] = "velero"
						upload["status"] = object{"phase": "Accepted"}
						uploads = append(uploads, upload)
					}
					return f.encode(object{"items": uploads})
				}
				if program == "kubectl" && len(args) > 4 && args[2] == "patch" {
					if writes >= len(expected.disks) || args[3] != "nodes.longhorn.io" ||
						args[4] != text(expected.disks[writes].node, "metadata", "name") {
						t.Fatal("write escaped the verified three-disk plan")
					}
					disk := expected.disks[writes]
					var changes []object
					if json.Unmarshal(input, &changes) != nil || len(changes) != 3 ||
						text(changes[0], "path") != "/metadata/uid" || text(changes[1], "path") != "/metadata/resourceVersion" ||
						text(changes[2], "path") != "/spec/disks/"+disk.disk+"/storageReserved" ||
						value(changes[2], "value") != float64(disk.reserve) {
						t.Fatal("backup staging exceeded the conditional reservation patch")
					}
					valueObject(disk.node, "spec", "disks", disk.disk)["storageReserved"] = float64(disk.reserve)
					writes++
					switch mode {
					case "lost patch reply":
						return nil, errors.New("patch response lost")
					case "disk replaced after write":
						valueObject(disk.node, "status", "diskStatus", disk.disk)["diskUUID"] = "replacement"
					}
					return nil, nil
				}
				return f.run(ctx, input, program, args...)
			}
			err = reserveV1BackupHeadroom(t.Context(), preparation.c, preparation.source)
			wantWrites := map[string]int{"complete": 3, "active backup": 0, "active upload": 0, "lost patch reply": 1, "disk replaced after write": 1}
			if (err == nil) != (mode == "complete") || writes != wantWrites[mode] {
				t.Fatalf("unexpected backup headroom result: writes=%d err=%v", writes, err)
			}
		})
	}
}

func TestBackupHeadroomRequiresItsOwnMainDispatchWithoutNodeInput(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "reserve-v1-backup-scratch"
	env["LONGHORN_V2_CONFIRM"] = "reserve-longhorn-v1-backup-scratch"
	delete(env, "LONGHORN_V2_NODE")
	getenv := func(key string) string { return env[key] }
	if err := authorizeV1BackupHeadroom(getenv); err != nil {
		t.Fatal(err)
	}
	env["LONGHORN_V2_NODE"] = v1RepairWorker
	if authorizeV1BackupHeadroom(getenv) == nil {
		t.Fatal("the backup reservation accepted an input-selected worker")
	}
	delete(env, "LONGHORN_V2_NODE")
	env["GITHUB_RUN_ATTEMPT"] = "2"
	if authorizeV1BackupHeadroom(getenv) == nil {
		t.Fatal("the backup reservation accepted a workflow rerun")
	}
}
