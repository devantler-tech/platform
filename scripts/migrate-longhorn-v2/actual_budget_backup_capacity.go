package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const (
	prepareActualBudgetScratch = "prepare-actual-budget-backup-scratch"
	retireActualBudgetScratch  = "retire-actual-budget-backup-scratch"
	actualBudgetScratchOwner   = "storage.platform.devantler.tech/actual-budget-backup-scratch"
	actualBudgetScratchPath    = "/metadata/annotations/storage.platform.devantler.tech~1actual-budget-backup-scratch"
)

type actualBudgetScratchReservation struct {
	Source, Run, NodeUID, StorageUID, DiskUUID, Disk string
	Original, Temporary                              int64
}

func authorizeActualBudgetScratch(getenv func(string) string) (string, error) {
	operation := getenv("LONGHORN_V2_OPERATION")
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	value, ok := strings.CutPrefix(confirmation, operation+":")
	if !ok || getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" ||
		!validBenchmarkOwner(getenv("GITHUB_RUN_ID"), getenv("GITHUB_SHA")) ||
		operation == prepareActualBudgetScratch && !retentionDigest.MatchString(value) ||
		operation == retireActualBudgetScratch && (!validBenchmarkOwner(value, getenv("GITHUB_SHA")) || value == getenv("GITHUB_RUN_ID")) ||
		operation != prepareActualBudgetScratch && operation != retireActualBudgetScratch {
		return "", errors.New("the fixed Actual Budget staging reservation and exact confirmation are required")
	}
	return value, authorizeWorkflow(getenv, confirmation)
}

func readActualBudgetScratchState(ctx context.Context, c client) (inventory, map[string]object, map[string]object, error) {
	backups, err := c.list(ctx, "backups.velero.io", "velero")
	if err != nil || !settledActualBudgetBackupWindow(backups) {
		return inventory{}, nil, nil, errors.New("a Velero backup is unfinished or unverified")
	}
	s, err := observeV1BackupStagingInventory(ctx, c)
	if err != nil {
		return inventory{}, nil, nil, err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return inventory{}, nil, nil, err
	}
	storage, err := indexed(s.storageNodes)
	return s, nodes, storage, err
}

func actualBudgetScratchDisk(node, storage object) (string, int64, object, bool) {
	if !ready(valueObject(node, "status")) || value(node, "spec", "unschedulable") == true ||
		text(node, "metadata", "annotations", fenceAnnotation) != "" ||
		!isolationUID.MatchString(text(node, "metadata", "uid")) ||
		!isolationUID.MatchString(text(storage, "metadata", "uid")) ||
		text(storage, "metadata", "resourceVersion") == "" {
		return "", 0, nil, false
	}
	return v1RepairDisk(storage)
}

func prepareActualBudgetReservation(ctx context.Context, c client, source, run, digest string) error {
	if _, err := observeActualBudgetStaging(ctx, c); err != nil {
		return err
	}
	if _, _, err := observeActualBudgetBackupSource(ctx, c, digest); err != nil {
		return err
	}
	s, nodes, storage, err := readActualBudgetScratchState(ctx, c)
	if err != nil {
		return err
	}
	for _, worker := range v1BackupWorkers {
		if value(storage[worker], "metadata", "annotations", actualBudgetScratchOwner) != nil {
			return errors.New("the existing backup scratch reservation requires explicit retirement")
		}
	}
	var selected object
	var reservation actualBudgetScratchReservation
	for _, worker := range v1BackupWorkers {
		lh := storage[worker]
		key, current, disk, ok := actualBudgetScratchDisk(nodes[worker], lh)
		if !ok {
			continue
		}
		target, fit := actualBudgetRequiredReservation(disk, current, s.replicas, worker)
		if !fit {
			continue
		}
		if target == current {
			return nil
		}
		if selected == nil || current-target < reservation.Original-reservation.Temporary {
			selected, reservation = lh, actualBudgetScratchReservation{Source: source, Run: run,
				NodeUID: text(nodes[worker], "metadata", "uid"), StorageUID: text(lh, "metadata", "uid"),
				DiskUUID: text(disk, "diskUUID"), Disk: key, Original: current, Temporary: target}
		}
	}
	if selected == nil {
		return errors.New("no qualified original disk can open the measured snapshot-copy shortfall")
	}
	annotation, err := json.Marshal(reservation)
	if err != nil {
		return err
	}
	marker := object{"op": "add", "path": actualBudgetScratchPath, "value": string(annotation)}
	if value(selected, "metadata", "annotations") == nil {
		marker = object{"op": "add", "path": "/metadata/annotations", "value": object{actualBudgetScratchOwner: string(annotation)}}
	}
	return updateActualBudgetScratch(ctx, c, source, selected, reservation.Disk, reservation.Temporary, marker, string(annotation))
}

