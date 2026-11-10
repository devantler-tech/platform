package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

// The check requires both pools. This fixed source keeps these tests independent
// of ksail.prod.yaml, which retires the pools when #5582 ends.
const reviewedPoolsSource = `{"spec":{"cluster":{"controlPlanes":3,"workers":3,"autoscaler":{"node":{"maxNodesTotal":9,"pools":[{"name":"autoscale-longhorn-v1","serverType":"cpx42","location":"fsn1","min":2,"max":2,"labels":{"storage.platform.devantler.tech/migration-pool":"v1"}},{"name":"autoscale-longhorn-v2","serverType":"cpx42","location":"fsn1","min":1,"max":1,"labels":{"storage.platform.devantler.tech/migration-pool":"v2"}}]}}},"provider":{"hetzner":{"serverLimit":10}}}}`

// protectedMigrationFixture holds one armed V1 migration worker carrying a replica.
func protectedMigrationFixture() (*fakeAPI, engine, object, object) {
	a := fakeFixture()
	a.objects[fakeKey("settings.longhorn.io", "longhorn-system", "system-managed-components-node-selector")][0]["value"] = ""
	a.objects[fakeKey("settings.longhorn.io", "longhorn-system", "create-default-disk-labeled-nodes")][0]["value"] = "true"
	for _, name := range []string{"longhorn-manager", "longhorn-csi-plugin", "engine-image-test"} {
		d := a.objects[fakeKey("daemonset", "longhorn-system", name)][0]
		d["spec"].(object)["template"].(object)["spec"].(object)["nodeSelector"] = object{}
	}
	name := "autoscale-longhorn-v1-0123456789abcdef"
	n := resource("Node", "", name)
	n["metadata"].(object)["labels"] = object{longhornmigration.PoolLabel: "v1", "ksail.io/autoscaled": "true", "node.kubernetes.io/instance-type": "cpx42"}
	n["metadata"].(object)["annotations"] = object{longhornmigration.ScaleDownDisabled: "true", longhornmigration.OwnerAnnotation: longhornmigration.OwnerValue}
	n["spec"] = object{"providerID": "hcloud://42"}
	n["status"] = object{"nodeInfo": object{"systemUUID": "machine"}, "conditions": []any{object{"type": "Ready", "status": "True"}}}
	a.objects[fakeKey("nodes", "", "")] = append(a.objects[fakeKey("nodes", "", "")], n)
	csi := resource("CSINode", "", name)
	csi["spec"] = object{"drivers": []any{object{"name": "driver.longhorn.io"}}}
	a.objects[fakeKey("csinodes", "", "")] = append(a.objects[fakeKey("csinodes", "", "")], csi)
	lh := resource("Node", "longhorn-system", name)
	lh["metadata"].(object)["labels"] = object{longhornmigration.EnabledLabel: "true"}
	lh["spec"] = object{"allowScheduling": true, "evictionRequested": false, "disks": object{"migration-v1": object{"diskType": "filesystem", "path": "/var/lib/longhorn", "allowScheduling": true, "evictionRequested": false}}}
	a.objects[fakeKey("nodes.longhorn.io", "longhorn-system", "")] = append(a.objects[fakeKey("nodes.longhorn.io", "longhorn-system", "")], lh)
	r := resource("Replica", "longhorn-system", "migration-copy")
	r["spec"] = object{"nodeID": name}
	a.objects[fakeKey("replicas.longhorn.io", "longhorn-system", "")] = []object{r}
	e := testEngine(a)
	e.migrationSource = func() ([]byte, error) { return []byte(reviewedPoolsSource), nil }
	return a, e, n, lh
}

func TestClientReadinessRecognizesOnlyProtectedMigrationStorage(t *testing.T) {
	_, e, n, _ := protectedMigrationFixture()
	if ok, err := e.clientsReady(context.Background()); !ok || err != nil {
		t.Fatalf("verified migration storage broke client readiness: %v", err)
	}
	delete(n["metadata"].(object)["annotations"].(object), longhornmigration.ScaleDownDisabled)
	if ok, err := e.clientsReady(context.Background()); ok || err == nil {
		t.Fatal("readiness accepted migration replicas after protection disappeared")
	}
}

