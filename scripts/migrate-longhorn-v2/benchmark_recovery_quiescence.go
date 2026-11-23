package main

import (
	"errors"
	"time"
)

func absentOrEmptyString(v any) bool { return v == nil || v == "" }

func absentOrEmptyMap(v any) bool {
	if v == nil {
		return true
	}
	m, ok := v.(object)
	return ok && len(m) == 0
}

// This is a necessary quiescence check, not deletion authorization. The caller
// still needs complete native inventories, original UID provenance, dependency
// checks, mutation fences and recovery ownership at each destructive boundary.
func verifyRecoveredBenchmarkDetached(volume object, attachments, engines, frontends []object) error {
	name := text(volume, "metadata", "name")
	if attachments == nil || engines == nil || frontends == nil {
		return errors.New("complete detached backend observations are required")
	}
	if err := verifyBenchmarkDetachedBackendState(volume); err != nil {
		return err
	}
	seen := map[string]bool{text(volume, "metadata", "uid"): true}
	for group, items := range [][]object{attachments, engines, frontends} {
		for _, item := range items {
			uid := text(item, "metadata", "uid")
			association := text(item, "spec", "volumeName")
			if group == 0 {
				association = text(item, "spec", "volume")
			}
			if uid == "" || seen[uid] || text(item, "metadata", "name") == "" || text(item, "metadata", "resourceVersion") == "" || association == "" {
				return errors.New("backend instance observations have incomplete or ambiguous identities")
			}
			seen[uid] = true
			if association != name {
				continue
			}
			if group == 0 {
				if !absentOrEmptyMap(value(item, "spec", "attachmentTickets")) || !absentOrEmptyMap(value(item, "status", "attachmentTicketStatuses")) {
					return errors.New("backend attachment tickets remain, including pending requests")
				}
			} else if text(item, "spec", "desireState") != "stopped" || text(item, "status", "currentState") != "stopped" {
				return errors.New("backend engine or frontend has not fully stopped")
			}
		}
	}
	return nil
}

func verifyBenchmarkDetachedBackendState(volume object) error {
	if text(volume, "metadata", "name") == "" || text(volume, "metadata", "uid") == "" ||
		text(volume, "metadata", "resourceVersion") == "" || text(volume, "status", "state") != "detached" {
		return errors.New("complete detached backend observations are required")
	}
	for _, key := range []string{"nodeID", "engineNodeID", "migrationNodeID"} {
		if !absentOrEmptyString(value(volume, "spec", key)) {
			return errors.New("backend still has a desired attachment or migration target")
		}
	}
	for _, key := range []string{"currentNodeID", "currentEngineNodeID", "currentMigrationNodeID", "switchoverState"} {
		if !absentOrEmptyString(value(volume, "status", key)) {
			return errors.New("backend attachment or switchover remains observed")
		}
	}
	return nil
}

// Longhorn's native deletion controller follows these stored names without
// original Kubernetes UID preconditions. Natural clearance is necessary, but
// it is not a lasting fence: its PV controller can repopulate status after a
// Volume DELETE. Do not add a backend DELETE entrypoint without resolving that
// reference-creation race as well as all other retirement gates.
func verifyRecoveredBackendCascadeCleared(volume object) error {
	status, ok := value(volume, "status", "kubernetesStatus").(object)
	if !ok || text(volume, "metadata", "uid") == "" || text(volume, "metadata", "resourceVersion") == "" {
		return errors.New("native backend Kubernetes status is unverified")
	}
	for _, key := range []string{"pvName", "pvcName", "namespace", "lastPVCRefAt"} {
		if v := status[key]; v != nil {
			if _, ok := v.(string); !ok {
				return errors.New("backend Kubernetes status contains an ambiguous reference")
			}
		}
	}
	if !absentOrEmptyString(status["pvName"]) {
		return errors.New("backend still names a Kubernetes PV for cascading deletion")
	}
	if !absentOrEmptyString(status["pvcName"]) {
		if _, err := time.Parse(time.RFC3339, text(status, "lastPVCRefAt")); err != nil {
			return errors.New("backend PVC reference has not been naturally retired")
		}
	}
	return nil
}
