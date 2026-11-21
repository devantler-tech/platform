package main

import "testing"

func TestProviderInventoryBindsTheOriginalClaimAndVolume(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(object, object)
		proven bool
	}{
		{"exact bound provider claim", func(object, object) {}, true},
		{"replacement claim", func(_ object, pvc object) { valueObject(pvc, "metadata")["uid"] = "replacement" }, false},
		{"replacement binding", func(_ object, pvc object) { valueObject(pvc, "spec")["volumeName"] = "other" }, false},
		{"unsafe resize in flight", func(_ object, pvc object) { valueObject(pvc, "spec", "resources", "requests")["storage"] = "11Gi" }, false},
		{"provider handle mismatch", func(pv, _ object) { valueObject(pv, "spec", "csi")["volumeHandle"] = "unknown" }, false},
		{"released source", func(pv, _ object) { valueObject(pv, "status")["phase"] = "Released" }, false},
		{"block mode outside filesystem plan", func(pv, _ object) { valueObject(pv, "spec")["volumeMode"] = "Block" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pv := object{
				"metadata": object{"name": "pv", "uid": "pv-uid", "resourceVersion": "42"},
				"spec": object{
					"claimRef": object{"name": "data", "namespace": "provider-app", "uid": "pvc-uid"},
					"capacity": object{"storage": "10Gi"}, "storageClassName": "hcloud",
					"csi":         object{"driver": "csi.hetzner.cloud", "volumeHandle": "1234"},
					"accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem",
				},
				"status": object{"phase": "Bound"},
			}
			pvc := object{
				"metadata": object{"name": "data", "namespace": "provider-app", "uid": "pvc-uid", "resourceVersion": "41"},
				"spec": object{
					"volumeName": "pv", "storageClassName": "hcloud", "accessModes": []any{"ReadWriteOnce"},
					"volumeMode": "Filesystem", "resources": object{"requests": object{"storage": "10Gi"}},
				},
				"status": object{"phase": "Bound", "capacity": object{"storage": "10Gi"}},
			}
			test.change(pv, pvc)
			claims, err := selectProviderVolumeClaims([]object{pv}, []object{pvc})
			if test.proven {
				if err != nil || len(claims) != 1 || claims[0].RequestedBytes != 10<<30 || claims[0].ClaimUID != "pvc-uid" {
					t.Fatalf("valid source inventory was rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("an incomplete or changed source counted as a verified capacity input")
			}
		})
	}
}
