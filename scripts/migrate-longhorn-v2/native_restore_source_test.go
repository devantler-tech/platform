package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func nativeRestoreAbsentSourceFixture() map[string][]object {
	groups := map[string][]object{}
	for _, resource := range []string{"persistentvolumes", "volumes.longhorn.io", "engines.longhorn.io", "replicas.longhorn.io", "enginefrontends.longhorn.io", "volumeattachments.longhorn.io", "snapshots.longhorn.io", "volumeattachments.storage.k8s.io"} {
		groups[resource] = []object{}
	}
	return groups
}

func TestNativeRestoreNeedsCurrentSourceAbsenceBeyondItsCleanupJournal(t *testing.T) {
	for _, fault := range []string{"", "original PV", "replacement PV name", "reused CSI handle", "original backend", "replacement backend name", "replica", "engine", "frontend", "pending CSI attachment", "Longhorn attachment", "snapshot", "owner-only association", "label-only association", "unread list", "missing list", "extra list", "missing child association", "malformed owner", "duplicate identity", "unfinished seed"} {
		t.Run(fault, func(t *testing.T) {
			record, seed := seedStateFixture(t, "source-cleaned")
			groups := nativeRestoreAbsentSourceFixture()
			backend, pv := record.Resources["LonghornVolume"], record.Resources["PersistentVolume"]
			child := func(name string) object {
				return object{"metadata": object{"name": name, "namespace": "longhorn-system", "uid": name + "-uid", "resourceVersion": "7"}, "spec": object{"volumeName": "another-volume"}}
			}
			switch fault {
			case "original PV", "replacement PV name", "reused CSI handle":
				v := object{"metadata": object{"name": pv.Name, "uid": pv.UID, "resourceVersion": "9"}, "spec": object{"csi": object{"driver": "driver.longhorn.io", "volumeHandle": backend.Name}}}
				if fault == "replacement PV name" {
					v["metadata"].(object)["uid"] = "replacement-pv-uid"
				}
				if fault == "reused CSI handle" {
					v["metadata"] = object{"name": "another-pv", "uid": "another-pv-uid", "resourceVersion": "10"}
				}
				groups["persistentvolumes"] = []object{v}
			case "original backend", "replacement backend name":
				v := child(backend.Name)
				v["metadata"].(object)["uid"] = backend.UID
				if fault == "replacement backend name" {
					v["metadata"].(object)["uid"] = "replacement-backend-uid"
				}
				groups["volumes.longhorn.io"] = []object{v}
			case "replica", "engine", "frontend", "Longhorn attachment", "snapshot", "pending CSI attachment":
				resource := map[string]string{"replica": "replicas.longhorn.io", "engine": "engines.longhorn.io", "frontend": "enginefrontends.longhorn.io", "Longhorn attachment": "volumeattachments.longhorn.io", "snapshot": "snapshots.longhorn.io", "pending CSI attachment": "volumeattachments.storage.k8s.io"}[fault]
				v := child("remaining-child")
				v["spec"] = object{"volumeName": backend.Name}
				if fault == "Longhorn attachment" || fault == "snapshot" {
					v["spec"] = object{"volume": backend.Name}
				}
				if fault == "pending CSI attachment" {
					delete(v["metadata"].(object), "namespace")
					v["spec"] = object{"source": object{"persistentVolumeName": pv.Name}}
					v["status"] = object{"attached": false}
				}
				groups[resource] = []object{v}
			case "owner-only association", "label-only association", "missing child association", "malformed owner":
				v := child("ambiguous-child")
				switch fault {
				case "owner-only association":
					v["metadata"].(object)["ownerReferences"] = []any{object{"name": backend.Name, "uid": backend.UID}}
				case "label-only association":
					v["metadata"].(object)["labels"] = object{"longhornvolume": backend.Name}
				case "missing child association":
					v["spec"] = object{}
				case "malformed owner":
					v["metadata"].(object)["ownerReferences"] = "unreadable"
				}
				groups["replicas.longhorn.io"] = []object{v}
			case "unread list":
				groups["engines.longhorn.io"] = nil
			case "missing list":
				delete(groups, "enginefrontends.longhorn.io")
			case "extra list":
				groups["unknown"] = []object{}
			case "duplicate identity":
				v := child("duplicated")
				groups["replicas.longhorn.io"], groups["engines.longhorn.io"] = []object{v}, []object{v}
			case "unfinished seed":
				seed.Phase = "backup-verified"
			}
			err := verifyNativeRestoreSourceAbsent(record, seed, groups)
			if (err != nil) != (fault != "") {
				t.Fatalf("source absence verdict for %q: %v", fault, err)
			}
		})
	}
}