// rawMigrationFixture represents a completed registration, independently of
// the activation builder. A pool's original allocation role remains unchanged.
func rawMigrationFixture(t *testing.T) (*fakeAPI, engine, object, object, object) {
	t.Helper()
	a, e, n, lh := protectedMigrationFixture()
	lh["metadata"].(object)["annotations"] = object{"storage.platform.devantler.tech/v2-activation": "activation-uid"}
	lh["spec"].(object)["disks"] = object{"longhorn-v2": object{"diskType": "block", "path": "/dev/mapper/luks2-r-longhorn", "diskDriver": "aio", "allowScheduling": true, "evictionRequested": false}}
	targets := object{}
	for i, selection := range []string{"temporary-v1-1", "temporary-v1-2", "temporary-v2-1"} {
		role := "v1"
		if i == 2 {
			role = "v2"
		}
		binding := object{"selection": selection, "name": fmt.Sprintf("autoscale-longhorn-%s-%016x", role, i+1), "nodeUID": fmt.Sprintf("node-%d", i), "providerID": fmt.Sprintf("hcloud://%d", i+1), "machineUUID": fmt.Sprintf("machine-%d", i), "bootID": "11111111-1111-1111-1111-111111111111", "osImage": "Talos (fixture)", "longhornUID": fmt.Sprintf("longhorn-%d", i), "ephemeralUUID": fmt.Sprintf("ephemeral-%d", i), "rawPartitionUUID": fmt.Sprintf("22222222-2222-2222-2222-%012d", i+1)}
		binding["preparationUID"] = fmt.Sprintf("preparation-%d", i)
		if i == 0 {
			binding["name"], binding["nodeUID"], binding["providerID"] = str(n, "metadata", "name"), str(n, "metadata", "uid"), "hcloud://42"
			binding["machineUUID"], binding["longhornUID"] = "machine", str(lh, "metadata", "uid")
		}
		targets[selection] = binding
	}
	record, err := json.Marshal(object{"version": 1, "source": strings.Repeat("a", 40), "ownerRun": "1234", "completed": true, "targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	cm := resource("ConfigMap", "longhorn-system", "longhorn-v2-temporary-activation")
	cm["metadata"].(object)["uid"] = "activation-uid"
	cm["data"] = object{"record": string(record)}
	a.objects[fakeKey("configmap", "longhorn-system", "longhorn-v2-temporary-activation")] = []object{cm}
	return a, e, n, lh, cm
}

// A completed raw registration must satisfy both publication and replica
// exclusion while the pool retains its original allocation label.
func TestCompletedRawTransitionPreservesPublicationAndReplicaExclusion(t *testing.T) {
	_, e, _, _, _ := rawMigrationFixture(t)
	if ok, err := e.clientsReady(context.Background()); !ok || err != nil {
		t.Fatalf("completed raw registration blocks production publication: %v", err)
	}
	if err := e.replicaExclusion(context.Background()); err != nil {
		t.Fatalf("completed raw registration loses its storage exception: %v", err)
	}
}

func TestStagedRawTransitionReadsItsRegistration(t *testing.T) {
	a, e, n, lh, cm := rawMigrationFixture(t)
	record, err := longhornmigration.DecodeActivationRecord([]byte(str(cm, "data", "record")))
	if err != nil {
		t.Fatal(err)
	}
	stage := record.Targets["temporary-v2-1"]
	meta := n["metadata"].(object)
	meta["name"], meta["uid"] = stage.Name, stage.NodeUID
	meta["labels"].(object)[longhornmigration.PoolLabel] = "v2"
	n["spec"].(object)["providerID"] = stage.ProviderID
	n["status"].(object)["nodeInfo"].(object)["systemUUID"] = stage.MachineUUID
	lhMeta := lh["metadata"].(object)
	lhMeta["name"], lhMeta["uid"] = stage.Name, stage.LonghornUID
	val(lh, "spec", "disks", "longhorn-v2").(object)["path"] = "/dev/mapper/luks2-" + longhornmigration.RetainedStageRawID
	if ok, err := e.migrationStorageEligible(t.Context(), n, lh); !ok || err != nil {
		t.Fatalf("completed staged registration did not reach the publication reader: %v", err)
	}
	delete(a.objects, fakeKey("configmap", "longhorn-system", longhornmigration.ActivationJournal))
	if ok, err := e.migrationStorageEligible(t.Context(), n, lh); ok || err == nil {
		t.Fatal("staged raw storage bypassed its registration")
	}
}

// An incomplete, replaced or foreign activation must fail both independent
// readers rather than turn a matching raw path into storage authorization.
func TestRawTransitionCannotBorrowAnotherActivationOrWorkerIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*fakeAPI, object, object, object){
		"missing journal": func(a *fakeAPI, _, _, _ object) {
			delete(a.objects, fakeKey("configmap", "longhorn-system", "longhorn-v2-temporary-activation"))
		},
		"replaced journal": func(_ *fakeAPI, _, _ object, cm object) { cm["metadata"].(object)["uid"] = "replacement" },
		"incomplete registration": func(_ *fakeAPI, _, _ object, cm object) {
			cm["data"].(object)["record"] = strings.Replace(str(cm, "data", "record"), `"completed":true`, `"completed":false`, 1)
		},
		"replaced Kubernetes node": func(_ *fakeAPI, n, _, _ object) { n["metadata"].(object)["uid"] = "replacement" },
		"replaced provider":        func(_ *fakeAPI, n, _, _ object) { n["spec"].(object)["providerID"] = "hcloud://99" },
		"replaced hardware": func(_ *fakeAPI, n, _, _ object) {
			n["status"].(object)["nodeInfo"].(object)["systemUUID"] = "replacement"
		},
		"replaced Longhorn node": func(_ *fakeAPI, _, lh, _ object) { lh["metadata"].(object)["uid"] = "replacement" },
		"foreign activation owner": func(_ *fakeAPI, _, lh, _ object) {
			lh["metadata"].(object)["annotations"].(object)["storage.platform.devantler.tech/v2-activation"] = "foreign"
		},
		"deleting journal": func(_ *fakeAPI, _, _ object, cm object) {
			cm["metadata"].(object)["deletionTimestamp"] = "2026-11-03T00:00:00Z"
		},
		"ambiguous completion": func(_ *fakeAPI, _, _ object, cm object) {
			cm["data"].(object)["record"] = strings.Replace(str(cm, "data", "record"), `"completed":true`, `"completed":false,"completed":true`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, e, n, lh, cm := rawMigrationFixture(t)
			mutate(a, n, lh, cm)
			if ok, err := e.clientsReady(context.Background()); ok || err == nil {
				t.Fatal("unbound raw registration accepted for publication")
			}
			if err := e.replicaExclusion(context.Background()); err == nil {
				t.Fatal("unbound raw registration accepted for replica storage")
			}
		})
	}
}

