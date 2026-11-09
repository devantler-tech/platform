package main

import (
	"fmt"
	"testing"
)

const v1FixtureGiB = int64(1 << 30)

func v1ReservationFixture() inventory {
	s := inventory{}
	for i := range 3 {
		name := fmt.Sprintf("prod-worker-%d", i+1)
		n := resource(name)
		n["status"] = object{"conditions": readyConditions()}
		s.nodes = append(s.nodes, n)
		storage := resource(name)
		storage["metadata"].(object)["namespace"] = "longhorn-system"
		reserve, scheduled := 22*v1FixtureGiB, 117*v1FixtureGiB
		diskName := "default-disk"
		if i == 0 {
			reserve, scheduled = 45*v1FixtureGiB, 102*v1FixtureGiB
			diskName = "default-disk-fixture"
		}
		disk := object{"path": "/var/lib/longhorn", "diskType": "filesystem", "allowScheduling": true, "evictionRequested": false, "storageReserved": float64(reserve)}
		storage["spec"] = object{"allowScheduling": true, "evictionRequested": false, "disks": object{diskName: disk}}
		conditions := append(readyConditions(), object{"type": "Schedulable", "status": "True"})
		status := object{"diskUUID": "disk-" + name, "conditions": conditions, "storageMaximum": float64(150 * v1FixtureGiB), "storageScheduled": float64(scheduled), "storageAvailable": float64(61 * v1FixtureGiB), "scheduledReplica": object{}}
		storage["status"] = object{"conditions": readyConditions(), "diskStatus": object{diskName: status}}
		s.storageNodes = append(s.storageNodes, storage)
	}
	for _, item := range []struct {
		name string
		size int64
	}{
		{"retained", 102}, {"substrate-db-2", 5}, {"rustfs-data", 10},
	} {
		v := resource(item.name)
		v["spec"] = object{"dataEngine": "v1", "numberOfReplicas": float64(3), "size": fmt.Sprint(item.size * v1FixtureGiB)}
		state, robustness, instanceState := "attached", "degraded", "running"
		if item.name == "retained" {
			state, robustness, instanceState = "detached", "unknown", "stopped"
		}
		v["status"] = object{"state": state, "robustness": robustness, "kubernetesStatus": object{"namespace": "ate-system", "pvcName": item.name},
			"conditions": []any{object{"type": "Scheduled", "status": "False", "reason": "ReplicaSchedulingFailure", "message": "insufficient storage"}}}
		s.volumes = append(s.volumes, v)
		e := resource(item.name + "-engine")
		e["spec"] = object{"volumeName": item.name, "dataEngine": "v1", "active": true}
		modes := object{}
		e["status"] = object{"currentState": instanceState, "replicaModeMap": modes}
		s.engines = append(s.engines, e)
		for i, node := range s.nodes {
			if i == 0 && item.name != "retained" {
				continue
			}
			name := fmt.Sprintf("%s-%d", item.name, i+1)
			r := resource(name)
			nodeName := text(node, "metadata", "name")
			r["spec"] = object{"volumeName": item.name, "engineName": item.name + "-engine", "dataEngine": "v1", "active": true, "nodeID": nodeName, "diskID": "disk-" + nodeName, "diskPath": "/var/lib/longhorn", "volumeSize": fmt.Sprint(item.size * v1FixtureGiB), "healthyAt": "2026-11-09T20:00:00Z"}
			r["status"] = object{"currentState": instanceState}
			s.replicas = append(s.replicas, r)
			modes[name] = "RW"
			diskName := "default-disk"
			if i == 0 {
				diskName = "default-disk-fixture"
			}
			value(s.storageNodes[i], "status", "diskStatus", diskName, "scheduledReplica").(object)[name] = float64(item.size * v1FixtureGiB)
		}
	}
	return s
}

func TestV1ReservationRepairOpensOnlyTheMissingReplicaCapacity(t *testing.T) {
	s := v1ReservationFixture()
	plan, err := planV1ReservationRepair(s)
	if err != nil || plan.reserve != 33*v1FixtureGiB || plan.disk != "default-disk-fixture" || text(plan.node, "metadata", "name") != "prod-worker-1" || len(plan.volumes) != 2 {
		t.Fatalf("complete repair projection refused or widened: %+v %v", plan, err)
	}
}

func TestV1ReservationRepairHoldsOnUnknownCapacityOrHealth(t *testing.T) {
	for name, change := range map[string]func(*inventory){
		"physical floor": func(s *inventory) {
			value(s.storageNodes[0], "status", "diskStatus", "default-disk-fixture").(object)["storageAvailable"] = float64(55 * v1FixtureGiB)
		},
		"unknown allocation": func(s *inventory) {
			value(s.storageNodes[0], "status", "diskStatus", "default-disk-fixture").(object)["storageScheduled"] = float64(103 * v1FixtureGiB)
		},
		"foreign claim": func(s *inventory) {
			value(s.volumes[1], "status", "kubernetesStatus").(object)["namespace"] = "unrelated"
		},
		"lost peer copy": func(s *inventory) {
			s.replicas = s.replicas[:len(s.replicas)-1]
		},
		"ambiguous third engine mode": func(s *inventory) {
			value(s.engines[1], "status", "replicaModeMap").(object)["unexpected-copy"] = "RW"
		},
		"peer reserve disagreement": func(s *inventory) {
			value(s.storageNodes[2], "spec", "disks", "default-disk").(object)["storageReserved"] = float64(23 * v1FixtureGiB)
		},
		"existing maintenance fence": func(s *inventory) {
			s.nodes[0]["metadata"].(object)["annotations"] = object{fenceAnnotation: "different-run"}
		},
		"target not ready": func(s *inventory) {
			value(s.nodes[0], "status").(object)["conditions"] = []any{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := v1ReservationFixture()
			change(&s)
			if _, err := planV1ReservationRepair(s); err == nil {
				t.Fatal("unverified replica scheduling repair accepted")
			}
		})
	}
}

func TestV1ReservationRepairRequiresItsOwnFixedDispatch(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "repair-v1-reservation"
	env["LONGHORN_V2_CONFIRM"] = "repair-longhorn-v1-reservation"
	getenv := func(key string) string { return env[key] }
	if err := authorizeV1ReservationRepair(getenv, "prod-worker-1"); err != nil {
		t.Fatal(err)
	}
	if authorizeV1ReservationRepair(getenv, "prod-worker-2") == nil || authorize(getenv, "prod-worker-1") == nil {
		t.Fatal("repair dispatch authorized a different mutation or worker")
	}
	env["GITHUB_RUN_ATTEMPT"] = "2"
	if authorizeV1ReservationRepair(getenv, "prod-worker-1") == nil {
		t.Fatal("repair accepted a workflow rerun")
	}
}
