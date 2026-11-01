package main

import (
	"context"
	"os"
	"testing"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

func TestClientReadinessRecognizesOnlyProtectedMigrationStorage(t *testing.T) {
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
	e.migrationSource = func() ([]byte, error) { return os.ReadFile("../../ksail.prod.yaml") }
	if ok, err := e.clientsReady(context.Background()); !ok || err != nil {
		t.Fatalf("verified migration storage broke client readiness: %v", err)
	}
	delete(n["metadata"].(object)["annotations"].(object), longhornmigration.ScaleDownDisabled)
	if ok, err := e.clientsReady(context.Background()); ok || err == nil {
		t.Fatal("readiness accepted migration replicas after protection disappeared")
	}
}