func retireActualBudgetReservation(ctx context.Context, c client, source, originalRun string) error {
	s, nodes, storage, err := readActualBudgetScratchState(ctx, c)
	if err != nil {
		return err
	}
	var selected object
	var reservation actualBudgetScratchReservation
	for _, worker := range v1BackupWorkers {
		lh := storage[worker]
		raw := text(lh, "metadata", "annotations", actualBudgetScratchOwner)
		if raw == "" && value(lh, "metadata", "annotations", actualBudgetScratchOwner) == nil {
			continue
		}
		key, current, disk, ok := actualBudgetScratchDisk(nodes[worker], lh)
		if selected != nil || !ok || !decodeActualBudgetScratchReservation(raw, &reservation) ||
			reservation.Run != originalRun || reservation.Disk != key || reservation.Temporary != current ||
			reservation.NodeUID != text(nodes[worker], "metadata", "uid") ||
			reservation.StorageUID != text(lh, "metadata", "uid") || reservation.DiskUUID != text(disk, "diskUUID") {
			return errors.New("the exact original backup staging reservation is unverified")
		}
		maximum, maxOK := v1CapacityBytes(disk["storageMaximum"])
		scheduled, scheduledOK := v1CapacityBytes(disk["storageScheduled"])
		if !maxOK || !scheduledOK || maximum-scheduled < reservation.Original ||
			!v1DiskLedgerMatches(s.replicas, worker, disk, maximum, scheduled) {
			return errors.New("restoring the original reservation would overcommit the current disk")
		}
		selected = lh
	}
	if selected == nil {
		return errors.New("the confirmed backup reservation is absent")
	}
	previous, err := readFinishedMigrationRun(ctx, c, reservation.Run, reservation.Source)
	if err != nil || previous.Attempt != 1 {
		return errors.New("the original reservation workflow has no terminal first attempt")
	}
	marker := object{"op": "remove", "path": actualBudgetScratchPath}
	return updateActualBudgetScratch(ctx, c, source, selected, reservation.Disk, reservation.Original, marker, "")
}

func decodeActualBudgetScratchReservation(raw string, reservation *actualBudgetScratchReservation) bool {
	fields := map[string][]string{"": {"Source", "Run", "NodeUID", "StorageUID", "DiskUUID", "Disk", "Original", "Temporary"}}
	return len(raw) > 0 && len(raw) <= 2048 && strictjson.Validate([]byte(raw), fields) == nil &&
		decodeIsolationJSON([]byte(raw), reservation) == nil && validBenchmarkOwner(reservation.Run, reservation.Source) &&
		reservation.Temporary > 0 && reservation.Original > reservation.Temporary &&
		reservation.Original-reservation.Temporary <= 2<<30
}

func updateActualBudgetScratch(ctx context.Context, c client, source string, original object, disk string, reserve int64, marker object, expectedMarker string) error {
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	change := object{"op": "replace", "path": "/spec/disks/" + disk + "/storageReserved", "value": reserve}
	if err := c.patch(ctx, "nodes.longhorn.io", "longhorn-system", original, change, marker); err != nil {
		return errors.New("the reservation write is unconfirmed and must be read before retry")
	}
	got, err := c.get(ctx, "nodes.longhorn.io", "longhorn-system", text(original, "metadata", "name"), false)
	if err != nil || benchmarkIdentity(got) != benchmarkIdentity(original) ||
		text(got, "status", "diskStatus", disk, "diskUUID") != text(original, "status", "diskStatus", disk, "diskUUID") ||
		value(got, "spec", "disks", disk, "storageReserved") != float64(reserve) ||
		text(got, "metadata", "annotations", actualBudgetScratchOwner) != expectedMarker ||
		(expectedMarker == "" && value(got, "metadata", "annotations", actualBudgetScratchOwner) != nil) {
		return errors.New("the reservation change was not confirmed by the original storage node")
	}
	return nil
}

func runActualBudgetScratch(ctx context.Context, c client, operation, source, run, confirmation string) error {
	if c.context != "admin@prod" || c.run == nil || !validBenchmarkOwner(run, source) {
		return errors.New("the protected Actual Budget staging reservation is unverified")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	switch operation {
	case prepareActualBudgetScratch:
		return prepareActualBudgetReservation(ctx, c, source, run, confirmation)
	case retireActualBudgetScratch:
		return retireActualBudgetReservation(ctx, c, source, confirmation)
	default:
		return errors.New("the staging reservation action is unsupported")
	}
}
