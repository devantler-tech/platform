package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const providerRetentionApproval = "retain-hetzner-source:openbao/data:50ca82cea943a2a39d6fbbccdb1988187c52903c5649562b1c9aa4d58681ba94"

func TestRetainProviderSourcePreservesOnlyTheApprovedPV(t *testing.T) {
	const source = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, test := range []struct {
		name, fault string
		writes      int
		proven      bool
	}{
		{"exact original", "", 1, true},
		{"already retained", "retained", 0, true},
		{"replacement handle", "handle", 0, false},
		{"storage class changes driver", "class", 0, false},
		{"backup-owned PVC", "backup", 0, false},
		{"lost write response read back", "lost", 1, true},
		{"changed binding after write", "rebound", 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
				"metadata": object{"name": "data", "namespace": "openbao", "uid": "claim-uid", "resourceVersion": "21"},
				"spec": object{"volumeName": "pvc-claim-uid", "storageClassName": "hcloud", "accessModes": []any{"ReadWriteOnce"},
					"volumeMode": "Filesystem", "resources": object{"requests": object{"storage": "10Gi"}}},
				"status": object{"phase": "Bound", "capacity": object{"storage": "10Gi"}}}
			pv := object{"apiVersion": "v1", "kind": "PersistentVolume",
				"metadata": object{"name": "pvc-claim-uid", "uid": "pv-uid", "resourceVersion": "31"},
				"spec": object{"claimRef": object{"name": "data", "namespace": "openbao", "uid": "claim-uid"},
					"capacity": object{"storage": "10Gi"}, "storageClassName": "hcloud", "persistentVolumeReclaimPolicy": "Delete",
					"csi": object{"driver": hetznerCSIDriver, "volumeHandle": "1234"}, "accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem"},
				"status": object{"phase": "Bound"}}
			class := object{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
				"metadata": object{"name": "hcloud", "uid": "class-uid", "resourceVersion": "41"}, "provisioner": hetznerCSIDriver}
			pvSpec, pvCSI := valueObject(pv, "spec"), valueObject(pv, "spec", "csi")
			switch test.fault {
			case "retained":
				pvSpec["persistentVolumeReclaimPolicy"] = "Retain"
			case "handle":
				pvCSI["volumeHandle"] = "5678"
			case "class":
				class["provisioner"] = "driver.longhorn.io"
			case "backup":
				valueObject(claim, "metadata")["ownerReferences"] = []any{object{"apiVersion": "velero.io/v2alpha1", "kind": "DataUpload", "uid": "upload-uid"}}
			}
			writes := 0
			c := client{context: "admin@prod", run: func(_ context.Context, body []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				switch request := strings.Join(args[2:5], " "); request {
				case "get persistentvolumeclaim data":
					if !strings.Contains(strings.Join(args, " "), "--namespace=openbao") {
						t.Fatal("claim read escaped the approved namespace")
					}
					return json.Marshal(claim)
				case "get persistentvolume pvc-claim-uid":
					return json.Marshal(pv)
				case "get storageclass hcloud":
					return json.Marshal(class)
				case "patch persistentvolume pvc-claim-uid":
					writes++
					var operations []object
					want := []object{
						{"op": "test", "path": "/metadata/uid", "value": "pv-uid"},
						{"op": "test", "path": "/metadata/resourceVersion", "value": "31"},
						{"op": "test", "path": "/spec/claimRef", "value": pvSpec["claimRef"]},
						{"op": "test", "path": "/spec/csi/driver", "value": hetznerCSIDriver},
						{"op": "test", "path": "/spec/csi/volumeHandle", "value": "1234"},
						{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Delete"},
						{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
					}
					if json.Unmarshal(body, &operations) != nil || !reflect.DeepEqual(operations, want) {
						t.Fatalf("unexpected provider Retain write: %s", body)
					}
					pvSpec["persistentVolumeReclaimPolicy"] = "Retain"
					valueObject(pv, "metadata")["resourceVersion"] = "32"
					if test.fault == "rebound" {
						valueObject(claim, "spec")["volumeName"] = "replacement"
					}
					if test.fault == "lost" {
						return nil, errors.New("write response lost")
					}
					return []byte("persistentvolume/pvc-claim-uid"), nil
				default:
					t.Fatalf("unexpected provider source operation %s %q", program, args)
					return nil, nil
				}
			}}
			err := retainProviderSource(t.Context(), c, source, providerRetentionApproval)
			if (err == nil) != test.proven || writes != test.writes {
				t.Fatalf("expected writes=%d proven=%v, got writes=%d error=%v", test.writes, test.proven, writes, err)
			}
		})
	}
}

func TestProviderRetentionRequiresItsOwnProtectedDispatch(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"], env["LONGHORN_V2_CONFIRM"] = retainProviderSourceOperation, "", providerRetentionApproval
	getenv := func(key string) string { return env[key] }
	if err := authorizeProviderSourceRetention(getenv); err != nil {
		t.Fatal(err)
	}
	for key, bad := range map[string]string{
		"LONGHORN_V2_OPERATION": "retain-v1-source",
		"LONGHORN_V2_NODE":      "prod-worker-1",
		"LONGHORN_V2_CONFIRM":   strings.Replace(providerRetentionApproval, "openbao/", "tenant/", 1),
		"GITHUB_RUN_ATTEMPT":    "2",
	} {
		saved := env[key]
		env[key] = bad
		if authorizeProviderSourceRetention(getenv) == nil {
			t.Fatalf("foreign provider dispatch accepted %s=%q", key, bad)
		}
		env[key] = saved
	}
}
