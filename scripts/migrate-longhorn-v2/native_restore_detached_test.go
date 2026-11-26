package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func nativeRestoreDetachedFixture(t *testing.T) (benchmarkRecord, canarySeedState, nativeRestoreState, nativeRestoreDetachedObservation) {
	t.Helper()
	record, seed, _, attached := nativeRestoreAttachedFixture(t, false)
	_, _, state := nativeRestoreStateFixture(t, "first-pod-deleted")
	observed := nativeRestoreDetachedObservation{Claim: attached.Claim, Class: attached.Class, PV: attached.PV, Inventory: attached.Inventory,
		Frontends: attached.Frontends, Attachments: []object{}, CSITickets: []object{}}
	observed.Inventory.pods = []object{}
	volume := observed.Inventory.volumes[0]
	valueObject(volume, "spec")["nodeID"] = ""
	valueObject(volume, "status")["currentNodeID"] = ""
	valueObject(volume, "status")["currentEngineNodeID"] = ""
	valueObject(volume, "status")["state"] = "detached"
	valueObject(volume, "status")["robustness"] = "unknown"
	for _, instances := range [][]object{observed.Inventory.engines, observed.Inventory.replicas, observed.Frontends} {
		for _, instance := range instances {
			valueObject(instance, "spec")["desireState"] = "stopped"
			valueObject(instance, "status")["currentState"] = "stopped"
			valueObject(instance, "status")["endpoint"] = ""
		}
	}
	return record, seed, state, observed
}

