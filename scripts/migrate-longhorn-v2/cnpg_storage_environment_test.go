package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// recoveryStorageAPI keeps an actual activation fixture and adds one healthy existing V2 allocation.
func recoveryStorageAPI(t *testing.T) (*activationAPI, temporaryV2Activation) {
	t.Helper()
	f, a := newActivationAPI(t)
	if err := a.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, record, err := a.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	v, err := cloneRecoverySpec(f.s.volumes[0])
	if err != nil {
		t.Fatal(err)
	}
	v["metadata"].(object)["name"], v["metadata"].(object)["uid"] = "existing-v2", "existing-v2-uid"
	v["spec"].(object)["dataEngine"] = "v2"
	engine, err := cloneRecoverySpec(f.s.engines[0])
	if err != nil {
		t.Fatal(err)
	}
	engine["metadata"].(object)["name"], engine["metadata"].(object)["uid"] = "existing-v2-engine", "existing-engine-uid"
	engine["spec"].(object)["volumeName"], engine["spec"].(object)["dataEngine"] = "existing-v2", "v2"
	engine["status"].(object)["replicaModeMap"] = object{}
	for i, selection := range temporaryV2Selections {
		binding := record.Targets[selection]
		var storage, manager object
		for _, n := range f.s.storageNodes {
			if text(n, "metadata", "name") == binding.Name {
				storage = n
			}
		}
		for _, m := range f.managers {
			if text(m, "spec", "nodeID") == binding.Name {
				manager = m
			}
		}
		disk := value(storage, "status", "diskStatus", "longhorn-v2").(object)
		path := text(storage, "spec", "disks", "longhorn-v2", "path")
		r, e := cloneRecoverySpec(f.s.replicas[i])
		if e != nil {
			t.Fatal(e)
		}
		name := fmt.Sprintf("existing-v2-replica-%d", i)
		r["metadata"].(object)["name"], r["metadata"].(object)["uid"] = name, name+"-uid"
		r["spec"].(object)["volumeName"], r["spec"].(object)["dataEngine"], r["spec"].(object)["engineName"] = "existing-v2", "v2", "existing-v2-engine"
		r["spec"].(object)["nodeID"], r["spec"].(object)["diskID"], r["spec"].(object)["diskPath"], r["spec"].(object)["volumeSize"] = binding.Name, text(disk, "diskUUID"), path, "10737418240"
		engine["status"].(object)["replicaModeMap"].(object)[name] = "RW"
		disk["conditions"] = []any{object{"type": "Ready", "status": "True"}, object{"type": "Schedulable", "status": "True"}}
		disk["diskPath"], disk["diskType"], disk["diskDriver"], disk["instanceManagerName"] = path, "block", "aio", text(manager, "metadata", "name")
		disk["storageMaximum"], disk["storageAvailable"], disk["storageScheduled"] = float64(143*recoveryGiB), float64(110*recoveryGiB), float64(10*recoveryGiB)
		disk["scheduledReplica"], disk["scheduledBackingImage"] = object{name: float64(10 * recoveryGiB)}, object{}
		f.s.replicas = append(f.s.replicas, r)
	}
	f.s.volumes, f.s.engines = append(f.s.volumes, v), append(f.s.engines, engine)
	f.writes = 0
	a.owner = "9999"
	delegate := a.c.run
	a.c.context = "admin@prod"
	a.c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if len(input) != 0 {
			t.Fatal("storage qualification attempted a write")
		}
		if program == "kubectl" {
			if len(args) < 4 || args[0] != "--context=admin@prod" || args[2] != "get" {
				t.Fatal("storage qualification escaped protected read scope")
			}
			if args[3] == "backingimages.longhorn.io" || args[3] == "backingimagedatasources.longhorn.io" || args[3] == "shards.longhorn.io" || args[3] == "shardgroups.longhorn.io" {
				return json.Marshal(object{"items": []any{}})
			}
			args = append([]string{}, args...)
			args[0] = "--context=fixture"
		}
		return delegate(ctx, input, program, args...)
	}
	return f, a
}

// TestRecoveryStorageRequalifiesActiveV2WithoutRepeatingRegistration permits subsequent recovery trials.
func TestRecoveryStorageRequalifiesActiveV2WithoutRepeatingRegistration(t *testing.T) {
	f, a := recoveryStorageAPI(t)
	got, err := a.recoveryStorage(t.Context(), cnpgRecoverySource{Namespace: "umami", Cluster: "umami-db", Size: "5Gi"})
	if err != nil || len(got.Disks) != 3 || len(got.Targets) != 3 || got.Activation.UID != "activation-journal-uid" || f.writes != 0 {
		t.Fatalf("qualified active V2 storage refused: %+v %v", got, err)
	}
}

