package main

import (
	"fmt"
	"testing"
)

func benchmarkComputeFixture() (inventory, []object, object) {
	s := benchmarkInventoryFixture()
	managers := []object{}
	for i := range 3 {
		name := fmt.Sprintf("im-%d", i)
		im := resource(name)
		im["spec"] = object{"nodeID": fmt.Sprintf("prod-worker-%d", i+1), "dataEngine": "v1", "image": "longhornio/longhorn-instance-manager:v1.13.0"}
		im["status"] = object{"currentState": "running"}
		managers = append(managers, im)
		pod := resource(name)
		pod["metadata"].(object)["namespace"] = "longhorn-system"
		pod["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "InstanceManager", "uid": text(im, "metadata", "uid"), "name": name, "controller": true}}
		pod["spec"] = object{"nodeName": text(im, "spec", "nodeID"), "containers": []any{object{"image": text(im, "spec", "image"), "resources": object{"requests": object{"cpu": "480m"}, "limits": object{"cpu": "2"}}}}}
		pod["status"] = object{"phase": "Running", "conditions": readyConditions(), "containerStatuses": []any{object{"imageID": "sha256:fixture"}}}
		s.pods = append(s.pods, pod)
	}
	objects, _ := benchmarkResources("v1", "1234", "longhorn-benchmark-v1-1234")
	return s, managers, objects[4]
}

func TestBenchmarkRecordsAdmittedClientAndStorageCPU(t *testing.T) {
	s, managers, probe := benchmarkComputeFixture()
	cpu, err := benchmarkCompute(s, managers, probe, "v1")
	if err != nil || len(cpu.Managers) != 3 || cpu.ClientRequest != "1" || cpu.ClientLimit != "2" {
		t.Fatalf("actual CPU context missing: %+v %v", cpu, err)
	}
	for name, change := range map[string]func(*inventory, *[]object, object){
		"changed client allocation": func(_ *inventory, _ *[]object, p object) {
			value(p, "spec", "containers").([]any)[0].(object)["resources"].(object)["limits"] = object{"cpu": "4"}
		},
		"missing storage manager": func(_ *inventory, m *[]object, _ object) { *m = (*m)[1:] },
		"unready storage pod":     func(s *inventory, _ *[]object, _ object) { s.pods[0]["status"].(object)["conditions"] = []any{} },
		"foreign storage pod":     func(s *inventory, _ *[]object, _ object) { s.pods[0]["metadata"].(object)["ownerReferences"] = []any{} },
		"missing storage request": func(s *inventory, _ *[]object, _ object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["resources"].(object)["requests"] = object{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, m, p := benchmarkComputeFixture()
			change(&s, &m, p)
			if _, err := benchmarkCompute(s, m, p, "v1"); err == nil {
				t.Fatal("unproven CPU allocation accepted for comparison")
			}
		})
	}
}