func TestNativeRestoreDetachPreservesTheOriginalClaimAndWaitsForEveryAttachment(t *testing.T) {
	for _, fault := range []string{"", "wrong phase", "missing pods", "reused pod name", "other pod mounts claim", "same UID in another namespace",
		"missing attachment list", "pending Longhorn ticket", "remaining Longhorn ticket status", "missing CSI list", "CSI attachment pending", "CSI source unknown", "CSI UID aliases old consumer",
		"foreign volume", "lost copy", "replica still running", "extra inactive replica running", "engine still stopping", "frontend still running", "stopped frontend endpoint",
		"foreign replica owner", "replaced PV", "desired engine node remains", "detaching"} {
		t.Run(fault, func(t *testing.T) {
			record, seed, state, observed := nativeRestoreDetachedFixture(t)
			pod := object{"metadata": object{"name": "fio", "namespace": benchmarkNamespace, "uid": "replacement-consumer", "resourceVersion": "22"}}
			lhAttachment := object{"metadata": object{"name": "target-attachment", "uid": "attachment-original", "resourceVersion": "22"},
				"spec": object{"volume": state.Resources["LonghornVolume"].Name}, "status": object{}}
			csi := object{"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttachment", "metadata": object{"name": "csi-another-attachment", "uid": "csi-original", "resourceVersion": "22"},
				"spec": object{"source": object{"persistentVolumeName": "pv-unrelated"}}, "status": object{"attached": false}}
			switch fault {
			case "wrong phase":
				state.Phase = "write-verified"
			case "missing pods":
				observed.Inventory.pods = nil
			case "reused pod name":
				observed.Inventory.pods = []object{pod}
			case "other pod mounts claim":
				valueObject(pod, "metadata")["name"] = "different-pod"
				pod["spec"] = object{"volumes": []any{object{"name": "claim", "persistentVolumeClaim": object{"claimName": "fio-data"}}}}
				observed.Inventory.pods = []object{pod}
			case "same UID in another namespace":
				valueObject(pod, "metadata")["namespace"] = "other"
				valueObject(pod, "metadata")["uid"] = state.Resources["FirstPod"].UID
				observed.Inventory.pods = []object{pod}
			case "missing attachment list":
				observed.Attachments = nil
			case "pending Longhorn ticket":
				valueObject(lhAttachment, "spec")["attachmentTickets"] = object{"pending": object{}}
				observed.Attachments = []object{lhAttachment}
			case "remaining Longhorn ticket status":
				valueObject(lhAttachment, "status")["attachmentTicketStatuses"] = object{"stale": object{}}
				observed.Attachments = []object{lhAttachment}
			case "missing CSI list":
				observed.CSITickets = nil
			case "CSI attachment pending":
				valueObject(csi, "spec", "source")["persistentVolumeName"] = state.Resources["PersistentVolume"].Name
				observed.CSITickets = []object{csi}
			case "CSI source unknown":
				delete(valueObject(csi, "spec"), "source")
				observed.CSITickets = []object{csi}
			case "CSI UID aliases old consumer":
				valueObject(csi, "metadata")["uid"] = state.Resources["FirstPod"].UID
				observed.CSITickets = []object{csi}
			case "foreign volume":
				valueObject(observed.Inventory.volumes[0], "metadata")["uid"] = "replacement-backend"
			case "lost copy":
				observed.Inventory.replicas = observed.Inventory.replicas[:2]
			case "replica still running":
				valueObject(observed.Inventory.replicas[0], "status")["currentState"] = "running"
			case "extra inactive replica running":
				replica := observed.Inventory.replicas[0]
				extra := object{"apiVersion": replica["apiVersion"], "kind": replica["kind"], "metadata": object{
					"name": "extra-replica", "uid": "extra-replica-uid", "namespace": "longhorn-system", "resourceVersion": "22", "ownerReferences": value(replica, "metadata", "ownerReferences")},
					"spec": object{"volumeName": state.Resources["LonghornVolume"].Name, "active": false, "dataEngine": "v2", "desireState": "stopped"}, "status": object{"currentState": "running"}}
				observed.Inventory.replicas = append(observed.Inventory.replicas, extra)
			case "engine still stopping":
				valueObject(observed.Inventory.engines[0], "status")["currentState"] = "stopping"
			case "frontend still running":
				valueObject(observed.Frontends[0], "status")["currentState"] = "running"
			case "stopped frontend endpoint":
				valueObject(observed.Frontends[0], "status")["endpoint"] = "/dev/longhorn/reconnected"
			case "foreign replica owner":
				value(observed.Inventory.replicas[0], "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "replacement-backend"
			case "replaced PV":
				valueObject(observed.PV, "metadata")["uid"] = "replacement-pv"
			case "desired engine node remains":
				valueObject(observed.Inventory.volumes[0], "spec")["engineNodeID"] = state.FirstNode
			case "detaching":
				valueObject(observed.Inventory.volumes[0], "status")["state"] = "detaching"
			default:
				observed.Attachments = []object{lhAttachment}
				observed.CSITickets = []object{csi}
			}
			err := verifyNativeRestoreDetached(record, seed, state, observed)
			if (err != nil) != (fault != "") {
				t.Fatalf("incorrect detached restore verdict for %q: %v", fault, err)
			}
		})
	}
}

func TestNativeRestoreDetachObserverReadsBothAttachmentKindsAcrossTwoSamples(t *testing.T) {
	for _, fault := range []string{"", "unauthorized", "stale source", "CSI attach appeared", "unread continuation", "recreated first Pod"} {
		t.Run(fault, func(t *testing.T) {
			record, seed, state, observed := nativeRestoreDetachedFixture(t)
			for key, value := range nativeRestoreDispatch(state) {
				t.Setenv(key, value)
			}
			counts := map[string]int{}
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
				if input != nil {
					t.Fatal("detach observation supplied a write body")
				}
				if name == "git" {
					counts["source"]++
					source := state.Source
					if fault == "stale source" && counts["source"] == 2 {
						source = strings.Repeat("f", 40)
					}
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				if name != "kubectl" || len(args) < 5 || args[0] != "--context=admin@prod" || args[2] != "get" {
					t.Fatal("detach observation escaped its read-only context", name, args)
				}
				kind := args[3]
				counts[kind]++
				s := observed.Inventory
				switch kind {
				case "persistentvolumeclaim":
					return json.Marshal(observed.Claim)
				case "persistentvolume":
					return json.Marshal(observed.PV)
				case "storageclass":
					return json.Marshal(observed.Class)
				}
				lists := map[string][]object{
					"nodes": s.nodes, "nodes.longhorn.io": s.storageNodes, "volumes.longhorn.io": s.volumes, "pods": s.pods, "orphans.longhorn.io": {},
					"replicas.longhorn.io": s.replicas, "engines.longhorn.io": s.engines, "enginefrontends.longhorn.io": observed.Frontends,
					"volumeattachments.longhorn.io": observed.Attachments, "volumeattachments.storage.k8s.io": observed.CSITickets,
				}
				items, ok := lists[kind]
				if !ok || args[4] != "--output=json" {
					t.Fatal("detach observation requested an unexpected collection", args)
				}
				namespace := "longhorn-system"
				switch kind {
				case "nodes", "volumeattachments.storage.k8s.io":
					namespace = ""
				case "pods":
					namespace = "*"
				}
				if strings.Join(args[5:], " ") != strings.Join(namespaceArgs(namespace), " ") {
					t.Fatal("detach observation changed the collection scope", args)
				}
				result := object{"items": items}
				if counts[kind] == 2 {
					switch {
					case kind == "volumeattachments.longhorn.io" && fault == "unread continuation":
						result["metadata"] = object{"continue": "more-items"}
					case kind == "volumeattachments.storage.k8s.io" && fault == "CSI attach appeared":
						result["items"] = []object{{"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttachment",
							"metadata": object{"name": "new-attachment", "uid": "new-csi-uid", "resourceVersion": "24"},
							"spec":     object{"source": object{"persistentVolumeName": state.Resources["PersistentVolume"].Name}}}}
					case kind == "pods" && fault == "recreated first Pod":
						result["items"] = []object{{"metadata": object{"name": "fio", "namespace": benchmarkNamespace, "uid": "replacement-pod"}}}
					}
				}
				return json.Marshal(result)
			}}
			if fault == "unauthorized" {
				c.context = "oidc@prod"
			}
			err := observeNativeRestoreDetached(t.Context(), c, record, seed, state)
			if (err != nil) != (fault != "") {
				t.Fatalf("incorrect live observer verdict for %q: %v", fault, err)
			}
			if fault == "" && (counts["source"] != 2 || counts["volumeattachments.longhorn.io"] != 2 || counts["volumeattachments.storage.k8s.io"] != 2) {
				t.Fatal("detach proof did not span two complete attachment observations", counts)
			}
			if fault == "unauthorized" && len(counts) != 0 {
				t.Fatal("an unauthorized observation reached the cluster", counts)
			}
		})
	}
}
