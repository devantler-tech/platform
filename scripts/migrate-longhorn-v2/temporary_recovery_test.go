package main

import "testing"

func TestTemporaryResetRecoveryCanObserveItsBoundNodeBeforeItRejoins(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	nodes, j := temporaryOwnedCordonFixture(t)
	j.Phase = "reset-requested"
	nodes[0]["status"].(object)["conditions"] = []any{}
	nodes[0]["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-000000000099"
	if _, _, err = resolveTemporaryInventoryTarget(j.TemporarySelection, nodes, pools); err == nil {
		t.Fatal("ordinary inventory accepted an unready cordoned node")
	}
	if target, err := resolveTemporaryResetRecovery(j.TemporarySelection, nodes, pools, j); err != nil || target != j.Target {
		t.Fatal("reset intent cannot reach read-only recovery:", err)
	}
	for _, scenario := range []string{"wrong phase", "missing proof", "wrong slot", "foreign fence", "replaced node", "changed provider", "unready peer"} {
		t.Run(scenario, func(t *testing.T) {
			nodes, j := temporaryOwnedCordonFixture(t)
			j.Phase = "reset-requested"
			switch scenario {
			case "wrong phase":
				j.Phase = "draining"
			case "missing proof":
				j.NativeEmptyDigest = ""
			case "wrong slot":
				j.TemporarySelection = "temporary-v1-2"
			case "foreign fence":
				nodes[0]["metadata"].(object)["annotations"].(object)[fenceAnnotation] = "foreign"
			case "replaced node":
				nodes[0]["metadata"].(object)["uid"] = "replacement"
			case "changed provider":
				nodes[0]["spec"].(object)["providerID"] = "hcloud://9999"
			case "unready peer":
				nodes[1]["status"].(object)["conditions"] = []any{}
			}
			if _, err := resolveTemporaryResetRecovery(j.TemporarySelection, nodes, pools, j); err == nil {
				t.Fatal("recovery selected a different or unproven physical node")
			}
		})
	}
}
