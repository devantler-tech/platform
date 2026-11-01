package main

import (
	"errors"
	"time"
)

type object = map[string]any

type inventory struct {
	nodes, storageNodes, volumes, replicas, engines, pods []object
}

func value(o object, path ...string) any {
	var v any = o
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func text(o object, path ...string) string {
	s, _ := value(o, path...).(string)
	return s
}

func ready(o object) bool {
	conditions, _ := value(o, "conditions").([]any)
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if ok && text(m, "type") == "Ready" && text(m, "status") == "True" {
			return true
		}
	}
	return false
}

func indexed(objects []object) (map[string]object, error) {
	result := map[string]object{}
	for _, o := range objects {
		name := text(o, "metadata", "name")
		if name == "" || text(o, "metadata", "uid") == "" || value(o, "metadata", "deletionTimestamp") != nil || result[name] != nil {
			return nil, errors.New("resource identity is incomplete, duplicated or terminating")
		}
		result[name] = o
	}
	return result, nil
}

func diskReady(n, replica object) bool {
	status, ok := value(n, "status").(map[string]any)
	if !ok || !ready(status) {
		return false
	}
	disks, _ := value(n, "spec", "disks").(map[string]any)
	statuses, _ := value(n, "status", "diskStatus").(map[string]any)
	for name, candidate := range statuses {
		diskStatus, ok := candidate.(map[string]any)
		disk, specOK := disks[name].(map[string]any)
		if ok && specOK && text(diskStatus, "diskUUID") == text(replica, "spec", "diskID") && text(diskStatus, "diskUUID") != "" &&
			text(disk, "path") == text(replica, "spec", "diskPath") && text(disk, "path") != "" && ready(diskStatus) {
			return true
		}
	}
	return false
}

func verifyRedundancy(s inventory) error {
	if len(s.volumes) == 0 {
		return errors.New("complete volume inventory is required")
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return err
	}
	for _, objects := range [][]object{s.volumes, s.replicas, s.engines} {
		if _, err = indexed(objects); err != nil {
			return err
		}
	}
	for _, v := range s.volumes {
		engine := text(v, "spec", "dataEngine")
		if engine != "v1" && engine != "v2" || value(v, "spec", "numberOfReplicas") != float64(3) {
			return errors.New("volume engine or required redundancy is outside the migration contract")
		}
		state, health := text(v, "status", "state"), text(v, "status", "robustness")
		suitable := state == "attached" && health == "healthy" || state == "detached" && (health == "healthy" || health == "unknown")
		if !suitable {
			return errors.New("volume health is not suitable for node maintenance")
		}
		var currentEngine object
		for _, e := range s.engines {
			if text(e, "spec", "volumeName") == text(v, "metadata", "name") && value(e, "spec", "active") == true {
				if currentEngine != nil {
					return errors.New("volume has multiple active engines")
				}
				currentEngine = e
			}
		}
		if currentEngine == nil || text(currentEngine, "spec", "dataEngine") != engine ||
			state == "attached" && text(currentEngine, "status", "currentState") != "running" ||
			state == "detached" && text(currentEngine, "status", "currentState") != "stopped" {
			return errors.New("the volume's current engine identity and state are not proven")
		}
		modes, _ := value(currentEngine, "status", "replicaModeMap").(map[string]any)
		if state == "attached" && modes == nil {
			return errors.New("attached volume has no live replica-mode evidence")
		}
		copies := map[string]bool{}
		for _, r := range s.replicas {
			if text(r, "spec", "volumeName") != text(v, "metadata", "name") || text(r, "spec", "failedAt") != "" || value(r, "spec", "active") != true ||
				text(r, "spec", "engineName") != text(currentEngine, "metadata", "name") || text(r, "spec", "dataEngine") != engine {
				continue
			}
			if _, err = time.Parse(time.RFC3339, text(r, "spec", "healthyAt")); err != nil {
				continue
			}
			node := text(r, "spec", "nodeID")
			nodeStatus, _ := value(nodes[node], "status").(map[string]any)
			if !ready(nodeStatus) || !diskReady(storage[node], r) {
				continue
			}
			current := text(r, "status", "currentState")
			if state == "attached" && (current != "running" || modes[text(r, "metadata", "name")] != "RW") ||
				state == "detached" && current != "stopped" {
				continue
			}
			copies[node] = true
		}
		if len(copies) < 3 {
			return errors.New("three healthy copies on distinct nodes are not proven for every volume")
		}
	}
	return nil
}
