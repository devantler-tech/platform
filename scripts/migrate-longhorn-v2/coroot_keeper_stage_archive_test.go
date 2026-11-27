package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestKeeperRollbackArchivesOnlyAnUnselectedPendingStage(t *testing.T) {
	for _, name := range []string{"unscheduled", "selected", "PV allocated", "backend allocated", "replacement claim"} {
		t.Run(name, func(t *testing.T) {
			const uid = "06c31a15-6cb3-4ad3-b6b5-63fd5c0c9a4e"
			const run = "991"
			source := strings.Repeat("a", 40)
			claim := corootKeeperStagingClaim(run, source)
			valueObject(claim, "metadata")["uid"] = uid
			valueObject(claim, "metadata")["resourceVersion"] = "12"
			claim["status"] = object{"phase": "Pending"}
			record := corootKeeperCutoverRecord{Run: run, Source: source, StagedClaim: benchmarkIdentity(claim)}
			class := object{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
				"metadata":    object{"name": "longhorn-v2", "uid": uid, "resourceVersion": "7"},
				"provisioner": "driver.longhorn.io", "reclaimPolicy": "Retain", "volumeBindingMode": "WaitForFirstConsumer",
				"parameters": object{"backupTargetName": "default", "dataEngine": "v2", "dataLocality": "disabled",
					"fsType": "ext4", "numberOfReplicas": "3", "staleReplicaTimeout": "30", "unmapMarkSnapChainRemoved": "disabled"},
			}
			reads := 0
			c := client{context: "admin@prod", run: func(_ context.Context, stdin []byte, command string, args ...string) ([]byte, error) {
				if stdin != nil || command != "kubectl" || len(args) < 4 || args[2] != "get" {
					t.Fatalf("unexpected access: %s %v", command, args)
				}
				switch args[3] {
				case "pods":
					return []byte(`{"items":[]}`), nil
				case "storageclass":
					return json.Marshal(class)
				case "persistentvolumeclaim":
					reads++
					if name == "selected" && reads == 2 {
						valueObject(claim, "metadata", "annotations")["volume.kubernetes.io/selected-node"] = "worker"
					}
					if name == "replacement claim" && reads == 2 {
						valueObject(claim, "metadata")["uid"] = "019bf822-ce0d-4fd5-89f4-0fe058205e5f"
					}
					return json.Marshal(claim)
				case "persistentvolume", "volumes.longhorn.io":
					if args[4] != "pvc-"+uid {
						t.Fatalf("unexpected staged resource name: %s", args[4])
					}
					if (name == "PV allocated" && args[3] == "persistentvolume") ||
						(name == "backend allocated" && args[3] == "volumes.longhorn.io") {
						return json.Marshal(object{"metadata": object{"name": args[4]}})
					}
					return nil, nil
				default:
					t.Fatalf("unexpected resource: %s", args[3])
					return nil, nil
				}
			}}
			journal := corootKeeperJournal{c: c}
			stage, err := journal.observeKeeperStagingBeforeArchive(t.Context(), record)
			if (err == nil) != (name == "unscheduled") || stage != (corootKeeperRetainedStage{}) {
				t.Fatalf("staging = %+v, err = %v", stage, err)
			}
		})
	}
}
