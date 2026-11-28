package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestKeeperFenceReleaseRechecksDestinationAfterBindingDisappears(t *testing.T) {
	for _, remaining := range []string{"policy only", "still bound", "binding read failed"} {
		t.Run(remaining, func(t *testing.T) {
			checkedClaim := false
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || program != "kubectl" || len(args) < 5 || args[2] != "get" {
					t.Fatal("a failed destination read must stop before a write")
				}
				switch args[3] {
				case "statefulset":
					return nil, errors.New("the former two-member observation has expired")
				case keeperBindingKind:
					switch remaining {
					case "still bound":
						return []byte(`{"metadata":{"uid":"recorded-binding","resourceVersion":"31"}}`), nil
					case "binding read failed":
						return nil, errors.New("binding presence could not be resolved")
					default:
						return nil, nil
					}
				case keeperPolicyKind:
					return []byte(`{"metadata":{"uid":"recorded-policy","resourceVersion":"12"}}`), nil
				case "persistentvolumeclaim":
					checkedClaim = true
					return nil, errors.New("destination binding is unverified")
				default:
					t.Fatalf("unexpected Kubernetes read: %s", args[3])
					return nil, nil
				}
			}}
			record := corootKeeperCutoverRecord{Phase: "destination-bound",
				NewClaim: benchmarkResource{Name: "keeper-destination", Namespace: corootKeeperNamespace}}
			j := corootKeeperJournal{c: c, record: record}
			fences := corootKeeperFences{pod: object{"metadata": object{"name": "keeper-owned-fence"}}}
			err := j.releaseKeeperPodFence(t.Context(), fences)
			if err == nil || checkedClaim != (remaining == "policy only") {
				t.Fatalf("destination rechecked=%t, error=%v", checkedClaim, err)
			}
		})
	}
}

func TestKeeperRebindRequeuesTheControllerOnlyAfterTheDestinationIsBound(t *testing.T) {
	const annotationPath = "/metadata/annotations/storage.platform.devantler.tech~1keeper-rebind-claim"
	source := strings.Repeat("a", 40)
	member := "coroot-clickhouse-keeper-0"
	keeper := benchmarkResource{Name: corootKeeperName, Namespace: corootKeeperNamespace, UID: "a4f66240-e801-4624-a98a-af06d7fbfb2f"}
	claim := benchmarkResource{Name: "data-" + member, Namespace: corootKeeperNamespace, UID: "d827f800-8471-40c6-815a-7e59617ee620"}
	pv := benchmarkResource{Name: "pvc-retained-v2", UID: "7860ea13-bc1d-4e4c-9a12-ed34c67dbbed"}
	identity := func(api, kind string, resource benchmarkResource) object {
		return object{"apiVersion": api, "kind": kind, "metadata": object{
			"name": resource.Name, "namespace": resource.Namespace, "uid": resource.UID, "resourceVersion": "51",
		}}
	}
	for _, tc := range []struct {
		name                         string
		fenced, unbound, existingPod bool
		wantPatch                    bool
	}{
		{name: "verified rebind", wantPatch: true},
		{name: "unreleased fence", fenced: true},
		{name: "missing destination binding", unbound: true},
		{name: "controller already created the member", existingPod: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer stop()
			statefulset := identity("apps/v1", "StatefulSet", keeper)
			valueObject(statefulset, "metadata")["annotations"] = object{"operator.example/owned": "keep"}
			statefulset["spec"] = object{"replicas": 3}
			replacement := identity("v1", "PersistentVolumeClaim", claim)
			valueObject(replacement, "metadata")["annotations"] = object{corootKeeperFenceOwner: "991:" + source}
			replacement["spec"] = object{"accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem",
				"storageClassName": "longhorn-v2", "volumeName": pv.Name,
				"resources": object{"requests": object{"storage": corootKeeperDataSize}}}
			replacement["status"] = object{"phase": "Bound"}
			if tc.unbound {
				valueObject(replacement, "status")["phase"] = "Pending"
			}
			volume := identity("v1", "PersistentVolume", pv)
			volume["status"] = object{"phase": "Bound"}
			volume["spec"] = object{"accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem",
				"storageClassName": "longhorn-v2", "persistentVolumeReclaimPolicy": "Retain",
				"claimRef": object{"name": claim.Name, "namespace": claim.Namespace, "uid": claim.UID},
				"csi":      object{"driver": "driver.longhorn.io", "volumeHandle": pv.Name}}
			patches := 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				switch args[2] + " " + args[3] {
				case "get statefulset":
					return json.Marshal(statefulset)
				case "get pod":
					if tc.existingPod {
						return json.Marshal(identity("v1", "Pod", benchmarkResource{Name: member, Namespace: claim.Namespace, UID: claim.UID}))
					}
					return nil, nil
				case "get " + keeperPolicyKind, "get " + keeperBindingKind:
					if tc.fenced {
						return []byte(`{"metadata":{"uid":"owned","resourceVersion":"9"}}`), nil
					}
					return nil, nil
				case "get persistentvolumeclaim":
					return json.Marshal(replacement)
				case "get persistentvolume":
					return json.Marshal(volume)
				case "patch statefulset":
					patches++
					var operations []object
					expected := append(identityTests(statefulset), object{"op": "add", "path": annotationPath, "value": claim.UID})
					if err := json.Unmarshal(input, &operations); err != nil || !reflect.DeepEqual(operations, expected) {
						t.Fatalf("unexpected reconcile patch: %s, %v", input, err)
					}
					stop()
					return []byte("statefulset.apps/" + keeper.Name), nil
				default:
					stop() // The test ends when the normal quorum read begins.
					return nil, errors.New("end of requeue fixture")
				}
			}}
			record := corootKeeperCutoverRecord{Run: "991", Source: source, Phase: "member-released",
				NewClaim: claim, Before: corootKeeperQuorumReceipt{Controller: keeper},
				Rebind: corootKeeperRebind{StagedPV: pv, StagedVolume: benchmarkResource{Name: pv.Name}}}
			j := corootKeeperJournal{c: c, record: record}
			_ = j.rebindAndResume(ctx, keeperFencesFor(record), nil)
			if (patches == 1) != tc.wantPatch {
				t.Fatalf("got %d reconcile writes, want one=%t", patches, tc.wantPatch)
			}
		})
	}
}
