package compiler_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// Check metadata without API writes so active and opt-in fixtures share the server.
func assertTenantControllerMetadata(t *testing.T, objects map[string]*unstructured.Unstructured) {
	t.Helper()
	if err := tenantControllerMetadataError(objects); err != nil {
		t.Fatal(err)
	}
}

// Compare actual compiled objects with the deployed KRO label decorator.
func tenantControllerMetadataError(objects map[string]*unstructured.Unstructured) error {
	if len(objects) == 0 {
		return fmt.Errorf("no compiled tenant resources were checked")
	}
	for id, object := range objects {
		if object == nil {
			return fmt.Errorf("missing compiled resource %s", id)
		}
		decorated := object.DeepCopy()
		metadata.NewNodeLabeler().ApplyLabels(decorated)
		if object.GetLabels()[metadata.ManagedByLabelKey] != decorated.GetLabels()[metadata.ManagedByLabelKey] {
			return fmt.Errorf("%s declares ownership different from the pinned controller", id)
		}
	}
	return nil
}

// Exercise the pinned SSA implementation against the isolated real API server.
// The competing writer uses the actual Kyverno generate body, not its controller.
func assertTenantNetworkFloor(t *testing.T, ctx context.Context, config *rest.Config, root string, objects map[string]*unstructured.Unstructured) {
	t.Helper()
	t.Run("network-floor-settles-with-competing-generator", func(t *testing.T) {
		clients, err := kroclient.NewSet(kroclient.Config{RestConfig: rest.CopyConfig(config)})
		if err != nil {
			t.Fatal(err)
		}
		const namespace = "tenant-fixture"
		if _, err := clients.Kubernetes().CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		var policy unstructured.Unstructured
		readYAML(t, filepath.Join(root, "k8s/bases/infrastructure/cluster-policies/best-practices/add-default-deny.yaml"), &policy)
		rules, found, err := unstructured.NestedSlice(policy.Object, "spec", "rules")
		if err != nil || !found {
			t.Fatalf("missing actual generate rules: %v", err)
		}
		var generated *unstructured.Unstructured
		for _, raw := range rules {
			rule, ok := raw.(map[string]interface{})
			if !ok || rule["name"] != "generate-default-deny-networkpolicy" {
				continue
			}
			generation, ok := rule["generate"].(map[string]interface{})
			if !ok || generation["namespace"] != "{{request.object.metadata.name}}" || generation["synchronize"] != true {
				t.Fatal("expected the synchronized namespace baseline")
			}
			data, ok := generation["data"].(map[string]interface{})
			if !ok {
				t.Fatal("missing generated baseline body")
			}
			generated = (&unstructured.Unstructured{Object: data}).DeepCopy()
			generated.SetAPIVersion(generation["apiVersion"].(string))
			generated.SetKind(generation["kind"].(string))
			generated.SetName(generation["name"].(string))
			generated.SetNamespace(namespace)
			generated.SetLabels(map[string]string{metadata.ManagedByLabelKey: "kyverno"})
		}
		if generated == nil || generated.GetKind() != "NetworkPolicy" {
			t.Fatal("missing actual generated NetworkPolicy")
		}
		desired := objects["defaultDeny"].DeepCopy()
		metadata.NewNodeLabeler().ApplyLabels(desired)
		parent := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kro.run/v1alpha1", "kind": "Tenant",
			"metadata": map[string]interface{}{"name": namespace, "namespace": "default"},
		}}
		applier := applyset.New(applyset.Config{Client: clients.Dynamic(), RESTMapper: clients.RESTMapper(), Log: logr.Discard(), ParentNamespace: "default"}, parent)
		resource := clients.Dynamic().Resource(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}).Namespace(namespace)
		// Seed the deployed old name, then follow KRO's apply-before-prune order.
		// Its replacement floor must exist before the shared old object is removed.
		old := desired.DeepCopy()
		old.SetName(generated.GetName())
		if result, _, err := applier.Apply(ctx, []applyset.Resource{{ID: "defaultDeny", Object: old}}, applyset.ApplyMode{}); err != nil || result.Errors() != nil {
			t.Fatalf("seed old controller baseline: result=%v error=%v", result, err)
		}
		result, scope, err := applier.Apply(ctx, []applyset.Resource{{ID: "defaultDeny", Object: desired.DeepCopy()}}, applyset.ApplyMode{})
		if err != nil || result.Errors() != nil {
			t.Fatalf("initial native controller apply: result=%v error=%v", result, err)
		}
		orphans, err := applier.ListOrphans(ctx, applyset.PruneOptions{KeepUIDs: result.ObservedUIDs(), Scope: scope.PruneScope()})
		if err != nil || len(orphans) != 1 || orphans[0].Object.GetName() != generated.GetName() {
			t.Fatalf("expected only the old shared baseline to retire: count=%d error=%v", len(orphans), err)
		}
		pruned, err := applier.DeleteOrphan(ctx, orphans[0])
		if err != nil || pruned.Pruned == nil || pruned.Conflict {
			t.Fatalf("native baseline retirement failed: result=%v error=%v", pruned, err)
		}
		if _, err := resource.Get(ctx, desired.GetName(), metav1.GetOptions{}); err != nil {
			t.Fatal("replacement floor disappeared during retirement:", err)
		}
		for round := 0; round < 3; round++ {
			if _, err := resource.Apply(ctx, generated.GetName(), generated.DeepCopy(), metav1.ApplyOptions{FieldManager: "kyverno-generate", Force: true}); err != nil {
				t.Fatal(err)
			}
			current, err := resource.Get(ctx, desired.GetName(), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := applier.Apply(ctx, []applyset.Resource{{ID: "defaultDeny", Object: desired.DeepCopy(), Current: current}}, applyset.ApplyMode{})
			if err != nil || result.Errors() != nil {
				t.Fatalf("native controller apply %d: result=%v error=%v", round, result, err)
			}
			if result.HasClusterMutation() {
				t.Fatalf("round %d: reconcilers still mutate the same baseline", round)
			}
		}
		for _, name := range []string{generated.GetName(), desired.GetName()} {
			object, err := resource.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			selector, found, err := unstructured.NestedMap(object.Object, "spec", "podSelector")
			if err != nil || !found || len(selector) != 0 {
				t.Fatal("network floor no longer selects every pod")
			}
			for _, field := range []string{"ingress", "egress"} {
				entries, _, err := unstructured.NestedSlice(object.Object, "spec", field)
				if err != nil || len(entries) != 0 {
					t.Fatal("network floor contains an allow rule")
				}
			}
			policyTypes, _, err := unstructured.NestedStringSlice(object.Object, "spec", "policyTypes")
			if err != nil || len(policyTypes) != 2 || policyTypes[0] != "Ingress" || policyTypes[1] != "Egress" {
				t.Fatal("network floor no longer isolates both directions")
			}
		}
	})
}
