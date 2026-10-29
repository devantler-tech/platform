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
		v["status"] = object{"state": state, "robustness": health, "actualSize": "1048576"}
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

func TestEvacuatedStoragePreservesAttachedAndRetainedVolumes(t *testing.T) {
	if err := verifyEvacuated("target", evacuatedFixture()); err != nil {
		t.Fatal(err)
	}
}

func TestEvacuationRefusesUnregisteredStorageOnTheResetTarget(t *testing.T) {
	s := evacuatedFixture()
	orphan := resource("unregistered-copy")
	orphan["spec"] = object{"nodeID": "target", "type": "replica"}
	s.orphans = []object{orphan}
	if verifyEvacuated("target", s) == nil {
		t.Fatal("unregistered replica data could be erased without a recovery decision")
	}
	orphan["spec"].(object)["nodeID"] = "storage-1"
	if err := verifyEvacuated("target", s); err != nil {
		t.Fatal("an orphan on another node does not authorize or prevent resetting this empty target", err)
	}
}

func TestWipeRefusesUnprovenEvacuation(t *testing.T) {
	cases := map[string]func(*inventory){
		"replica still references target":       func(s *inventory) { s.replicas[0]["spec"].(object)["nodeID"] = "target" },
		"engine still running on target":        func(s *inventory) { s.engines[0]["spec"].(object)["nodeID"] = "target" },
		"one copy is missing":                   func(s *inventory) { s.replicas = s.replicas[1:] },
		"two copies share a node":               func(s *inventory) { s.replicas[0]["spec"].(object)["nodeID"] = "storage-2" },
		"failed copy retains healthy timestamp": func(s *inventory) { s.replicas[0]["spec"].(object)["failedAt"] = "2026-10-29T01:00:00Z" },
		"unhealthy storage node":                func(s *inventory) { s.storageNodes[0]["status"].(object)["conditions"] = []any{} },
		"unready Kubernetes node":               func(s *inventory) { s.nodes[1]["status"].(object)["conditions"] = []any{} },
		"disk has been replaced": func(s *inventory) {
			s.storageNodes[0]["status"].(object)["diskStatus"].(object)["data"].(object)["diskUUID"] = "replacement"
		},
		"disk is not ready": func(s *inventory) {
			s.storageNodes[0]["status"].(object)["diskStatus"].(object)["data"].(object)["conditions"] = []any{}
		},
		"engine has not admitted rebuilt copy": func(s *inventory) {
			s.engines[0]["status"].(object)["replicaModeMap"].(object)["active-replica-1"] = "WO"
		},
		"retained volume has only two good copies": func(s *inventory) { s.replicas[3]["spec"].(object)["healthyAt"] = "" },
		"retained copy is in an unknown state":     func(s *inventory) { s.replicas[3]["status"].(object)["currentState"] = "unknown" },
		"inactive retained leftover":               func(s *inventory) { s.replicas[3]["spec"].(object)["active"] = false },
		"retained copy belongs to an old engine":   func(s *inventory) { s.replicas[3]["spec"].(object)["engineName"] = "old-engine" },
		"retained engine is inactive":              func(s *inventory) { s.engines[1]["spec"].(object)["active"] = false },
		"retained engine is missing":               func(s *inventory) { s.engines = s.engines[:1] },
		"volume is faulted":                        func(s *inventory) { s.volumes[0]["status"].(object)["robustness"] = "faulted" },
		"missing volume inventory":                 func(s *inventory) { s.volumes = nil },
		"terminating copy":                         func(s *inventory) { s.replicas[0]["metadata"].(object)["deletionTimestamp"] = "2026-10-29T01:00:00Z" },
		"active claim consumer remains": func(s *inventory) {
			p := resource("consumer")
			p["spec"] = object{"nodeName": "target", "volumes": []any{object{"persistentVolumeClaim": object{"claimName": "data"}}}}
			p["status"] = object{"phase": "Running"}
			s.pods = append(s.pods, p)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := evacuatedFixture()
			mutate(&s)
			if err := verifyEvacuated("target", s); err == nil {
				t.Fatal("unsafe node wipe was accepted")
			}
		})
	}
}
