package main

import (
	"strings"
	"testing"
)

func TestRetirementInstancesRequireSealedNodesWithoutManagerNames(t *testing.T) {
	for _, manager := range []string{"absent", "empty"} {
		for _, kind := range []string{"engines", "replicas", "enginefrontends"} {
			for _, node := range []string{"prod-worker-1", "prod-worker-foreign"} {
				t.Run(manager+"/"+kind+"/"+node, func(t *testing.T) {
					record, groups := retirementEmptyManagerNodeFixture(t, kind, manager, node)
					children, err := qualifyRetirementInstances(record, groups)
					_, fenceErr := benchmarkRetirementInstanceFence(record, groups, "5678-"+strings.Repeat("a", 32))
					want := node == "prod-worker-1"
					if (err == nil) != want || (fenceErr == nil) != want || want && len(children) != 4 {
						t.Fatalf("unsealed child placement qualified: children=%d qualification=%v fence=%v", len(children), err, fenceErr)
					}
				})
			}
		}
		for _, node := range []string{"", "prod-worker-4"} {
			t.Run(manager+"/detached-or-additional-epoch/"+node, func(t *testing.T) {
				record, groups := retirementEmptyManagerNodeFixture(t, "engines", manager, node)
				if _, err := qualifyRetirementInstances(record, groups); err != nil {
					t.Fatal("native detached or additionally sealed child refused:", err)
				}
			})
		}
	}
}

func retirementEmptyManagerNodeFixture(t *testing.T, kind, manager, node string) (benchmarkRecord, map[string][]object) {
	t.Helper()
	record, groups := retirementRuntimeInstanceFixture(t)
	status := groups[kind][0]["status"].(object)
	delete(status, "instanceManagerName")
	if manager == "empty" {
		status["instanceManagerName"] = ""
	}
	groups[kind][0]["spec"].(object)["nodeID"] = node
	if node == "prod-worker-4" {
		epoch := record.V1Runtime.Epochs[0]
		epoch.Pod, epoch.PodUID, epoch.ManagerUID, epoch.Node = "instance-manager-4", "pod-4", "manager-4", node
		epoch.ContainerID = "containerd://" + strings.Repeat("4", 64)
		record.V1Runtime.Epochs = append(record.V1Runtime.Epochs, epoch)
	}
	if err := validateBenchmarkRuntimeReceipt(record); err != nil {
		t.Fatal("invalid sealed-node runtime fixture:", err)
	}
	return record, groups
}
