package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func capacityFixture() []object {
	nodes := []object{}
	for i := 1; i <= 6; i++ {
		name, labels := fmt.Sprintf("prod-worker-%d", i), object{}
		if i > 3 {
			name = fmt.Sprintf("prod-control-plane-%d", i)
			labels["node-role.kubernetes.io/control-plane"] = ""
		}
		n := resource(name)
		n["metadata"].(object)["labels"] = labels
		n["status"] = object{"conditions": healthyWorkerConditions(), "capacity": object{"hugepages-2Mi": "2Gi"}, "allocatable": object{"hugepages-2Mi": "2Gi"}}
		nodes = append(nodes, n)
	}
	for i, role := range []string{"v1", "v1", "v2"} {
		n := migrationNodeFixture(role, fmt.Sprintf("%016x", i))
		n["metadata"].(object)["annotations"].(object)[scaleDownDisabled] = "true"
		n["metadata"].(object)["annotations"].(object)[capacityOwnerAnnotation] = capacityOwnerValue
		nodes = append(nodes[:3+i], append([]object{n}, nodes[3+i:]...)...)
	}
	return nodes
}

func TestResetRequiresAllTemporaryCapacityAndNoComputeOverflow(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyMigrationCapacity(capacityFixture(), pools); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]object) []object{
		"missing sixth worker":     func(nodes []object) []object { return append(nodes[:5], nodes[6:]...) },
		"unready peer":             func(nodes []object) []object { nodes[2]["status"].(object)["conditions"] = []any{}; return nodes },
		"compute overflow remains": func(nodes []object) []object { return append(nodes, resource("autoscale-unknown")) },
		"control plane missing":    func(nodes []object) []object { return nodes[:8] },
		"huge pages not reported": func(nodes []object) []object {
			nodes[0]["status"].(object)["capacity"] = object{"hugepages-2Mi": "0"}
			return nodes
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyMigrationCapacity(change(capacityFixture()), pools); err == nil {
				t.Fatal("reset admitted without the complete ready migration capacity")
			}
		})
	}
}

func enabledTemporaryPoolFixture() (inventory, map[string]migrationPool) {
	s := inventory{nodes: capacityFixture()}
	for _, n := range s.nodes {
		if text(n, "metadata", "labels", migrationPoolLabel) != "v1" {
			continue
		}
		lh := resource(text(n, "metadata", "name"))
		lh["metadata"].(object)["labels"] = object{migrationEnabledLabel: "true"}
		lh["spec"] = object{"allowScheduling": true, "evictionRequested": false,
			"disks": object{"migration-v1": object{"allowScheduling": true, "evictionRequested": false}}}
		lh["status"] = object{"diskStatus": object{"migration-v1": object{"diskUUID": "disk", "conditions": readyConditions()}}}
		s.storageNodes = append(s.storageNodes, lh)
	}
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		panic(err)
	}
	return s, pools
}

func TestEvacuationRequiresEveryTemporaryV1NodeToAcceptReplicas(t *testing.T) {
	s, pools := enabledTemporaryPoolFixture()
	if err := verifyTemporaryStorageEnabled(s, pools); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(lh object){
		"label":       func(lh object) { delete(lh["metadata"].(object)["labels"].(object), migrationEnabledLabel) },
		"scheduling":  func(lh object) { lh["spec"].(object)["allowScheduling"] = false },
		"eviction":    func(lh object) { lh["spec"].(object)["evictionRequested"] = true },
		"no disk":     func(lh object) { lh["spec"].(object)["disks"] = object{} },
		"disk status": func(lh object) { delete(lh["status"].(object)["diskStatus"].(object), "migration-v1") },
	} {
		t.Run(name, func(t *testing.T) {
			s, pools := enabledTemporaryPoolFixture()
			change(s.storageNodes[1])
			if err := verifyTemporaryStorageEnabled(s, pools); err == nil {
				t.Fatal("a partly enabled temporary pool was accepted")
			}
		})
	}
	s, pools = enabledTemporaryPoolFixture()
	s.storageNodes = s.storageNodes[:1]
	if err := verifyTemporaryStorageEnabled(s, pools); err == nil {
		t.Fatal("a temporary node without a storage record was accepted")
	}
}

func TestBeginHoldsBeforeJournalWhenTemporaryStorageIsNotEnabled(t *testing.T) {
	f, p := newPreparationAPI(t, "evacuating")
	f.cm = nil
	if err := p.begin(context.Background()); err == nil || !strings.Contains(err.Error(), "temporary V1 pool") {
		t.Fatalf("begin passed the fence point without enabled temporary storage: %v", err)
	}
	if f.cm != nil {
		t.Fatal("begin wrote a journal before proving temporary storage")
	}
}

func TestV2CapacityHoldsWhileTheEngineIsOff(t *testing.T) {
	f, c, _ := newCapacityAPI(t)
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	f.base.v2Off = true
	if err = bootstrapCapacity(context.Background(), c, journalFixture().Source, pools["v2"]); err == nil || !strings.Contains(err.Error(), "V2 data engine is off") || f.storageWrites != 0 {
		t.Fatalf("V2 capacity activation ran with the engine off: %v", err)
	}
	f.base.v2Off = false
	if err = bootstrapCapacity(context.Background(), c, journalFixture().Source, pools["v2"]); err != nil && strings.Contains(err.Error(), "V2 data engine is off") {
		t.Fatal("V2 capacity activation held with the engine on")
	}
	f.base.v2Off = true
	if err = bootstrapCapacity(context.Background(), c, journalFixture().Source, pools["v1"]); err != nil {
		t.Fatalf("V1 capacity must not depend on the V2 engine: %v", err)
	}
}

// enableTemporaryStorage marks the fixture's temporary V1 pool as ready for replicas.
func enableTemporaryStorage(s *inventory) {
	for _, n := range s.nodes {
		if text(n, "metadata", "labels", migrationPoolLabel) != "v1" {
			continue
		}
		name := text(n, "metadata", "name")
		var lh object
		for _, candidate := range s.storageNodes {
			if text(candidate, "metadata", "name") == name {
				lh = candidate
			}
		}
		if lh == nil {
			lh = resource(name)
			lh["spec"] = object{"disks": object{}}
			lh["status"] = object{"conditions": readyConditions(), "diskStatus": object{}}
			s.storageNodes = append(s.storageNodes, lh)
		}
		lh["metadata"].(object)["labels"] = object{migrationEnabledLabel: "true"}
		lh["spec"].(object)["allowScheduling"] = true
		lh["spec"].(object)["evictionRequested"] = false
		lh["spec"].(object)["disks"].(object)["migration-v1"] = object{"allowScheduling": true, "evictionRequested": false}
		lh["status"].(object)["diskStatus"].(object)["migration-v1"] = object{"diskUUID": "disk-" + name, "conditions": readyConditions()}
	}
}
