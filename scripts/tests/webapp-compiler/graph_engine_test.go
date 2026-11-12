package compiler_test

import (
	"slices"
	"testing"

	kroapi "github.com/kubernetes-sigs/kro/api/v1alpha1"
	gecompiler "github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	geruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// resolveChildren renders an instance through the pinned controller's own
// instance path: the RGD adapter and program cache it calls on every reconcile,
// the graph-engine compiler, and the runtime nodes in the executor's order.
// Each child's observation is seeded from its own desired object, as the
// executor records it after an apply, so this proves the expressions, not
// cluster reconciliation. It returns the rendered children by node ID and the
// IDs of the children the instance's inclusion conditions leave out.
func resolveChildren(t *testing.T, engine *gecompiler.Compiler, definition *kroapi.ResourceGraphDefinition, instance *unstructured.Unstructured) (map[string]*unstructured.Unstructured, []string) {
	t.Helper()
	resolved, _, err := rgdadapter.BuildRuntimeForInstanceCached(definition, instance, engine, registry.New(),
		geruntime.WithMaxCollectionSize(5))
	if err != nil {
		t.Fatalf("build the controller runtime: %v", err)
	}
	objects := map[string]*unstructured.Unstructured{}
	var ignored []string
	sawSchema := false
	for _, node := range resolved.Nodes() {
		ignore, err := node.IsIgnored()
		if err != nil {
			t.Fatalf("evaluate inclusion %s: %v", node.ID(), err)
		}
		if ignore {
			ignored = append(ignored, node.ID())
			continue
		}
		desired, err := node.Resolve()
		if err != nil || len(desired) != 1 {
			t.Fatalf("resolve %s: count=%d error=%v", node.ID(), len(desired), err)
		}
		switch node.Kind() {
		case gecompiler.NodeKindDef:
			if node.ID() != rgdadapter.SchemaNodeID || sawSchema {
				t.Fatalf("unexpected definition node %s", node.ID())
			}
			sawSchema = true
		case gecompiler.NodeKindTemplate:
			objects[node.ID()] = desired[0]
		default:
			t.Fatalf("unexpected %s node %s in a graph of plain children", node.Kind(), node.ID())
		}
		node.SetObserved(desired, desired)
		resolved.Set(node.ID(), desired[0].Object)
	}
	if !sawSchema {
		t.Fatal("the controller runtime evaluated no instance schema")
	}
	slices.Sort(ignored)
	return objects, ignored
}
