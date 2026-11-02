package main

import (
	"context"
	"testing"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

// ksail.prod.yaml holds no V2 pool until a later slice of #5655, and the check
// requires both pools, so this source is the configuration that slice adds.
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
