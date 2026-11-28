package main

import "testing"

func TestProviderCNPGOriginalDetachRequiresTheExactRetainedDisk(t *testing.T) {
	const claimUID = "31915a8b-321e-4c0c-a6bc-cc91baf16b4b"
	const volumeUID = "456a6700-9888-44f4-8e30-aeab136f0352"
	const claimName = "customer-identity-db-2"
	const namespace = "customer-identity"
	const handle = "916230"
	const pvName = "pvc-" + claimUID
	original := providerCNPGOriginal{
		Claim: benchmarkResource{Name: claimName, Namespace: namespace, UID: claimUID},
		PV:    benchmarkResource{Name: pvName, UID: volumeUID}, Handle: handle,
	}
	for _, scenario := range []struct {
		name, phase string
		unsafe      bool
	}{
		{name: "bound retained", phase: "Bound"},
		{name: "released retained", phase: "Released"},
		{name: "reclaimable original", phase: "Released", unsafe: true},
		{name: "different pv identity", phase: "Bound", unsafe: true},
		{name: "duplicate volume handle", phase: "Bound", unsafe: true},
		{name: "pending attachment", phase: "Released", unsafe: true},
		{name: "provider attachment", phase: "Bound", unsafe: true},
		{name: "missing assignee", phase: "Bound", unsafe: true},
		{name: "foreign provider labels", phase: "Released", unsafe: true},
		{name: "unknown lifecycle", phase: "Available", unsafe: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			pv := object{
				"apiVersion": "v1", "kind": "PersistentVolume",
				"metadata": object{"name": pvName, "uid": volumeUID, "resourceVersion": "20"},
				"spec": object{"storageClassName": "hcloud", "persistentVolumeReclaimPolicy": "Retain", "volumeMode": "Filesystem",
					"accessModes": []any{"ReadWriteOnce"}, "capacity": object{"storage": "10Gi"},
					"csi":      object{"driver": hetznerCSIDriver, "volumeHandle": handle},
					"claimRef": object{"name": claimName, "namespace": namespace, "uid": claimUID}},
				"status": object{"phase": scenario.phase},
			}
			provider := object{"id": float64(916230), "size": float64(10), "status": "available",
				"server": nil, "assignee": nil, "linux_device": "/dev/disk/by-id/scsi-0HC_Volume_" + handle,
				"labels": object{"managed-by": "csi-driver", "pv-name": pvName, "pvc-name": claimName, "pvc-namespace": namespace}}
			pvs, tickets := []object{pv}, []object{}
			switch scenario.name {
			case "reclaimable original":
				valueObject(pv, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
			case "different pv identity":
				valueObject(pv, "metadata")["uid"] = "d26334f5-a765-4bc9-9744-cc129fa59419"
			case "duplicate volume handle":
				pvs = append(pvs, object{"metadata": object{"name": "duplicate"}, "spec": object{
					"csi": object{"driver": hetznerCSIDriver, "volumeHandle": handle},
				}})
			case "pending attachment":
				tickets = append(tickets, object{"spec": object{"source": object{"persistentVolumeName": pvName}}})
			case "provider attachment":
				provider["assignee"] = object{"id": float64(1), "type": "server"}
			case "missing assignee":
				delete(provider, "assignee")
			case "foreign provider labels":
				valueObject(provider, "labels")["pvc-namespace"] = "different-database"
			}
			err := verifyProviderCNPGDetached(original, scenario.phase, pvs, tickets, provider)
			if (err != nil) != scenario.unsafe {
				t.Fatalf("retained provider disk verdict: %v; unsafe=%t", err, scenario.unsafe)
			}
		})
	}
}

func TestProviderCNPGAttachedVolumeRequiresTheNativeNode(t *testing.T) {
	original := providerCNPGOriginal{Node: "database-worker"}
	node := object{
		"apiVersion": "v1", "kind": "Node", "metadata": object{"name": original.Node,
			"uid": "31915a8b-321e-4c0c-a6bc-cc91baf16b4b", "resourceVersion": "20"},
		"spec":   object{"providerID": "hcloud://503"},
		"status": object{"conditions": []any{object{"type": "Ready", "status": "True"}}},
	}
	for _, scenario := range []struct {
		name    string
		assign  object
		legacy  any
		allowed bool
	}{
		{name: "native server", assign: object{"type": "server", "id": float64(503)}, allowed: true},
		{name: "legacy field agrees", assign: object{"type": "server", "id": float64(503)}, legacy: float64(503), allowed: true},
		{name: "legacy field conflicts", assign: object{"type": "server", "id": float64(503)}, legacy: float64(504)},
		{name: "legacy field only", legacy: float64(503)},
		{name: "other server", assign: object{"type": "server", "id": float64(504)}},
		{name: "other resource", assign: object{"type": "other", "id": float64(503)}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			provider := object{"assignee": scenario.assign, "server": scenario.legacy}
			if providerCNPGAttachedToNode(original, provider, node) != scenario.allowed {
				t.Fatalf("native provider attachment: want allowed=%t", scenario.allowed)
			}
		})
	}
}
