package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
)

func unstartedBenchmarkFixture(t *testing.T) (benchmarkRecord, map[string]object) {
	t.Helper()
	record := recoveryFixture(t)
	record.Version, record.Namespace, record.NamespaceUID = 3, benchmarkNamespace, "uid-workspace"
	record.Resources = map[string]benchmarkResource{
		"Namespace": {Name: record.Namespace, UID: record.NamespaceUID},
	}
	record.TagNodes = map[string]string{"prod-worker-1": "node-1", "prod-worker-2": "node-2", "prod-worker-3": "node-3"}
	assets, err := benchmarkResourcesForClient(record.Phase, record.Run, record.Namespace, record.clientNode())
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]object{}
	for _, asset := range assets {
		kind := text(asset, "kind")
		meta := asset["metadata"].(object)
		meta["uid"], meta["resourceVersion"] = "uid-"+text(asset, "metadata", "name"), "1"
		if kind == "Pod" || kind == "PersistentVolumeClaim" {
			asset["status"] = object{"phase": "Pending"}
		}
		record.Resources[kind] = benchmarkIdentity(asset)
		result[kind] = asset
	}
	return record, result
}

func TestRecoverUnstartedBenchmarkOnlyAcceptsUnscheduledOwnedAssets(t *testing.T) {
	record, assets := unstartedBenchmarkFixture(t)
	if err := verifyUnstartedBenchmark(record); err != nil {
		t.Fatal(err)
	}
	for kind, asset := range assets {
		if err := verifyUnstartedBenchmarkAsset(kind, asset, record); err != nil {
			t.Fatalf("original %s rejected: %v", kind, err)
		}
	}
	for name, tc := range map[string]struct {
		kind   string
		change func(object)
	}{
		"assigned Pod": {"Pod", func(asset object) { asset["spec"].(object)["nodeName"] = legacyBenchmarkClientNode }},
		"scheduled Pod": {"Pod", func(asset object) {
			asset["status"].(object)["conditions"] = []any{object{"type": "PodScheduled", "status": "True"}}
		}},
		"replacement Pod": {"Pod", func(asset object) { asset["metadata"].(object)["uid"] = "other" }},
		"assigned volume": {"PersistentVolumeClaim", func(asset object) { asset["spec"].(object)["volumeName"] = "other" }},
		"selected node": {"PersistentVolumeClaim", func(asset object) {
			asset["metadata"].(object)["annotations"] = object{"volume.kubernetes.io/selected-node": legacyBenchmarkClientNode}
		}},
		"eager class": {"StorageClass", func(asset object) { asset["volumeBindingMode"] = "Immediate" }},
	} {
		t.Run(name, func(t *testing.T) {
			record, assets := unstartedBenchmarkFixture(t)
			tc.change(assets[tc.kind])
			if err := verifyUnstartedBenchmarkAsset(tc.kind, assets[tc.kind], record); err == nil {
				t.Fatal("unqualified asset accepted")
			}
		})
	}
	record.Resources["PersistentVolume"] = benchmarkResource{Name: "pv", UID: "original-pv"}
	if verifyUnstartedBenchmark(record) == nil {
		t.Fatal("a benchmark with an original PV was accepted")
	}
}

func TestUnstartedRecoveryRefusesStorageReferences(t *testing.T) {
	record, assets := unstartedBenchmarkFixture(t)
	claim, class := record.Resources["PersistentVolumeClaim"], record.Resources["StorageClass"]
	handle := "pvc-" + claim.UID
	for name, tc := range map[string]struct {
		kind string
		item object
	}{
		"workspace pod": {"pods", object{"metadata": object{"name": "foreign"}}},
		"foreign claim": {"persistentvolumeclaims", object{
			"metadata": object{"name": "foreign"}, "spec": object{"storageClassName": class.Name}}},
		"existing PV":      {"persistentvolumes", object{"spec": object{"claimRef": object{"uid": claim.UID}}}},
		"existing backend": {"volumes.longhorn.io", object{"metadata": object{"name": handle}}},
		"native attachment": {"volumeattachments.storage.k8s.io", object{
			"spec": object{"source": object{"persistentVolumeName": handle}}}},
		"Longhorn attachment": {"volumeattachments.longhorn.io", object{"spec": object{"volume": handle}}},
		"unstarted":           {},
	} {
		t.Run(name, func(t *testing.T) {
			c := client{run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if input != nil || program != "kubectl" || args[2] != "get" {
					t.Fatal("storage qualification attempted a write")
				}
				if args[3] == "storageclass" && args[4] == class.Name {
					return json.Marshal(assets["StorageClass"])
				}
				items := []object{}
				if args[3] == "persistentvolumeclaims" {
					items = append(items, assets["PersistentVolumeClaim"])
				}
				if args[3] == tc.kind {
					items = append(items, tc.item)
				}
				return json.Marshal(object{"items": items})
			}}
			if err := verifyUnstartedBenchmarkStorage(t.Context(), c, record); (err != nil) != (tc.item != nil) {
				t.Fatalf("unexpected storage qualification: %v", err)
			}
		})
	}
}

