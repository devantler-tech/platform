package main

import (
	"fmt"
	"os"
	"testing"
)

func poolConfigFixture() []byte {
	return []byte(`{"spec":{"cluster":{"controlPlanes":3,"workers":3,"autoscaler":{"node":{"maxNodesTotal":9,"pools":[{"name":"autoscale-longhorn-v1","serverType":"cpx42","location":"fsn1","min":2,"max":2,"labels":{"storage.platform.devantler.tech/migration-pool":"v1"}},{"name":"autoscale-longhorn-v2","serverType":"cpx42","location":"fsn1","min":1,"max":1,"labels":{"storage.platform.devantler.tech/migration-pool":"v2"}}]}}},"provider":{"hetzner":{"serverLimit":10}}}}`)
}

func TestMigrationPoolsRequireFixedSizeAndReservedAccountHeadroom(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil || pools["v1"].count != 2 || pools["v2"].count != 1 {
		t.Fatalf("fixed migration pools rejected: %v", err)
	}
	for _, bad := range [][]byte{
		[]byte(`{"spec":{}}`),
		[]byte(`{"spec":{"cluster":{"workers":6}}}`),
		[]byte("spec:\n  cluster: {}\n  cluster: {}\n"),
	} {
		if _, err = configuredMigrationPools(bad); err == nil {
			t.Fatal("unresolved or duplicated capacity configuration accepted")
		}
	}
}

func migrationNodeFixture(role, suffix string) object {
	n := nodeFixture()
	n["metadata"].(object)["name"] = "autoscale-longhorn-" + role + "-" + suffix
	n["metadata"].(object)["labels"] = object{"ksail.io/autoscaled": "true", "storage.platform.devantler.tech/migration-pool": role, "node.kubernetes.io/instance-type": "cpx42"}
	n["status"].(object)["nodeInfo"].(object)["bootID"] = "boot-" + suffix
	sum := 0
	for _, b := range []byte(suffix) {
		sum += int(b)
	}
	n["status"].(object)["addresses"] = []any{object{"type": "InternalIP", "address": fmt.Sprintf("10.0.99.%d", 20+sum%200)}}
	return n
}

func TestCapacityActivationBindsPoolIdentityAndCurrentHardware(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyMigrationNode(migrationNodeFixture("v1", "0123456789abcdef"), pools["v1"]); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(object){
		"ordinary compute": func(n object) { n["metadata"].(object)["name"] = "autoscale-cx43-0123456789abcdef" },
		"near-miss pool":   func(n object) { n["metadata"].(object)["name"] = "autoscale-longhorn-v10-0123456789abcdef" },
		"missing pool label": func(n object) {
			delete(n["metadata"].(object)["labels"].(object), "storage.platform.devantler.tech/migration-pool")
		},
		"wrong server type": func(n object) {
			n["metadata"].(object)["labels"].(object)["node.kubernetes.io/instance-type"] = "cax21"
		},
		"missing provider": func(n object) { delete(n["spec"].(object), "providerID") },
		"unready":          func(n object) { n["status"].(object)["conditions"] = []any{} },
	} {
		t.Run(name, func(t *testing.T) {
			n := migrationNodeFixture("v1", "0123456789abcdef")
			mutate(n)
			if err = verifyMigrationNode(n, pools["v1"]); err == nil {
				t.Fatal("unproven migration-capacity identity accepted")
			}
		})
	}
}

// main reads this before any preparation step, so a dispatch holds with no write
// until the V2 pool lands. Whoever adds that pool replaces this test on purpose.
func TestPreparationStaysDormantUntilTheV2PoolLands(t *testing.T) {
	config, err := os.ReadFile("../../ksail.prod.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = configuredMigrationPools(config); err == nil {
		t.Fatal("the reviewed configuration now resolves both pools; re-prove what a dispatch can reach")
	}
}
