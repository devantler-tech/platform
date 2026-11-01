package main

import (
	"fmt"
	"testing"
)

func resource(name string) object {
	return object{"metadata": object{"name": name, "uid": "uid-" + name, "resourceVersion": "1"}}
}
func readyConditions() []any {
	return []any{object{"type": "Ready", "status": "True"}}
}
func evacuatedFixture() inventory {
	s := inventory{}
	for _, name := range []string{"target", "storage-1", "storage-2", "storage-3"} {
		n := resource(name)
		n["status"] = object{"conditions": readyConditions()}
		s.nodes = append(s.nodes, n)
		if name == "target" {
			continue
		}
		n = resource(name)
		n["spec"] = object{"disks": object{"data": object{"path": "/data"}}}
		n["status"] = object{"conditions": readyConditions(), "diskStatus": object{"data": object{"diskUUID": "disk-" + name, "conditions": readyConditions()}}}
		s.storageNodes = append(s.storageNodes, n)
	}
	for _, name := range []string{"active", "retained"} {
		v := resource(name)
		v["spec"] = object{"dataEngine": "v1", "numberOfReplicas": float64(3), "size": "1073741824"}
		state, health, replicaState := "attached", "healthy", "running"
		if name == "retained" {
			state, health, replicaState = "detached", "unknown", "stopped"
		}
		v["status"] = object{"state": state, "robustness": health, "actualSize": float64(1048576)}
		s.volumes = append(s.volumes, v)
		modes := object{}
		for i := 1; i <= 3; i++ {
			node := fmt.Sprintf("storage-%d", i)
			r := resource(fmt.Sprintf("%s-replica-%d", name, i))
			r["spec"] = object{"volumeName": name, "engineName": name + "-engine", "dataEngine": "v1", "active": true, "nodeID": node, "diskID": "disk-" + node, "diskPath": "/data", "healthyAt": "2026-10-29T00:00:00Z", "failedAt": ""}
			r["status"] = object{"currentState": replicaState}
			s.replicas = append(s.replicas, r)
			modes[r["metadata"].(object)["name"].(string)] = "RW"
		}
		e := resource(name + "-engine")
		e["spec"] = object{"volumeName": name, "dataEngine": "v1", "active": true, "nodeID": "storage-1"}
		e["status"] = object{"currentState": replicaState, "replicaModeMap": modes}
		s.engines = append(s.engines, e)
	}
	return s
}

func TestRefreshRequiresThreeHealthyCopiesOfEveryVolume(t *testing.T) {
	if err := verifyRedundancy(evacuatedFixture()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*inventory){
		"empty volume inventory": func(s *inventory) { s.volumes = nil },
		"degraded volume":        func(s *inventory) { s.volumes[0]["status"].(object)["robustness"] = "degraded" },
		"two copies":             func(s *inventory) { s.replicas = s.replicas[1:] },
		"copy on a NotReady node": func(s *inventory) {
			s.nodes[1]["status"] = object{"conditions": []any{}}
		},
		"failed copy":         func(s *inventory) { s.replicas[0]["spec"].(object)["failedAt"] = "2026-11-01T00:00:00Z" },
		"copy not read-write": func(s *inventory) { s.engines[0]["status"].(object)["replicaModeMap"] = object{} },
		"no active engine":    func(s *inventory) { s.engines = s.engines[1:] },
		"two replicas wanted": func(s *inventory) { s.volumes[1]["spec"].(object)["numberOfReplicas"] = float64(2) },
	} {
		t.Run(name, func(t *testing.T) {
			s := evacuatedFixture()
			mutate(&s)
			if err := verifyRedundancy(s); err == nil {
				t.Fatal("refresh admitted without proven redundancy")
			}
		})
	}
}