func TestNativeRestoreSourceAbsenceAllowsOtherApplicationStorage(t *testing.T) {
	record, seed := seedStateFixture(t, "source-cleaned")
	groups := nativeRestoreAbsentSourceFixture()
	for _, resource := range []string{"engines.longhorn.io", "replicas.longhorn.io", "enginefrontends.longhorn.io", "volumeattachments.longhorn.io", "snapshots.longhorn.io"} {
		association := "volumeName"
		if resource == "volumeattachments.longhorn.io" || resource == "snapshots.longhorn.io" {
			association = "volume"
		}
		groups[resource] = []object{{"metadata": object{"name": resource + "-other", "namespace": "longhorn-system", "uid": resource + "-uid", "resourceVersion": "1"}, "spec": object{association: "application-data"}}}
	}
	groups["persistentvolumes"] = []object{{"metadata": object{"name": "other-pv", "uid": "other-pv-uid", "resourceVersion": "1"}, "spec": object{"csi": object{"driver": "driver.longhorn.io", "volumeHandle": "application-data"}}}}
	groups["volumes.longhorn.io"] = []object{{"metadata": object{"name": "application-data", "namespace": "longhorn-system", "uid": "application-volume-uid", "resourceVersion": "1"}}}
	groups["volumeattachments.storage.k8s.io"] = []object{{"metadata": object{"name": "other-attachment", "uid": "other-attachment-uid", "resourceVersion": "1"}, "spec": object{"source": object{"persistentVolumeName": "other-pv"}}}}
	if err := verifyNativeRestoreSourceAbsent(record, seed, groups); err != nil {
		t.Fatal("unrelated application storage blocked the dedicated source absence check:", err)
	}
}

func TestNativeRestoreSourceObserverRejectsFailedAndIncompleteReads(t *testing.T) {
	for _, fault := range []string{"", "request failed", "invalid JSON", "continued list", "missing items"} {
		t.Run(fault, func(t *testing.T) {
			record, seed := seedStateFixture(t, "source-cleaned")
			reads := 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if reads >= len(nativeRestoreSourceQueries) {
					t.Fatal("source observer escaped its fixed read surface")
				}
				query := nativeRestoreSourceQueries[reads]
				expected := []string{"--context=admin@prod", "--request-timeout=20s", "get", query.resource, "--output=json"}
				if query.namespace != "" {
					expected = append(expected, "--namespace="+query.namespace)
				}
				if program != "kubectl" || input != nil || !slices.Equal(args, expected) {
					t.Fatal("source absence observation selected an unexpected command or target")
				}
				reads++
				if reads == 3 {
					switch fault {
					case "request failed":
						return nil, errors.New("unavailable")
					case "invalid JSON":
						return []byte("unreadable"), nil
					case "continued list":
						return []byte(`{"metadata":{"continue":"unread"},"items":[]}`), nil
					case "missing items":
						return []byte(`{"metadata":{}}`), nil
					}
				}
				return []byte(`{"metadata":{},"items":[]}`), nil
			}}
			err := observeNativeRestoreSourceAbsent(t.Context(), c, record, seed)
			if (err != nil) != (fault != "") || fault == "" && reads != len(nativeRestoreSourceQueries) || fault != "" && reads != 3 {
				t.Fatalf("an incomplete source read became absence evidence: fault=%q reads=%d err=%v", fault, reads, err)
			}
		})
	}
}

