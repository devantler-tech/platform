package main

import "testing"

func TestKeeperCopyPodBindsSchedulerTopologyToItsNode(t *testing.T) {
	for _, test := range []struct {
		name, assigned, region, zone string
		extra, allowed               bool
	}{
		{name: "before scheduling", allowed: true},
		{name: "assigned topology", assigned: "worker-1", region: "region-1", zone: "zone-1", allowed: true},
		{name: "no topology labels", assigned: "worker-1", allowed: true},
		{name: "wrong node", assigned: "worker-2", region: "region-1", zone: "zone-1"},
		{name: "wrong region", assigned: "worker-1", region: "region-2", zone: "zone-1"},
		{name: "wrong zone", assigned: "worker-1", region: "region-1", zone: "zone-2"},
		{name: "before scheduling with topology", region: "region-1", zone: "zone-1"},
		{name: "extra policy selector", assigned: "worker-1", region: "region-1", zone: "zone-1", extra: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const source = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			desired := corootKeeperStagePod("12345", source, "worker-1")
			pod := corootKeeperStagePod("12345", source, "worker-1")
			metadata := valueObject(pod, "metadata")
			metadata["uid"] = "263efda9-7536-4b18-8e55-f8bb00ec6c21"
			metadata["resourceVersion"] = "15"
			labels := valueObject(metadata, "labels")
			if test.assigned != "" {
				valueObject(pod, "spec")["nodeName"] = test.assigned
			}
			if test.region != "" {
				labels["topology.kubernetes.io/region"] = test.region
			}
			if test.zone != "" {
				labels["topology.kubernetes.io/zone"] = test.zone
			}
			if test.extra {
				labels["app"] = "unrelated"
			}
			node := object{"apiVersion": "v1", "kind": "Node", "metadata": object{
				"name": "worker-1", "uid": "738eb607-5118-41da-82af-f36fa1ecb67c", "resourceVersion": "19",
				"labels": object{"kubernetes.io/hostname": "worker-1", "topology.kubernetes.io/region": "region-1", "topology.kubernetes.io/zone": "zone-1"},
			}}
			adjusted, err := copyPodDesiredTopology(pod, desired, node, "worker-1")
			matched := err == nil && matchCorootKeeperVolumePod(pod, pod, adjusted)
			if matched != test.allowed {
				t.Fatalf("Pod admission match = %v, want %v", matched, test.allowed)
			}
			if len(valueObject(desired, "metadata", "labels")) != 1 {
				t.Fatal("the reviewed Pod template was modified")
			}
		})
	}
}