func TestBenchmarkClientMemoryCountsExistingRequestsBeforeScratch(t *testing.T) {
	s := benchmarkInventoryFixture()
	_, assets := unstartedBenchmarkFixture(t)
	probe := assets["Pod"]
	probe["spec"].(object)["nodeSelector"] = object{"kubernetes.io/hostname": benchmarkClientNode}
	s.nodes[0]["status"].(object)["allocatable"].(object)["memory"] = "12Gi"
	workload := object{"spec": object{"nodeName": benchmarkClientNode, "containers": []any{
		object{"resources": object{"requests": object{"memory": "11Gi"}}},
	}}, "status": object{"phase": "Running"}}
	s.pods = []object{workload}
	if err := verifyBenchmarkClientMemory(s, probe); err != nil {
		t.Fatal(err)
	}
	s.nodes[0]["status"].(object)["allocatable"].(object)["memory"] = "11500Mi"
	if err := verifyBenchmarkClientMemory(s, probe); err == nil {
		t.Fatal("benchmark was admitted with less than 512 MiB available")
	}
	if (benchmarkRecord{}).clientNode() != legacyBenchmarkClientNode ||
		(benchmarkRecord{ClientNode: benchmarkClientNode}).clientNode() != benchmarkClientNode {
		t.Fatal("old or new benchmark client identity changed")
	}
}

