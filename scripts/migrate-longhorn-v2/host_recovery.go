package main

import (
	"context"
	"errors"
	"strings"
	"time"
)

// recoveredHost requires the original hardware, encrypted disk, OS and loaded
// reservation on a new boot, with native and Kubernetes huge-page capacity.
func (h hostMemoryReboot) recoveredHost(ctx context.Context, r hostRebootRecord) (object, error) {
	n, _, err := h.targetState(ctx, r)
	boot := text(n, "status", "nodeInfo", "bootID")
	if err != nil || !inventoryBoot.MatchString(boot) || boot == r.OriginalBootID || r.RecoveredBootID != "" && boot != r.RecoveredBootID || !hugepagesReady(n) {
		return nil, errHostMemory
	}
	memory, err := h.memory(ctx)
	if err != nil || memory.PersistentPages != 1024 || memory.TotalPages != 1024 || memory.PageSizeKiB != 2048 {
		return nil, errHostMemory
	}
	return n, nil
}

// recoveryPeers proves the two independent live copies and unchanged database
// quorums before restoring scheduling. This is not the three-copy acceptance
// check: the returning journal continues to block the next worker.
func (h hostMemoryReboot) recoveryPeers(ctx context.Context, r hostRebootRecord) error {
	setting, err := h.c.get(ctx, "settings.longhorn.io", "longhorn-system", "v2-data-engine", false)
	if err != nil || text(setting, "metadata", "name") != "v2-data-engine" || text(setting, "metadata", "namespace") != "longhorn-system" || text(setting, "value") != "false" || h.drainPolicy(ctx) != nil {
		return errHostMemory
	}
	s, err := h.c.snapshot(ctx)
	if err != nil || verifyCapacityRoster(s.nodes, h.pools, false) != nil || verifyHealthyCopies(s, 2, h.target) != nil || h.peersClear(ctx, s.nodes) != nil {
		return errHostMemory
	}
	for _, volume := range s.volumes {
		if text(volume, "spec", "dataEngine") != "v1" {
			return errHostMemory
		}
	}
	_, err = h.databases(ctx, r.Databases)
	if err != nil && !errors.Is(err, errHostDatabasesRecovering) {
		return err
	}
	return nil
}

// recover journals the verified new boot before returning the owned node to
// scheduling. Longhorn can then restart its instance manager. The marker and
// unfinished journal remain until full redundancy and database health return;
// neither a timeout nor a resumed invocation repeats the reboot.
func (h hostMemoryReboot) recover(ctx context.Context, r hostRebootRecord) error {
	recovery, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if r.Phase == "reboot-requested" {
		var host object
		if err := waitReadyWithin(recovery, 20*time.Minute, func(ctx context.Context) error {
			if _, err := h.owned(ctx, r); err != nil {
				return errors.Join(errReadinessTerminal, err)
			}
			var err error
			host, err = h.recoveredHost(ctx, r)
			if err != nil {
				return err
			}
			return h.recoveryPeers(ctx, r)
		}); err != nil {
			return err
		}
		cm, err := h.owned(recovery, r)
		if err != nil {
			return err
		}
		r.Phase, r.RecoveredBootID = "returning", text(host, "status", "nodeInfo", "bootID")
		if err = h.write(recovery, cm, r); err != nil {
			return err
		}
	}
	host, err := h.recoveredHost(recovery, r)
	if err != nil || h.recoveryPeers(recovery, r) != nil {
		return errHostMemory
	}
	if value(host, "spec", "unschedulable") == true {
		if r.Phase != "returning" || text(host, "metadata", "annotations", fenceAnnotation) != h.name() {
			return errHostMemory
		}
		if _, err = h.owned(recovery, r); err != nil {
			return err
		}
		if err = h.c.patch(recovery, "node", "", host, object{"op": "add", "path": "/spec/unschedulable", "value": false}); err != nil {
			return err
		}
	}
	if err = waitReadyWithin(recovery, 20*time.Minute, func(ctx context.Context) error {
		if _, err := h.owned(ctx, r); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		var err error
		host, err = h.recoveredHost(ctx, r)
		if err != nil || value(host, "spec", "unschedulable") == true {
			return errHostMemory
		}
		s, _, err := h.healthy(ctx, r.Databases)
		if err != nil {
			return err
		}
		return h.peersClear(ctx, s.nodes)
	}); err != nil {
		return err
	}
	if text(host, "metadata", "annotations", fenceAnnotation) == h.name() {
		if _, err = h.owned(recovery, r); err != nil {
			return err
		}
		if err = h.c.patch(recovery, "node", "", host, object{"op": "remove", "path": "/metadata/annotations/" + strings.ReplaceAll(fenceAnnotation, "/", "~1")}); err != nil {
			return err
		}
	}
	released, err := h.recoveredHost(recovery, r)
	if err != nil || value(released, "spec", "unschedulable") == true || text(released, "metadata", "annotations", fenceAnnotation) != "" {
		return errHostMemory
	}
	if r.Phase == "recovered" {
		return nil
	}
	cm, err := h.owned(recovery, r)
	if err != nil {
		return err
	}
	r.Phase = "recovered"
	return h.write(recovery, cm, r)
}