func TestNativeRestoreSeedObserverUsesTheRetainedBackupAfterSourceRemoval(t *testing.T) {
	for _, fault := range []string{"", "wrong context", "wrong grant", "stale source", "source survived", "changed snapshot", "changed location", "replaced journal", "changed seed journal"} {
		t.Run(fault, func(t *testing.T) {
			record, seed := seedStateFixture(t, "source-cleaned")
			_, retained, backup, _, transfer, repository := retainedSeedBackupFixture(t)
			location, schedules, _ := seedBackupInputFixture()
			valueObject(location, "status")["lastValidationTime"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			seed.RepositorySpec = retained.RepositorySpec
			var err error
			seed.LocationSpec, err = seedSpecDigest(location)
			if err != nil {
				t.Fatal(err)
			}
			_, _, restore := nativeRestoreStateFixture(t, "created")
			for key, value := range nativeRestoreDispatch(restore) {
				t.Setenv(key, value)
			}
			if fault == "wrong grant" {
				t.Setenv("LONGHORN_V2_OPERATION", nativeSeedOperation)
			}
			groups := nativeRestoreAbsentSourceFixture()
			if fault == "source survived" {
				groups["volumes.longhorn.io"] = []object{{"metadata": object{"name": record.Resources["LonghornVolume"].Name, "uid": "still-present", "namespace": "longhorn-system", "resourceVersion": "3"}}}
			}
			if fault == "changed snapshot" {
				valueObject(transfer, "status")["snapshotID"] = "some-other-snapshot"
			}
			if fault == "changed location" {
				valueObject(location, "spec", "objectStorage")["prefix"] = "some-other-location"
			}
			reads := map[string]int{}
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil {
					t.Fatal("source proof attempted a write")
				}
				if program == "git" {
					reads["source"]++
					source := restore.Source
					if fault == "stale source" && reads["source"] == 2 {
						source = strings.Repeat("f", 40)
					}
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 5 || args[0] != "--context=admin@prod" || args[2] != "get" {
					t.Fatal("native seed proof left its read-only scope", program, args)
				}
				kind := args[3]
				reads[kind]++
				switch kind {
				case "configmap":
					if args[4] != nativeSeedJournal {
						t.Fatal("the seed proof read another journal")
					}
					id := restore.SeedJournal.UID
					current := seed
					if reads[kind] > 1 && fault == "replaced journal" {
						id = "replacement-journal"
					}
					if reads[kind] > 1 && fault == "changed seed journal" {
						current.PayloadSHA256 = strings.Repeat("f", 64)
					}
					recordData, _ := json.Marshal(record)
					seedData, _ := json.Marshal(current)
					return json.Marshal(object{"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": object{"name": nativeSeedJournal, "namespace": "longhorn-system", "uid": id, "resourceVersion": "17"},
						"data":     object{"record": string(recordData), "seed": string(seedData)}})
				case "backups.velero.io":
					return json.Marshal(backup)
				case "backupstoragelocations.velero.io":
					return json.Marshal(location)
				case "schedules.velero.io":
					return json.Marshal(object{"items": schedules})
				case "podvolumebackups.velero.io":
					return json.Marshal(object{"items": []object{transfer}})
				case "backuprepositories.velero.io":
					return json.Marshal(object{"items": []object{repository}})
				}
				if items, ok := groups[kind]; ok {
					return json.Marshal(object{"items": items})
				}
				t.Fatal("restore asked for an unreviewed resource or tried to fetch the deleted seed Pod", args)
				return nil, nil
			}}
			if fault == "wrong context" {
				c.context = "oidc@prod"
			}
			got, err := observeNativeRestoreSeed(t.Context(), c, restore.Source, restore.Run)
			if (err != nil) != (fault != "") {
				t.Fatalf("incorrect retained seed verdict for %q: %v", fault, err)
			}
			if fault == "" && (got.Journal != restore.SeedJournal || !reflect.DeepEqual(got.Record, record) || got.Seed != seed ||
				reads["source"] != 2 || reads["configmap"] != 2 || reads["podvolumebackups.velero.io"] != 1) {
				t.Fatal("restore did not bind the original seed journal and data transfer across both source reads", reads)
			}
			if fault != "" && !reflect.DeepEqual(got, nativeRestoreSeedReceipt{}) {
				t.Fatal("unconfirmed seed evidence escaped the observer")
			}
		})
	}
}