func TestRecoveryStorageChecksActualBudgetClaimCapacity(t *testing.T) {
	f, a := recoveryStorageAPI(t)
	got, err := a.recoveryClaimStorage(t.Context(), "10Gi")
	if err != nil || len(got.Disks) != 3 || f.writes != 0 {
		t.Fatalf("qualified 10 GiB claim refused: %v", err)
	}
	if _, err := a.recoveryStorage(t.Context(), cnpgRecoverySource{Namespace: "unreviewed", Cluster: "umami-db", Size: "5Gi"}); err == nil {
		t.Fatal("a database in the wrong namespace was accepted")
	}
}

// TestRecoveryStorageRefusesLostOwnershipOrCapacity prevents trusting an old activation completion bit.
func TestRecoveryStorageRefusesLostOwnershipOrCapacity(t *testing.T) {
	for _, scenario := range []string{"capacity lost", "registration replaced", "partition changed", "unfinished activation", "foreign V1 allocation"} {
		t.Run(scenario, func(t *testing.T) {
			f, a := recoveryStorageAPI(t)
			var record temporaryActivationRecord
			if json.Unmarshal([]byte(text(f.record, "data", "record")), &record) != nil {
				t.Fatal("invalid activation fixture")
			}
			target := record.Targets["temporary-v1-1"]
			for _, n := range f.s.storageNodes {
				if text(n, "metadata", "name") != target.Name {
					continue
				}
				if scenario == "capacity lost" {
					value(n, "status", "diskStatus", "longhorn-v2").(object)["storageAvailable"] = float64(30 * recoveryGiB)
				}
				if scenario == "registration replaced" {
					n["metadata"].(object)["annotations"].(object)[temporaryActivationOwner] = "foreign"
				}
			}
			if scenario == "partition changed" {
				target.RawPartitionUUID = "99999999-9999-4999-8999-999999999999"
				record.Targets["temporary-v1-1"] = target
			}
			if scenario == "unfinished activation" {
				record.Completed = false
			}
			if scenario == "foreign V1 allocation" {
				r, _ := cloneRecoverySpec(f.s.replicas[0])
				r["metadata"].(object)["name"] = "foreign-v1"
				r["spec"].(object)["nodeID"] = target.Name
				f.s.replicas = append(f.s.replicas, r)
			}
			raw, _ := json.Marshal(record)
			f.record["data"].(object)["record"] = string(raw)
			got, err := a.recoveryStorage(t.Context(), cnpgRecoverySource{Namespace: "umami", Cluster: "umami-db", Size: "5Gi"})
			if err == nil || !reflect.DeepEqual(got, cnpgStorageEnvironment{}) || f.writes != 0 {
				t.Fatal("unqualified recovery storage accepted")
			}
		})
	}
}

// TestRecoveryStorageRebindsAfterNativeReads catches allocations and ownership changes after the initial API snapshot.
func TestRecoveryStorageRebindsAfterNativeReads(t *testing.T) {
	for _, scenario := range []string{"capacity changed", "registration changed", "node rebooted"} {
		t.Run(scenario, func(t *testing.T) {
			f, a := recoveryStorageAPI(t)
			reader, closes := a.reader, 0
			a.reader = func(ctx context.Context, address string) (nativeReplicaReader, func() error, error) {
				r, closeReader, err := reader(ctx, address)
				if err != nil {
					return nil, nil, err
				}
				return r, func() error {
					closes++
					if closes == 3 {
						for _, n := range f.s.storageNodes {
							if value(n, "status", "diskStatus", "longhorn-v2") == nil {
								continue
							}
							if scenario == "capacity changed" {
								value(n, "status", "diskStatus", "longhorn-v2").(object)["storageAvailable"] = float64(30 * recoveryGiB)
							}
							if scenario == "registration changed" {
								n["metadata"].(object)["annotations"].(object)[temporaryActivationOwner] = "foreign"
							}
						}
						if scenario == "node rebooted" {
							value(f.s.nodes[3], "status", "nodeInfo").(object)["bootID"] = "99999999-9999-4999-8999-999999999999"
						}
					}
					return closeReader()
				}, nil
			}
			got, err := a.recoveryStorage(t.Context(), cnpgRecoverySource{Namespace: "umami", Cluster: "umami-db", Size: "5Gi"})
			if err == nil || !reflect.DeepEqual(got, cnpgStorageEnvironment{}) || f.writes != 0 {
				t.Fatal("storage changed after native reads but was accepted")
			}
		})
	}
}
