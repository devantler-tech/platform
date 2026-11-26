package main

import (
	"testing"
)

func TestSelectCNPGRecoveryHostRequiresEnoughProtectedCapacity(t *testing.T) {
	t.Parallel()

	bindings := map[string]temporaryV2Binding{}
	live := inventory{}
	for i, selection := range temporaryV2Selections {
		name := "worker-" + string(rune('a'+i))
		binding := temporaryV2Binding{
			Name: name, Selection: selection, NodeUID: name + "-uid",
			ProviderID: "hcloud://" + name, MachineUUID: name + "-machine",
			BootID: name + "-boot", OSImage: "Talos",
		}
		bindings[selection] = binding
		node := object{
			"metadata": object{"name": name, "uid": binding.NodeUID, "labels": object{"kubernetes.io/hostname": name},
				"annotations": object{"cluster-autoscaler.kubernetes.io/scale-down-disabled": "true"}},
			"spec": object{"providerID": binding.ProviderID},
			"status": object{"nodeInfo": object{"systemUUID": binding.MachineUUID, "bootID": binding.BootID, "osImage": binding.OSImage},
				"capacity": object{"hugepages-2Mi": "2Gi"}, "allocatable": object{"hugepages-2Mi": "2Gi", "memory": "8Gi", "cpu": "8", "pods": "110"},
				"conditions": []any{object{"type": "Ready", "status": "True"}, object{"type": "DiskPressure", "status": "False"},
					object{"type": "MemoryPressure", "status": "False"}, object{"type": "PIDPressure", "status": "False"}}},
		}
		live.nodes = append(live.nodes, node)
		memory := "8Gi"
		if i == 2 {
			memory = "5Gi"
		}
		live.pods = append(live.pods, object{"spec": object{"nodeName": name, "containers": []any{
			object{"resources": object{"requests": object{"memory": memory, "cpu": "1"}}},
		}}, "status": object{"phase": "Running"}})
	}

	want := bindings[temporaryV2Selections[2]].Name
	host, err := selectCNPGRecoveryHost(live, bindings)
	if err != nil || host != want {
		t.Fatalf("expected only the worker with room, got %q (%v)", host, err)
	}

	node := live.nodes[2]
	annotations := valueObject(node, "metadata", "annotations")
	delete(annotations, "cluster-autoscaler.kubernetes.io/scale-down-disabled")
	if _, err := selectCNPGRecoveryHost(live, bindings); err == nil {
		t.Fatal("an unprotected autoscaled worker was accepted")
	}

	annotations["cluster-autoscaler.kubernetes.io/scale-down-disabled"] = "true"
	valueObject(node, "metadata")["uid"] = "replaced"
	if _, err := selectCNPGRecoveryHost(live, bindings); err == nil {
		t.Fatal("a replacement worker inherited the old registration proof")
	}
}
