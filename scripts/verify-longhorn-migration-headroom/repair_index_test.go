package main

import "testing"

func TestProviderIndexRepairBindsTheNativeNodeAndPreservesOtherLabels(t *testing.T) {
	servers, nodes := roster(t, true)
	servers[0].Labels["ksail.node.index"] = "4"
	servers[0].Labels["example.com/other"] = "preserve-me"
	row := controlPlaneIndex{ID: servers[0].ID, Name: servers[0].Name, Expected: "2", Observed: "4"}
	expected := []string{indexWitness(row, nodes[0].Metadata.UID)}
	repairs, err := planIndexRepair(servers, nodes, expected)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 {
		t.Fatalf("expected one index correction, got %d", len(repairs))
	}
	change := repairs[0]
	if change.original.Labels["ksail.node.index"] != "4" || change.updated.Labels["ksail.node.index"] != "2" ||
		change.updated.Labels["example.com/other"] != "preserve-me" || servers[0].Labels["ksail.node.index"] != "4" {
		t.Fatal("repair changed something outside the proposed index label")
	}
	nodes[0].Metadata.UID = "replacement-node"
	if _, err := planIndexRepair(servers, nodes, expected); err == nil {
		t.Fatal("repair accepted a replaced Kubernetes node")
	}
	nodes[0].Metadata.UID = "node-0"
	servers[1].Labels["ksail.node.index"] = "7"
	if _, err := planIndexRepair(servers, nodes, expected); err == nil {
		t.Fatal("repair accepted a second inconsistent control-plane index")
	}
	servers[1].Labels["ksail.node.index"] = "4"
	servers[3].Labels["ksail.node.index"] = "4"
	row = controlPlaneIndex{ID: servers[3].ID, Name: servers[3].Name, Expected: "1", Observed: "4"}
	expected = append(expected, indexWitness(row, nodes[3].Metadata.UID))
	repairs, err = planIndexRepair(servers, nodes, expected)
	if err != nil || len(repairs) != 2 {
		t.Fatalf("control-plane and worker changes should qualify together: %v, %v", repairs, err)
	}
}
