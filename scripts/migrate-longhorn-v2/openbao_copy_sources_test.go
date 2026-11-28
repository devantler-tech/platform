package main

import "testing"

func TestOpenBaoCopyRequiresBothExactProviderAssignments(t *testing.T) {
	for _, fault := range []string{"", "old pod still present", "old ticket still present",
		"other provider target", "wrong provider server", "no authoritative assignee", "duplicate provider id",
		"wrong topology", "writable source"} {
		t.Run(fault, func(t *testing.T) {
			plan := openBaoPairPlanFixture(t)
			_, originalPods, claims, pvs, attachments := openBaoProviderStorageFixture()
			copyPod := plan.copyPod()
			meta := valueObject(copyPod, "metadata")
			meta["uid"], meta["resourceVersion"] = "bd879525-004e-400a-be89-719605db0f95", "51"
			valueObject(meta, "labels")["topology.kubernetes.io/zone"] = "zone-1"
			valueObject(copyPod, "spec")["nodeName"] = plan.Original.Node
			copyPod["status"] = object{"phase": "Pending"}
			node := object{
				"apiVersion": "v1", "kind": "Node",
				"metadata": object{"name": plan.Original.Node, "uid": "4ebbd7a0-ce28-443a-823e-b3264c6840bc", "resourceVersion": "70",
					"labels": object{"kubernetes.io/hostname": plan.Original.Node, "topology.kubernetes.io/zone": "zone-1"}},
				"spec":   object{"providerID": "hcloud://12"},
				"status": object{"conditions": []any{object{"type": "Ready", "status": "True"}}},
			}
			pods := []object{copyPod}
			newUIDs := [2]string{"740a7a18-c5e9-4ca3-8f6b-551c1c50bd0f", "69735d93-3a09-4bf6-b285-8a0f630c2e27"}
			provider := []object{
				{"id": float64(101), "size": float64(10), "status": "available", "server": float64(12)},
				{"id": float64(102), "size": float64(40), "status": "available"},
			}
			for i, disk := range plan.Original.Disks {
				valueObject(attachments[i], "metadata")["uid"] = newUIDs[i]
				provider[i]["assignee"] = object{"type": "server", "id": float64(12)}
				provider[i]["linux_device"] = "/dev/disk/by-id/scsi-0HC_Volume_" + disk.ProviderHandle
				provider[i]["labels"] = object{"managed-by": "csi-driver", "pv-name": disk.PV.Name,
					"pvc-name": disk.PVC.Name, "pvc-namespace": openBaoNamespace}
			}
			switch fault {
			case "old pod still present":
				pods = append(pods, originalPods[0])
			case "old ticket still present":
				valueObject(attachments[1], "metadata")["uid"] = plan.Original.Disks[1].Attachment.UID
			case "other provider target":
				provider[1]["assignee"] = object{"type": "other", "id": float64(12)}
			case "wrong provider server":
				provider[1]["assignee"] = object{"type": "server", "id": float64(13)}
			case "no authoritative assignee":
				delete(provider[0], "assignee")
			case "duplicate provider id":
				provider = append(provider, object{"id": float64(101)})
			case "wrong topology":
				valueObject(meta, "labels")["topology.kubernetes.io/zone"] = "zone-2"
			case "writable source":
				volume := value(copyPod, "spec", "volumes").([]any)[2].(object)
				valueObject(volume, "persistentVolumeClaim")["readOnly"] = false
			}
			if err := verifyOpenBaoCopySources(plan, copyPod, node, pods, claims, pvs, attachments, provider); (err == nil) != (fault == "") {
				t.Fatalf("source acceptance for %q: %v", fault, err)
			}
		})
	}
}
