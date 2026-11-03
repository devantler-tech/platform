package main

import (
	"testing"
)

func temporaryOwnedCordonFixture(t *testing.T) ([]object, journal) {
	t.Helper()
	nodes := temporaryInventoryRoster()
	j := decodeTemporaryJournal(t, temporaryJournalDocument(t))
	n := nodes[0]
	j.Target, j.NodeUID = text(n, "metadata", "name"), text(n, "metadata", "uid")
	j.ProviderID, j.MachineUUID = text(n, "spec", "providerID"), text(n, "status", "nodeInfo", "systemUUID")
	j.OriginalBootID = text(n, "status", "nodeInfo", "bootID")
	n["spec"].(object)["unschedulable"] = true
	n["metadata"].(object)["annotations"].(object)[fenceAnnotation] = "longhorn-v2-prepare-temporary-v1-1"
	return nodes, j
}

func TestTemporaryInventoryRecognizesOnlyItsIdentityBoundOwnedCordon(t *testing.T) {
	pools, err := configuredMigrationPools(poolConfigFixture())
	if err != nil {
		t.Fatal(err)
	}
	nodes, j := temporaryOwnedCordonFixture(t)
	if _, _, err = resolveTemporaryInventoryTarget(j.TemporarySelection, nodes, pools); err == nil {
		t.Fatal("ordinary read-only selection accepted a cordoned temporary node")
	}
	view, err := temporaryInventoryNodes(nodes, &j)
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := resolveTemporaryInventoryTarget(j.TemporarySelection, view, pools)
	if err != nil || target != j.Target || value(nodes[0], "spec", "unschedulable") != true {
		t.Fatal("owned cordon could not be observed without changing the original node:", err)
	}
	for name, mutate := range map[string]func([]object, *journal){
		"foreign fence": func(n []object, _ *journal) {
			n[0]["metadata"].(object)["annotations"].(object)[fenceAnnotation] = "foreign"
		},
		"missing fence": func(n []object, _ *journal) {
			delete(n[0]["metadata"].(object)["annotations"].(object), fenceAnnotation)
		},
		"replaced node": func(n []object, _ *journal) { n[0]["metadata"].(object)["uid"] = "replacement" },
		"changed hardware": func(n []object, _ *journal) {
			n[0]["status"].(object)["nodeInfo"].(object)["systemUUID"] = "replacement"
		},
		"changed boot": func(n []object, _ *journal) {
			n[0]["status"].(object)["nodeInfo"].(object)["bootID"] = "12345678-1234-4234-8234-123456789abc"
		},
		"changed provider":  func(n []object, _ *journal) { n[0]["spec"].(object)["providerID"] = "hcloud://9999" },
		"another slot":      func(_ []object, j *journal) { j.TemporarySelection = "temporary-v1-2" },
		"no physical proof": func(_ []object, j *journal) { j.NativeEmptyDigest = "" },
		"wrong schema":      func(_ []object, j *journal) { j.Schema = 1 },
		"cordoned peer":     func(n []object, _ *journal) { n[1]["spec"].(object)["unschedulable"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			nodes, j := temporaryOwnedCordonFixture(t)
			mutate(nodes, &j)
			view, err := temporaryInventoryNodes(nodes, &j)
			if err == nil {
				_, _, err = resolveTemporaryInventoryTarget(j.TemporarySelection, view, pools)
			}
			if err == nil {
				t.Fatal("an unowned, replaced or unproven cordon was accepted")
			}
		})
	}
}
