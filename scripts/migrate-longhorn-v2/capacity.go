package main

import (
	"errors"
	"fmt"
	"strings"
)

func verifyCapacityRoster(nodes []object, pools map[string]migrationPool) error {
	byName, err := indexed(nodes)
	if err != nil {
		return fmt.Errorf("migration node inventory is unusable: %w", err)
	}
	if len(byName) != 9 || len(pools) != 2 {
		return errors.New("migration requires three static workers, three pinned migration workers and three control planes; compute overflow must be retired first")
	}
	for _, name := range []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"} {
		if byName[name] == nil {
			return errors.New("temporary worker capacity is incomplete")
		}
	}
	controlPlanes := 0
	migrationCounts := map[string]int{}
	for name, n := range byName {
		status, _ := value(n, "status").(map[string]any)
		if !ready(status) {
			return errors.New("every migration node must be Ready")
		}
		if strings.HasPrefix(name, "prod-worker-") {
			continue
		}
		role := text(n, "metadata", "labels", migrationPoolLabel)
		if pool, ok := pools[role]; ok {
			if err = verifyMigrationNode(n, pool); err != nil {
				return fmt.Errorf("migration node %s failed verification: %w", name, err)
			}
			migrationCounts[role]++
			continue
		}
		labels, _ := value(n, "metadata", "labels").(map[string]any)
		if _, ok := labels["node-role.kubernetes.io/control-plane"]; !ok || !strings.HasPrefix(name, "prod-control-plane-") {
			return errors.New("unknown or compute-overflow node remains during migration")
		}
		controlPlanes++
	}
	if controlPlanes != 3 || migrationCounts["v1"] != 2 || migrationCounts["v2"] != 1 {
		return errors.New("ready control planes and fixed migration-pool capacity are incomplete")
	}
	return nil
}
