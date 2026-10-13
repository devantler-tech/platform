package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const deploymentManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: backstage
  namespace: backstage
spec:
  template:
    spec:
      containers:
        - name: backstage
          image: example.invalid/backstage:1
      volumes:
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: backstage
  namespace: backstage
`

func fixture() (map[string]any, map[string]any) {
	labels := map[string]any{"app.kubernetes.io/name": "backstage", "app.kubernetes.io/instance": "backstage", "app.kubernetes.io/component": "backstage"}
	d := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata("backstage", "deployment-uid"), "spec": map[string]any{"selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "backstage"}}, "volumes": []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{}}}}}}}
	p := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata("backstage-pod", "pod-uid"), "spec": map[string]any{"containers": []any{map[string]any{"name": "backstage"}}, "volumes": []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{}}}}}
	object(p["metadata"])["labels"] = labels
	s := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": metadata("backstage", "service-uid")}
	l := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{d, p, map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": metadata("backstage-db", "database-uid")}, map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": metadata("backstage-db-3", "claim-uid")}}}
	r := map[string]any{"name": "backstage", "namespace": "backstage", "version": 4, "manifest": deploymentManifest, "config": map[string]any{"marker": "DO_NOT_PRINT"}, "info": map[string]any{"status": "deployed", "resources": map[string]any{"v1/Deployment": []any{d}, "v1/Service": []any{s}, "v1/Pod(related)": []any{map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{"resourceVersion": "100"}, "items": []any{p}}}}}}
	r["chart"] = map[string]any{"metadata": map[string]any{"name": "backstage", "version": "2.10.2", "apiVersion": "v2"}}
	return r, l
}

func metadata(name, uid string) map[string]any {
	return map[string]any{"name": name, "namespace": "backstage", "uid": uid, "resourceVersion": "12"}
}
func item(l map[string]any, index int) map[string]any { return object(l["items"].([]any)[index]) }
func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func protectedRunner(t *testing.T) {
	t.Helper()
	// The protected workflow invokes the entrypoint from the reviewed repo root.
	t.Chdir("../..")
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", "devantler-tech/platform")
	t.Setenv("GITHUB_REF", "refs/heads/main")
}
func addObservedResource(r map[string]any, kind, name string) {
	r["manifest"] = r["manifest"].(string) + "---\napiVersion: v1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\n  namespace: backstage\n"
	object(object(r["info"])["resources"])["v1/"+kind] = []any{map[string]any{"apiVersion": "v1", "kind": kind, "metadata": metadata(name, strings.ToLower(kind)+"-uid")}}
}
