package main

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

func TestBoundRecoveryRequiresExactUnacceptedScratch(t *testing.T) {
	record, _ := unstartedBenchmarkFixture(t)
	runtime, _ := retirementRuntimeInstanceFixture(t)
	record.V1Runtime = runtime.V1Runtime
	claim := record.Resources["PersistentVolumeClaim"]
	claim.UID = "ab0b1496-88bd-4104-bc7c-e8b0f0730931"
	record.Resources["PersistentVolumeClaim"] = claim
	handle := "pvc-" + claim.UID
	record.Resources["PersistentVolume"] = benchmarkResource{Name: handle, UID: "pv"}
	record.Resources["LonghornVolume"] = benchmarkResource{Name: handle, Namespace: "longhorn-system", UID: "backend"}
	record.CPU = benchmarkCPU{ClientRequest: "1", ClientLimit: "2"}

	if err := verifyBoundRecoveryRecord(record, record.Run); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*benchmarkRecord){
		"other run":               func(r *benchmarkRecord) { r.Run = "4321" },
		"accepted":                func(r *benchmarkRecord) { r.Cleaned = true },
		"measurement":             func(r *benchmarkRecord) { r.Measurements = make([]fioMeasurement, 1) },
		"unknown create response": func(r *benchmarkRecord) { r.PendingCreate = "Pod" },
		"missing runtime":         func(r *benchmarkRecord) { r.V1Runtime = nil },
		"missing backend":         func(r *benchmarkRecord) { delete(r.Resources, "LonghornVolume") },
		"foreign backend": func(r *benchmarkRecord) {
			r.Resources["LonghornVolume"] = benchmarkResource{Name: "other", Namespace: "longhorn-system", UID: "backend"}
		},
		"fence without recovery": func(r *benchmarkRecord) { r.FencePending = "unowned/policy" },
	} {
		t.Run(name, func(t *testing.T) {
			recordWithFault := record
			recordWithFault.Resources = maps.Clone(record.Resources)
			change(&recordWithFault)
			if verifyBoundRecoveryRecord(recordWithFault, record.Run) == nil {
				t.Fatal("unsafe recovery accepted")
			}
		})
	}
}

func TestBoundRecoveryRequiresItsOwnWorkflowAndConfirmation(t *testing.T) {
	env := map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/prepare-longhorn-v2-node.yaml@refs/heads/main",
		"GITHUB_SHA":          strings.Repeat("a", 40), "GITHUB_RUN_ID": "3456", "LONGHORN_V2_OPERATION": recoverBoundV1Operation,
		"LONGHORN_V2_CONFIRM": recoverBoundV1Confirmation + "1234",
	}
	get := func(key string) string { return env[key] }
	if err := authorizeBoundV1Recovery(get); err != nil {
		t.Fatal(err)
	}
	for key, invalid := range map[string]string{
		"LONGHORN_V2_CONFIRM": recoverBoundV1Confirmation, "LONGHORN_V2_OPERATION": recoverUnstartedV1Operation,
		"LONGHORN_V2_NODE": "prod-worker-1", "LONGHORN_V2_DATABASE": "coroot-db",
		"GITHUB_RUN_ATTEMPT": "2", "GITHUB_REF": "refs/heads/other",
	} {
		original := env[key]
		env[key] = invalid
		if authorizeBoundV1Recovery(get) == nil {
			t.Errorf("invalid %s accepted", key)
		}
		env[key] = original
	}
}

func TestBoundRecoveryValidatesOriginalStoppedStorageGraph(t *testing.T) {
	for _, fault := range []string{"", "replacement claim", "foreign PV", "live Pod", "missing original Pod", "recorded removal"} {
		t.Run(fault, func(t *testing.T) {
			record, observed := unstartedBenchmarkFixture(t)
			workspace := object{"apiVersion": "v1", "kind": "Namespace", "metadata": object{
				"name": record.Namespace, "uid": record.NamespaceUID, "resourceVersion": "1", "labels": object{
					"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest",
				}, "annotations": object{"kustomize.toolkit.fluxcd.io/prune": "disabled"},
			}, "status": object{"phase": "Active"}}
			observed["Namespace"] = workspace
			pod := observed["Pod"]
			pod["spec"].(object)["nodeName"] = record.clientNode()
			pod["status"] = object{"phase": "Failed", "containerStatuses": []any{object{
				"name": "fio", "imageID": fioImage,
			}}}
			claim := observed["PersistentVolumeClaim"]
			handle := "pvc-" + record.Resources["PersistentVolumeClaim"].UID
			claim["status"] = object{"phase": "Bound"}
			claim["spec"].(object)["volumeName"] = handle
			volume := resource(handle)
			volume["metadata"].(object)["namespace"] = "longhorn-system"
			volume["spec"] = object{"dataEngine": "v1", "accessMode": "rwo"}
			volume["status"] = object{"kubernetesStatus": object{"pvName": handle}}
			pv := resource(handle)
			pv["spec"] = object{
				"storageClassName":              text(observed["StorageClass"], "metadata", "name"),
				"persistentVolumeReclaimPolicy": "Retain", "csi": object{"driver": "driver.longhorn.io", "volumeHandle": handle},
				"claimRef": object{"uid": text(claim, "metadata", "uid"), "namespace": record.Namespace, "name": "fio-data"},
			}
			observed["PersistentVolume"], observed["LonghornVolume"] = pv, volume
			record.Resources["PersistentVolume"], record.Resources["LonghornVolume"] = benchmarkIdentity(pv), benchmarkIdentity(volume)
			var otherPV object
			switch fault {
			case "replacement claim":
				claim["metadata"].(object)["uid"] = "foreign-pvc"
			case "foreign PV":
				otherPV = resource("other-pv")
				otherPV["spec"] = object{"csi": object{"driver": "driver.longhorn.io", "volumeHandle": handle}}
			case "live Pod":
				pod["status"].(object)["phase"] = "Running"
			case "missing original Pod", "recorded removal":
				delete(observed, "Pod")
				if fault == "recorded removal" {
					record.Recovery = &benchmarkRecoveryOwner{Deletes: map[string]benchmarkResource{"Pod": record.Resources["Pod"]}}
				}
			}
			resources := map[string]string{
				"namespace": "Namespace", "pod": "Pod", "pods": "Pod", "storageclass": "StorageClass",
				"persistentvolumeclaim": "PersistentVolumeClaim", "persistentvolumeclaims": "PersistentVolumeClaim",
				"persistentvolume": "PersistentVolume", "persistentvolumes": "PersistentVolume",
				"volumes.longhorn.io": "LonghornVolume", "networkpolicy": "NetworkPolicy",
			}
			c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, command string, args ...string) ([]byte, error) {
				if command != "kubectl" || args[2] != "get" {
					t.Fatalf("unexpected mutation during recovery admission: %s %v", command, args)
				}
				o := observed[resources[args[3]]]
				if args[4] != "--output=json" {
					if o == nil {
						return nil, nil
					}
					return json.Marshal(o)
				}
				items := []object{}
				if o != nil {
					items = append(items, o)
				}
				if args[3] == "persistentvolumes" && otherPV != nil {
					items = append(items, otherPV)
				}
				return json.Marshal(object{"kind": "List", "items": items})
			}}
			_, err := observeBoundBenchmarkAssets(t.Context(), c, record)
			if allowed := fault == "" || fault == "recorded removal"; allowed != (err == nil) {
				t.Fatalf("recovery admission for %q: %v", fault, err)
			}
		})
	}
}
