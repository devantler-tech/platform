package main

import (
	"fmt"
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

func TestClientRefreshRequiresTheCompleteFixedRoster(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCapacityRoster(capacityFixture(), pools); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]object) []object{
		"missing sixth worker": func(nodes []object) []object { return append(nodes[:5], nodes[6:]...) },
		"unready peer": func(nodes []object) []object {
			nodes[2]["status"].(object)["conditions"] = []any{}
			return nodes
		},
		"compute overflow remains": func(nodes []object) []object { return append(nodes, resource("autoscale-unknown-1")) },
		"control plane missing":    func(nodes []object) []object { return nodes[:8] },
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyCapacityRoster(change(capacityFixture()), pools); err == nil {
				t.Fatal("client refresh admitted without the complete ready roster")
			}
		})
	}
}
