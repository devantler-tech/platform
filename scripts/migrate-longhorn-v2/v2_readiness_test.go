package main

import (
	"fmt"
	"testing"
)

// v2ReadinessFixture keeps original V1 replicas on permanent workers while
// independent temporary disks supply the prospective V2 storage.
func v2ReadinessFixture() (inventory, []object) {
	return v2ReadinessFixtureForRoute(false)
}

// v2ReadinessFixtureForRoute can represent the superseded original-disk route
// solely to prove that it cannot satisfy temporary-only activation admission.
func v2ReadinessFixtureForRoute(legacy bool) (inventory, []object) {
	s := evacuatedFixture()
	s.nodes = capacityFixture()
	old := []string{"storage-1", "storage-2", "storage-3"}
	v1Nodes := []string{"prod-worker-1", "prod-worker-2", "prod-worker-3"}
	if legacy {
		v1Nodes = []string{"prod-worker-3", text(s.nodes[3], "metadata", "name"), text(s.nodes[4], "metadata", "name")}
	}
	for i, name := range v1Nodes {
		s.storageNodes[i]["metadata"].(object)["name"] = name
		for _, r := range s.replicas {
			if text(r, "spec", "nodeID") == old[i] {
				r["spec"].(object)["nodeID"] = name
			}
		}
	}
	for _, n := range s.nodes[:6] {
		n["status"].(object)["allocatable"].(object)["cpu"] = "8"
		role := text(n, "metadata", "labels", migrationPoolLabel)
		if legacy && (text(n, "metadata", "name") == "prod-worker-3" || role == "v1") || !legacy && role == "" {
			continue
		}
		lh := resource(text(n, "metadata", "name"))
		lh["spec"] = object{"disks": blockDisks(), "allowScheduling": true, "evictionRequested": false, "instanceManagerCPURequest": float64(0)}
		lh["status"] = object{"conditions": readyConditions(), "diskStatus": object{"longhorn-v2": object{"diskUUID": "raw-" + text(n, "metadata", "name"), "conditions": readyConditions()}}}
		s.storageNodes = append(s.storageNodes, lh)
	}
	managers := []object{}
	for i, n := range s.nodes[:6] {
		im := resource(fmt.Sprintf("im-%d", i))
		im["metadata"].(object)["namespace"] = "longhorn-system"
		im["spec"] = object{"nodeID": text(n, "metadata", "name"), "dataEngine": "v2", "type": "aio", "image": "longhornio/longhorn-instance-manager:v1.13.0"}
		im["status"] = object{"currentState": "running"}
		managers = append(managers, im)
		p := resource(text(im, "metadata", "name"))
		p["metadata"].(object)["namespace"] = "longhorn-system"
		p["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "InstanceManager", "name": text(im, "metadata", "name"), "uid": text(im, "metadata", "uid"), "controller": true}}
		p["spec"] = object{"nodeName": text(n, "metadata", "name"), "containers": []any{object{"name": "instance-manager", "image": text(im, "spec", "image"),
			"args":      []any{"start-spdk-tgt", "--spdk-cpumask", "0x3", "--spdk-core-number", "0", "--spdk-memory-size", "2048", "--enable-spdk", "daemon", "--spdk-enabled", "--enable-irq-affinity", "--enable-workqueue-affinity", "--enable-rps"},
			"resources": object{"requests": object{"cpu": "2"}, "limits": object{"cpu": "4", "hugepages-2Mi": "2Gi"}}}}}
		p["status"] = object{"phase": "Running", "conditions": readyConditions()}
		s.pods = append(s.pods, p)
	}
	return s, managers
}

// Raw storage on original workers cannot borrow the temporary-route authority.
func TestV2RuntimeRejectsTheEarlierRebuiltPermanentWorkerRoute(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	s, managers := v2ReadinessFixtureForRoute(true)
	if verifyV2Runtime(s, managers, pools, "longhornio/longhorn-instance-manager:v1.13.0") == nil {
		t.Fatal("the temporary-only route accepted raw storage on original workers")
	}
}

// Ready disk identities, original redundancy and effective SPDK ownership,
// executable arguments and reservations are all required for storage readiness.
func TestV2RuntimeRequiresRealRawDisksAndReservedReactors(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	s, managers := v2ReadinessFixture()
	if err = verifyV2Runtime(s, managers, pools, "longhornio/longhorn-instance-manager:v1.13.0"); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*inventory, *[]object){
		"unrebuilt disk":     func(s *inventory, _ *[]object) { s.storageNodes[3]["spec"].(object)["disks"] = object{} },
		"raw disk not ready": func(s *inventory, _ *[]object) { s.storageNodes[3]["status"].(object)["diskStatus"] = object{} },
		"raw disk identity reused": func(s *inventory, _ *[]object) {
			s.storageNodes[4]["status"].(object)["diskStatus"].(object)["longhorn-v2"].(object)["diskUUID"] = text(s.storageNodes[3], "status", "diskStatus", "longhorn-v2", "diskUUID")
		},
		"retained copy lost":         func(s *inventory, _ *[]object) { s.replicas[3]["spec"].(object)["healthyAt"] = "" },
		"client manager missing":     func(_ *inventory, m *[]object) { *m = (*m)[1:] },
		"client manager not running": func(_ *inventory, m *[]object) { (*m)[2]["status"].(object)["currentState"] = "starting" },
		"manager pod unready":        func(s *inventory, _ *[]object) { s.pods[0]["status"].(object)["conditions"] = []any{} },
		"manager type replaced":      func(_ *inventory, m *[]object) { (*m)[0]["spec"].(object)["type"] = "engine" },
		"manager deletion pending": func(_ *inventory, m *[]object) {
			(*m)[0]["metadata"].(object)["deletionTimestamp"] = "2026-11-03T09:00:00Z"
		},
		"manager identity missing": func(_ *inventory, m *[]object) { delete((*m)[0]["metadata"].(object), "uid") },
		"foreign manager pod":      func(s *inventory, _ *[]object) { s.pods[0]["metadata"].(object)["ownerReferences"] = []any{} },
		"entrypoint overridden": func(s *inventory, _ *[]object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["command"] = []any{"/bin/true"}
		},
		"SPDK bootstrap absent": func(s *inventory, _ *[]object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["args"].([]any)[0] = "daemon"
		},
		"SPDK launch disabled": func(s *inventory, _ *[]object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["args"].([]any)[7] = "--debug"
		},
		"V2 daemon disabled": func(s *inventory, _ *[]object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["args"].([]any)[9] = "--debug"
		},
		"insufficient cpu": func(s *inventory, _ *[]object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["resources"].(object)["requests"] = object{"cpu": "480m"}
		},
		"reactors consume whole cpu limit": func(s *inventory, _ *[]object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["resources"].(object)["limits"].(object)["cpu"] = "2"
		},
		"hugepages not reserved": func(s *inventory, _ *[]object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["resources"].(object)["limits"] = object{"hugepages-2Mi": "0"}
		},
		"one busy reactor": func(s *inventory, _ *[]object) {
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["args"].([]any)[2] = "0x1"
		},
		"cpu isolation missing": func(s *inventory, _ *[]object) {
			c := value(s.pods[0], "spec", "containers").([]any)[0].(object)
			c["args"] = c["args"].([]any)[:7]
		},
		"node pressured": func(s *inventory, _ *[]object) { s.nodes[0]["status"].(object)["conditions"] = readyConditions() },
		"raw cpu override": func(s *inventory, _ *[]object) {
			s.storageNodes[3]["spec"].(object)["instanceManagerCPURequest"] = float64(480)
		},
		"raw eviction status missing": func(s *inventory, _ *[]object) {
			delete(s.storageNodes[3]["spec"].(object), "evictionRequested")
		},
		"matching but unreviewed image": func(s *inventory, m *[]object) {
			(*m)[0]["spec"].(object)["image"] = "example.invalid/foreign:latest"
			value(s.pods[0], "spec", "containers").([]any)[0].(object)["image"] = "example.invalid/foreign:latest"
		},
		"interrupt mode assignment": func(s *inventory, _ *[]object) {
			c := value(s.pods[0], "spec", "containers").([]any)[0].(object)
			c["args"] = append(c["args"].([]any), "--spdk-interrupt-mode=true")
		},
		"hugepage bypass assignment": func(s *inventory, _ *[]object) {
			c := value(s.pods[0], "spec", "containers").([]any)[0].(object)
			c["args"] = append(c["args"].([]any), "--spdk-no-hugepage=true")
		},
		"isolation disabled by later assignment": func(s *inventory, _ *[]object) {
			c := value(s.pods[0], "spec", "containers").([]any)[0].(object)
			c["args"] = append(c["args"].([]any), "--enable-irq-affinity=false")
		},
		"unknown extra manager engine": func(_ *inventory, m *[]object) {
			im := resource("unknown-engine")
			im["spec"] = object{"dataEngine": "v3"}
			*m = append(*m, im)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, m := v2ReadinessFixture()
			change(&s, &m)
			if verifyV2Runtime(s, m, pools, "longhornio/longhorn-instance-manager:v1.13.0") == nil {
				t.Fatal("V2 canary admitted without its real runtime prerequisites")
			}
		})
	}
}
