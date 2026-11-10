package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const retentionApproval = "retain-longhorn-v1-source:tenant/data:d9a84d4abc09981ae87e930351eef94589a255439b4ab6c79529cfd7ef38d720"

func TestRetainV1SourceWritesOnlyTheApprovedPersistentVolume(t *testing.T) {
	const source = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name, change string
		write, fail  bool
	}{
		{name: "moves only the approved PV from Delete to Retain", write: true},
		{name: "already retained is an observation", change: "retained"},
		{name: "new claim identity is rejected", change: "claim", fail: true},
		{name: "new backend identity is rejected", change: "backend", fail: true},
		{name: "deleting PV is rejected", change: "deleting", fail: true},
		{name: "data mover is excluded", change: "upload", fail: true},
		{name: "base moved before write", change: "main", fail: true},
		{name: "lost write is observed without replay", change: "lost", write: true},
		{name: "backend replaced after write holds the next step", change: "post-backend", write: true, fail: true},
		{name: "unconfirmed write holds without replay", change: "hold", write: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
				"metadata": object{"name": "data", "namespace": "tenant", "uid": "claim-uid", "resourceVersion": "15"},
				"spec":     object{"volumeName": "pvc-claim-uid", "storageClassName": "longhorn-wffc"},
				"status":   object{"phase": "Bound"}}
			pv := object{"apiVersion": "v1", "kind": "PersistentVolume",
				"metadata": object{"name": "pvc-claim-uid", "uid": "pv-uid", "resourceVersion": "25"},
				"spec": object{"claimRef": object{"name": "data", "namespace": "tenant", "uid": "claim-uid"}, "storageClassName": "longhorn-wffc",
					"persistentVolumeReclaimPolicy": "Delete", "csi": object{"driver": "driver.longhorn.io", "volumeHandle": "pvc-claim-uid"}},
				"status": object{"phase": "Bound"}}
			backend := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
				"metadata": object{"name": "pvc-claim-uid", "namespace": "longhorn-system", "uid": "volume-uid", "resourceVersion": "35"},
				"spec":     object{"dataEngine": "v1"},
				"status":   object{"kubernetesStatus": object{"pvName": "pvc-claim-uid"}}}
			claimMeta := claim["metadata"].(object)
			pvMeta, pvSpec := pv["metadata"].(object), pv["spec"].(object)
			switch tc.change {
			case "retained":
				pvSpec["persistentVolumeReclaimPolicy"] = "Retain"
			case "claim":
				claimMeta["uid"] = "replacement"
				pvSpec["claimRef"].(object)["uid"] = "replacement"
			case "backend":
				backend["metadata"].(object)["uid"] = "replacement"
			case "deleting":
				pvMeta["deletionTimestamp"] = "2026-11-10T08:00:00Z"
			case "upload":
				claimMeta["ownerReferences"] = []any{object{"apiVersion": "velero.io/v2alpha1", "kind": "DataUpload", "uid": "upload-uid"}}
			}
			writes, mainReads := 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, body []byte, cmd string, args ...string) ([]byte, error) {
				if cmd == "git" {
					mainReads++
					if tc.change == "main" && mainReads >= 2 {
						return nil, errors.New("main advanced")
					}
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				request := strings.Join(args, " ")
				switch {
				case strings.Contains(request, "get persistentvolumeclaim data "):
					return json.Marshal(claim)
				case strings.Contains(request, "get persistentvolume pvc-claim-uid "):
					return json.Marshal(pv)
				case strings.Contains(request, "get volumes.longhorn.io pvc-claim-uid "):
					return json.Marshal(backend)
				case strings.Contains(request, "patch persistentvolume pvc-claim-uid "):
					writes++
					var changes []object
					want := []object{
						{"op": "test", "path": "/metadata/uid", "value": "pv-uid"},
						{"op": "test", "path": "/metadata/resourceVersion", "value": "25"},
						{"op": "test", "path": "/spec/claimRef", "value": object{"name": "data", "namespace": "tenant", "uid": "claim-uid"}},
						{"op": "test", "path": "/spec/csi/volumeHandle", "value": "pvc-claim-uid"},
						{"op": "test", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Delete"},
						{"op": "replace", "path": "/spec/persistentVolumeReclaimPolicy", "value": "Retain"},
					}
					if json.Unmarshal(body, &changes) != nil || !reflect.DeepEqual(changes, want) {
						t.Fatalf("patch changes more than the confirmed reclaim policy: %s", body)
					}
					if tc.change != "hold" {
						pvSpec["persistentVolumeReclaimPolicy"] = "Retain"
						pvMeta["resourceVersion"] = "26"
					}
					if tc.change == "post-backend" {
						backend["metadata"].(object)["uid"] = "replacement"
					}
					if tc.change == "hold" || tc.change == "lost" {
						return nil, errors.New("write response lost")
					}
					return []byte("persistentvolume/pvc-claim-uid"), nil
				default:
					t.Fatalf("unexpected request %q %q", cmd, args)
					return nil, nil
				}
			}}
			err := retainV1Source(t.Context(), c, source, retentionApproval)
			if (err != nil) != tc.fail || (writes == 1) != tc.write || writes > 1 {
				t.Fatalf("expected write=%v fail=%v, got writes=%d error=%v", tc.write, tc.fail, writes, err)
			}
		})
	}
}

func TestSourceRetentionRequiresExactClaimConfirmation(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = "retain-v1-source"
	env["LONGHORN_V2_NODE"] = ""
	env["LONGHORN_V2_CONFIRM"] = retentionApproval
	getenv := func(key string) string { return env[key] }
	if err := authorizeV1SourceRetention(getenv); err != nil {
		t.Fatal(err)
	}
	for key, foreign := range map[string]string{
		"LONGHORN_V2_OPERATION": "benchmark-v1",
		"LONGHORN_V2_NODE":      "prod-worker-1",
		"LONGHORN_V2_CONFIRM":   strings.Replace(retentionApproval, "tenant/", "velero/", 1),
		"GITHUB_RUN_ATTEMPT":    "2",
		"GITHUB_REF":            "refs/heads/candidate",
	} {
		original := env[key]
		env[key] = foreign
		if authorizeV1SourceRetention(getenv) == nil {
			t.Fatalf("foreign dispatch authorized with %s=%q", key, foreign)
		}
		env[key] = original
	}
}