func TestReplicaExclusionAdmitsOnlyProtectedMigrationReplicas(t *testing.T) {
	_, e, n, _ := protectedMigrationFixture()
	if err := e.replicaExclusion(context.Background()); err != nil {
		t.Fatalf("verified migration storage failed replica exclusion: %v", err)
	}
	delete(n["metadata"].(object)["annotations"].(object), longhornmigration.ScaleDownDisabled)
	if err := e.replicaExclusion(context.Background()); err == nil {
		t.Fatal("replica exclusion accepted a migration node after protection disappeared")
	}
}

// A fenced node with no enabled label is ordinary compute, whatever its name,
// so a replica on it still fails the check.
func TestReplicaExclusionRefusesAReplicaOnAFencedMigrationNode(t *testing.T) {
	_, e, _, lh := protectedMigrationFixture()
	delete(lh["metadata"].(object)["labels"].(object), longhornmigration.EnabledLabel)
	lh["spec"] = object{"allowScheduling": false, "evictionRequested": true, "disks": object{}}
	if err := e.replicaExclusion(context.Background()); err == nil {
		t.Fatal("replica exclusion accepted a replica on an unarmed migration node")
	}
}

// A malformed authorization holds even on a fenced node with nothing on it.
func TestReplicaExclusionHoldsOnAMalformedAuthorization(t *testing.T) {
	a, e, _, lh := protectedMigrationFixture()
	lh["metadata"].(object)["labels"].(object)[longhornmigration.EnabledLabel] = "yes"
	lh["spec"] = object{"allowScheduling": false, "evictionRequested": true, "disks": object{}}
	a.objects[fakeKey("replicas.longhorn.io", "longhorn-system", "")] = []object{}
	if err := e.replicaExclusion(context.Background()); err == nil {
		t.Fatal("replica exclusion accepted a malformed migration authorization")
	}
}