func TestUnstartedBenchmarkRecoveryOrdersAndBindsEveryDeletion(t *testing.T) {
	for _, schedulerRace := range []bool{false, true} {
		t.Run(fmt.Sprintf("scheduler_race=%t", schedulerRace), func(t *testing.T) {
			record, assets := unstartedBenchmarkFixture(t)
			runtime, _ := retirementRuntimeInstanceFixture(t)
			record.V1Runtime = runtime.V1Runtime
			journal := resource("longhorn-v2-benchmark-v1")
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			journal["data"] = object{"record": string(raw)}
			namespace := resource(record.Namespace)
			namespace["apiVersion"], namespace["kind"], namespace["status"] = "v1", "Namespace", object{"phase": "Active"}
			meta := namespace["metadata"].(object)
			meta["uid"], meta["annotations"] = record.NamespaceUID, object{"kustomize.toolkit.fluxcd.io/prune": "disabled"}
			meta["labels"] = object{"pod-security.kubernetes.io/enforce": "restricted",
				"pod-security.kubernetes.io/enforce-version": "latest", "velero.io/exclude-from-backup": "true"}
			objects := map[string]object{"namespace/" + record.Namespace: namespace, "configmap/longhorn-v2-benchmark-v1": journal}
			for kind, asset := range assets {
				objects[strings.ToLower(kind)+"/"+text(asset, "metadata", "name")] = asset
			}
			for name, uid := range record.TagNodes {
				node := resource(name)
				node["metadata"].(object)["uid"] = uid
				objects["nodes.longhorn.io/"+name] = node
			}
			const archive = "configmap/longhorn-v2-benchmark-v1-unstarted-1234"
			allowedDeletes := map[string]string{
				"/api/v1/namespaces/longhorn-benchmark/pods/fio":                                            "pod/fio",
				"/api/v1/namespaces/longhorn-benchmark/persistentvolumeclaims/fio-data":                     "persistentvolumeclaim/fio-data",
				"/apis/networking.k8s.io/v1/namespaces/longhorn-benchmark/networkpolicies/fio-default-deny": "networkpolicy/fio-default-deny",
				"/apis/storage.k8s.io/v1/storageclasses/lh-benchmark-v1-1234":                               "storageclass/lh-benchmark-v1-1234",
				"/api/v1/namespaces/longhorn-system/configmaps/longhorn-v2-benchmark-v1":                    "configmap/longhorn-v2-benchmark-v1",
			}
			source := strings.Repeat("2", 40)
			terminal := false
			var deletes []string
			c := client{run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				switch program {
				case "git":
					return []byte(source + "\trefs/heads/main\n"), nil
				case "gh":
					terminal = true
					return []byte(`{"id":1234,"status":"completed","head_sha":"` + record.Source + `","head_branch":"main","event":"workflow_dispatch","run_attempt":1,"path":".github/workflows/prepare-longhorn-v2-node.yaml"}`), nil
				case "kubectl":
					switch args[2] {
					case "get":
						if !strings.HasPrefix(args[4], "--") {
							item := objects[args[3]+"/"+args[4]]
							if item == nil {
								return nil, nil
							}
							return json.Marshal(item)
						}
						items := []object{}
						for key, item := range objects {
							if strings.HasPrefix(key, strings.TrimSuffix(args[3], "s")+"/") {
								items = append(items, item)
							}
						}
						return json.Marshal(object{"items": items})
					case "patch":
						var patch []object
						if !terminal || args[3] != "configmap" || args[4] != "longhorn-v2-benchmark-v1" ||
							json.Unmarshal(input, &patch) != nil || len(patch) != 3 ||
							text(patch[0], "path") != "/metadata/uid" || text(patch[0], "value") != text(journal, "metadata", "uid") ||
							text(patch[1], "path") != "/metadata/resourceVersion" || text(patch[1], "value") != text(journal, "metadata", "resourceVersion") {
							t.Fatal("journal changed without a terminal owner and fresh UID/version")
						}
						journal["data"] = object{"record": text(patch[2], "value")}
						version := journal["metadata"].(object)
						version["resourceVersion"] = text(journal, "metadata", "resourceVersion") + "1"
						return json.Marshal(journal)
					case "delete":
						target := strings.TrimPrefix(args[3], "--raw=")
						key := allowedDeletes[target]
						asset := objects[key]
						var opts object
						if !terminal || key == "" || asset == nil || json.Unmarshal(input, &opts) != nil ||
							text(opts, "preconditions", "uid") != text(asset, "metadata", "uid") ||
							text(opts, "preconditions", "resourceVersion") != text(asset, "metadata", "resourceVersion") {
							t.Fatal("delete is outside the original terminal ownership and UID/version")
						}
						if schedulerRace && key == "pod/fio" {
							return nil, errors.New("Pod resource version advanced while scheduling")
						}
						if key == "configmap/longhorn-v2-benchmark-v1" && objects[archive] == nil {
							t.Fatal("journal deleted before the original evidence was sealed")
						}
						deletes = append(deletes, key)
						delete(objects, key)
						return nil, nil
					case "create":
						var item object
						if json.Unmarshal(input, &item) != nil || value(item, "immutable") != true ||
							text(item, "kind") != "ConfigMap" || text(item, "metadata", "name") != path.Base(archive) {
							t.Fatal("create exceeded the immutable evidence archive")
						}
						meta := item["metadata"].(object)
						meta["uid"], meta["resourceVersion"] = "archive-uid", "9"
						objects[archive] = item
						return json.Marshal(item)
					}
				}
				t.Fatalf("unexpected recovery command: %s %v", program, args)
				return nil, nil
			}}
			err = recoverUnstartedV1Benchmark(t.Context(), c, source, "5678")
			if schedulerRace {
				if err == nil || len(deletes) != 0 || objects[archive] != nil {
					t.Fatalf("raced scheduling changed scratch state: %v %v", err, deletes)
				}
				return
			}
			want := []string{"pod/fio", "persistentvolumeclaim/fio-data", "networkpolicy/fio-default-deny",
				"storageclass/lh-benchmark-v1-1234", "configmap/longhorn-v2-benchmark-v1"}
			if err != nil || !slices.Equal(deletes, want) || objects[archive] == nil {
				t.Fatalf("cleanup did not retain its archive and delete exactly in order: %v %v", err, deletes)
			}
		})
	}
}
