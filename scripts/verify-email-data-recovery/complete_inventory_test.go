package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/namespaceinventory"
	"gopkg.in/yaml.v3"
)

const fixtureBundleUID = "f75759ba-2d74-4227-8033-a255cd544258"

// platformNamespaceFixture holds what production adds to every namespace
// besides the four generated network policies, in the shapes read on
// 2026-11-25: Kyverno's LimitRange and ResourceQuota and trust-manager's CA
// bundle ConfigMap.
func platformNamespaceFixture(namespace, namespaceUID string) []object {
	generated := func(policy, rule string) object {
		return object{"app.kubernetes.io/managed-by": "kyverno", "generate.kyverno.io/policy-name": policy, "generate.kyverno.io/policy-namespace": "", "generate.kyverno.io/rule-name": rule, "generate.kyverno.io/trigger-group": "", "generate.kyverno.io/trigger-kind": "Namespace", "generate.kyverno.io/trigger-namespace": "", "generate.kyverno.io/trigger-uid": namespaceUID, "generate.kyverno.io/trigger-version": "v1"}
	}
	return []object{
		{"apiVersion": "v1", "kind": "LimitRange", "metadata": object{"name": "default-limitrange", "namespace": namespace, "uid": "generated-limitrange", "labels": generated("add-default-limitrange", "generate-limitrange"), "annotations": object{"generate.kyverno.io/provenance": "{}"}}, "spec": object{"limits": []any{object{"default": object{"cpu": "500m", "memory": "512Mi"}, "defaultRequest": object{"cpu": "15m", "memory": "128Mi"}, "type": "Container"}}}},
		{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": object{"name": "default-resourcequota", "namespace": namespace, "uid": "generated-resourcequota", "labels": generated("add-ns-quota", "generate-resourcequota"), "annotations": object{"generate.kyverno.io/provenance": "{}"}}, "spec": object{"hard": object{"requests.cpu": "8", "requests.memory": "16Gi"}}, "status": object{}},
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": "cloudflare-origin-ca", "namespace": namespace, "uid": "trust-bundle-configmap", "labels": object{"trust.cert-manager.io/bundle": "cloudflare-origin-ca"}, "annotations": object{"trust.cert-manager.io/hash": "fixture"}, "ownerReferences": []any{object{"apiVersion": "trust.cert-manager.io/v1alpha1", "kind": "Bundle", "name": "cloudflare-origin-ca", "uid": fixtureBundleUID, "controller": true, "blockOwnerDeletion": true}}}, "data": object{"ca-certificates.crt": "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"}},
	}
}

// platformCleanupFixture adds every object the platform puts in a namespace.
func platformCleanupFixture(t *testing.T) (*recoveryDriver, *cleanupAPI, context.Context) {
	t.Helper()
	d, api, ctx := cleanupFixture(t, false)
	namespace, _ := d.ref("Namespace", d.i.namespace, "")
	objects := append(generatedNamespacePolicyFixture(d.i.namespace, namespace.uid), platformNamespaceFixture(d.i.namespace, namespace.uid)...)
	for _, o := range objects {
		api.objects[cleanupKey(text(o, "kind"), d.i.namespace, text(child(o, "metadata"), "name"))] = o
	}
	api.objects[cleanupKey("bundles.trust.cert-manager.io", "", trustBundle)] = object{"apiVersion": "trust.cert-manager.io/v1alpha1", "kind": "Bundle", "metadata": object{"name": trustBundle, "uid": fixtureBundleUID}}
	return d, api, ctx
}

func TestCleanupRecognizesWhatThePlatformAddsToEveryNamespace(t *testing.T) {
	d, api, ctx := platformCleanupFixture(t)
	if err := d.cleanupRestore(ctx); err != nil {
		t.Fatalf("platform-generated objects prevented owned namespace disposal: %s", err)
	}
	if _, exists := api.objects[cleanupKey("Namespace", "", d.i.namespace)]; exists {
		t.Fatal("the verified disposable namespace was retained")
	}
}

func TestCleanupRetainsChangedPlatformObjects(t *testing.T) {
	limit := cleanupKey("LimitRange", "", "default-limitrange")
	quota := cleanupKey("ResourceQuota", "", "default-resourcequota")
	bundle := cleanupKey("ConfigMap", "", trustBundle)
	mutations := map[string]struct {
		target string
		mutate func(object)
	}{
		"limit-of-another-namespace": {limit, func(o object) {
			child(child(o, "metadata"), "labels")["generate.kyverno.io/trigger-uid"] = "another-namespace-uid"
		}},
		"limit-of-another-policy": {limit, func(o object) {
			child(child(o, "metadata"), "labels")["generate.kyverno.io/policy-name"] = "add-ns-quota"
		}},
		"limit-with-an-owner": {limit, func(o object) {
			child(o, "metadata")["ownerReferences"] = []any{object{"apiVersion": "v1", "kind": "Pod", "name": "foreign", "uid": "foreign"}}
		}},
		"limit-not-from-kyverno": {limit, func(o object) {
			child(child(o, "metadata"), "labels")["app.kubernetes.io/managed-by"] = "another-manager"
		}},
		"quota-of-another-rule": {quota, func(o object) {
			child(child(o, "metadata"), "labels")["generate.kyverno.io/rule-name"] = "generate-limitrange"
		}},
		"quota-with-unknown-content": {quota, func(o object) { o["data"] = object{"unexpected": "value"} }},
		"bundle-with-another-key":    {bundle, func(o object) { child(o, "data")["extra"] = "value" }},
		"bundle-with-binary-data":    {bundle, func(o object) { o["binaryData"] = object{"blob": "AA=="} }},
		"bundle-of-another-owner": {bundle, func(o object) {
			items(child(o, "metadata"), "ownerReferences")[0]["uid"] = "another-bundle-uid"
		}},
		"bundle-without-controller": {bundle, func(o object) {
			items(child(o, "metadata"), "ownerReferences")[0]["controller"] = false
		}},
		"bundle-with-another-label": {bundle, func(o object) {
			child(child(o, "metadata"), "labels")["app"] = "foreign"
		}},
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			d, api, ctx := platformCleanupFixture(t)
			parts := strings.SplitN(mutation.target, "/", 3)
			key := cleanupKey(parts[0], d.i.namespace, parts[2])
			mutation.mutate(api.objects[key])
			if d.cleanupRestore(ctx) == nil {
				t.Fatal("a changed platform object allowed namespace disposal")
			}
			for _, retained := range []string{key, cleanupKey("Namespace", "", d.i.namespace), cleanupKey("Secret", d.i.namespace, "storage-key")} {
				if _, found := api.objects[retained]; !found {
					t.Fatal("an uncertain object or the recovery custody was removed")
				}
			}
		})
	}
	t.Run("missing-bundle", func(t *testing.T) {
		d, api, ctx := platformCleanupFixture(t)
		delete(api.objects, cleanupKey("bundles.trust.cert-manager.io", "", trustBundle))
		if d.cleanupRestore(ctx) == nil {
			t.Fatal("a CA bundle ConfigMap without its Bundle allowed namespace disposal")
		}
	})
}

// The generated-limit table must name the rules the repository declares, so a
// rename fails here instead of retaining every restored copy.
func TestGeneratedLimitsMatchTheirDeclaredPolicies(t *testing.T) {
	declared := map[string][2]string{}
	for _, path := range []string{"add-default-limitrange.yaml", "add-ns-quota.yaml"} {
		data, err := os.ReadFile("../../k8s/bases/infrastructure/cluster-policies/best-practices/" + path)
		if err != nil {
			t.Fatal(err)
		}
		var policy object
		if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&policy); err != nil {
			t.Fatal(err)
		}
		for _, rule := range items(child(policy, "spec"), "rules") {
			generate := child(rule, "generate")
			if text(generate, "apiVersion") != "v1" || text(generate, "namespace") != "{{request.object.metadata.name}}" || generate["synchronize"] != true {
				continue
			}
			key := text(generate, "kind") + "/" + text(generate, "name")
			if _, seen := declared[key]; !seen {
				declared[key] = [2]string{text(child(policy, "metadata"), "name"), text(rule, "name")}
			}
		}
	}
	for key, rule := range generatedLimits {
		if declared[key] != rule {
			t.Fatalf("%s is generated by %v in the repository, not %v", key, declared[key], rule)
		}
	}
}

// inventoryBindings is the complete inventory of the fixture namespace, with
// the records the platform adds while a reader runs.
func inventoryBindings(d *recoveryDriver, api *cleanupAPI) []namespaceinventory.Binding {
	resources := map[string]string{"Pod": "pods", "PersistentVolumeClaim": "persistentvolumeclaims", "Secret": "secrets", "ConfigMap": "configmaps", "ServiceAccount": "serviceaccounts", "CiliumNetworkPolicy": "ciliumnetworkpolicies.cilium.io", "NetworkPolicy": "networkpolicies.networking.k8s.io", "LimitRange": "limitranges", "ResourceQuota": "resourcequotas", "CiliumEndpoint": "ciliumendpoints.cilium.io"}
	var bindings []namespaceinventory.Binding
	for _, o := range api.objects {
		m := child(o, "metadata")
		if text(m, "namespace") == d.i.namespace {
			bindings = append(bindings, namespaceinventory.Binding{Name: text(m, "name"), Namespace: d.i.namespace, UID: text(m, "uid"), Kind: text(o, "kind"), Resource: resources[text(o, "kind")]})
		}
	}
	return append(bindings,
		namespaceinventory.Binding{Name: "reader.18a1", Namespace: d.i.namespace, UID: "event-uid", Kind: "Event", Resource: "events"},
		namespaceinventory.Binding{Name: "reader.18a1", Namespace: d.i.namespace, UID: "event-uid", Kind: "Event", Resource: "events.events.k8s.io"},
		namespaceinventory.Binding{Name: "service-account-uid", Namespace: d.i.namespace, UID: "report-uid", Kind: "PolicyReport", Resource: "policyreports.wgpolicyk8s.io"},
		namespaceinventory.Binding{Name: fixtureReaderUID + "-k2x9q", Namespace: d.i.namespace, UID: "ephemeral-uid", Kind: "EphemeralReport", Resource: "ephemeralreports.reports.kyverno.io"},
	)
}

const fixtureReaderUID = "0f1e2d3c-4b5a-4968-8776-a5b4c3d2e1f0"

// completeInventoryFixture is a running reader's namespace: every platform
// object, the default ServiceAccount and Cilium's endpoint for the reader.
func completeInventoryFixture(t *testing.T) (*recoveryDriver, *cleanupAPI, context.Context) {
	t.Helper()
	d, api, ctx := platformCleanupFixture(t)
	reader := api.objects[cleanupKey("Pod", d.i.namespace, "reader")]
	child(reader, "metadata")["uid"] = fixtureReaderUID
	ref := d.refs[cleanupKey("Pod", d.i.namespace, "reader")]
	ref.uid = fixtureReaderUID
	d.refs[ref.key()] = ref
	d.created = map[string]bool{}
	for _, owned := range d.refs {
		d.created[owned.uid] = true
	}
	api.objects[cleanupKey("ServiceAccount", d.i.namespace, "default")] = object{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": object{"name": "default", "namespace": d.i.namespace, "uid": "service-account-uid"}}
	api.objects[cleanupKey("ciliumendpoints.cilium.io", d.i.namespace, "reader")] = object{"apiVersion": "cilium.io/v2", "kind": "CiliumEndpoint", "metadata": object{"name": "reader", "namespace": d.i.namespace, "uid": "endpoint-uid", "ownerReferences": []any{object{"apiVersion": "v1", "kind": "Pod", "name": "reader", "uid": fixtureReaderUID}}}}
	return d, api, ctx
}

func TestCompleteInventoryAcceptsOwnedAndPlatformObjectsOnly(t *testing.T) {
	d, api, ctx := completeInventoryFixture(t)
	if err := d.classifyInventory(ctx, inventoryBindings(d, api)); err != nil {
		t.Fatalf("a namespace holding only owned and platform objects was refused: %s", err)
	}
	foreign := func(kind, resource, name, uid string) func([]namespaceinventory.Binding) []namespaceinventory.Binding {
		return func(b []namespaceinventory.Binding) []namespaceinventory.Binding {
			return append(b, namespaceinventory.Binding{Name: name, Namespace: d.i.namespace, UID: uid, Kind: kind, Resource: resource})
		}
	}
	replace := func(kind string, change func(*namespaceinventory.Binding)) func([]namespaceinventory.Binding) []namespaceinventory.Binding {
		return func(b []namespaceinventory.Binding) []namespaceinventory.Binding {
			for i := range b {
				if b[i].Kind == kind {
					change(&b[i])
				}
			}
			return b
		}
	}
	mutations := map[string]func([]namespaceinventory.Binding) []namespaceinventory.Binding{
		"role-binding":            foreign("RoleBinding", "rolebindings.rbac.authorization.k8s.io", "reader", "foreign"),
		"unknown-custom-object":   foreign("Widget", "widgets.example.io", "widget", "foreign"),
		"another-limit-range":     foreign("LimitRange", "limitranges", "another-limitrange", "foreign"),
		"limit-from-another-api":  foreign("LimitRange", "limitranges.example.io", "default-limitrange", "foreign"),
		"another-config-map":      foreign("ConfigMap", "configmaps", "another", "foreign"),
		"another-service-account": foreign("ServiceAccount", "serviceaccounts", "reader", "foreign"),
		"report-on-unknown-uid":   foreign("PolicyReport", "policyreports.wgpolicyk8s.io", "11111111-1111-4111-8111-111111111111", "foreign"),
		"report-on-a-report":      foreign("PolicyReport", "policyreports.wgpolicyk8s.io", "report-uid", "foreign"),
		"ephemeral-unknown-uid":   foreign("EphemeralReport", "ephemeralreports.reports.kyverno.io", "11111111-1111-4111-8111-111111111111-k2x9q", "foreign"),
		"ephemeral-malformed":     foreign("EphemeralReport", "ephemeralreports.reports.kyverno.io", fixtureReaderUID, "foreign"),
		"endpoint-of-another-pod": foreign("CiliumEndpoint", "ciliumendpoints.cilium.io", "foreign", "foreign"),
		"event-from-another-api":  foreign("Event", "events.example.io", "reader.2", "foreign"),
		"replaced-owned-secret": replace("Secret", func(b *namespaceinventory.Binding) {
			if b.Name == "storage-key" {
				b.UID = "replacement"
			}
		}),
		"owned-kind-from-another-api": replace("Pod", func(b *namespaceinventory.Binding) { b.Resource = "pods.example.io" }),
		"object-in-another-namespace": replace("LimitRange", func(b *namespaceinventory.Binding) { b.Namespace = "another" }),
		"missing-owned-object": func(b []namespaceinventory.Binding) []namespaceinventory.Binding {
			var kept []namespaceinventory.Binding
			for _, binding := range b {
				if binding.Kind != "Secret" {
					kept = append(kept, binding)
				}
			}
			return kept
		},
		"duplicate-name": func(b []namespaceinventory.Binding) []namespaceinventory.Binding {
			for _, binding := range b {
				if binding.Kind == "ServiceAccount" {
					binding.UID = "second"
					return append(b, binding)
				}
			}
			return b
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			d, api, ctx := completeInventoryFixture(t)
			if d.classifyInventory(ctx, mutate(inventoryBindings(d, api))) == nil {
				t.Fatal("an object that is neither owned nor platform-added was accepted")
			}
		})
	}
	endpoint := map[string]func(object){
		"another-owner": func(o object) { items(child(o, "metadata"), "ownerReferences")[0]["uid"] = "another-pod" },
		"another-kind":  func(o object) { items(child(o, "metadata"), "ownerReferences")[0]["kind"] = "Deployment" },
		"two-owners": func(o object) {
			child(o, "metadata")["ownerReferences"] = append(child(o, "metadata")["ownerReferences"].([]any), object{"kind": "Pod"})
		},
		"replaced-object":  func(o object) { child(o, "metadata")["uid"] = "replacement" },
		"another-api-kind": func(o object) { o["apiVersion"] = "cilium.io/v3" },
	}
	for name, mutate := range endpoint {
		t.Run("endpoint-"+name, func(t *testing.T) {
			d, api, ctx := completeInventoryFixture(t)
			// The inventory was read first; the endpoint read follows it.
			bindings := inventoryBindings(d, api)
			mutate(api.objects[cleanupKey("ciliumendpoints.cilium.io", d.i.namespace, "reader")])
			if d.classifyInventory(ctx, bindings) == nil {
				t.Fatal("an endpoint not bound to the created reader was accepted")
			}
		})
	}
	t.Run("endpoint-read-fails", func(t *testing.T) {
		d, api, ctx := completeInventoryFixture(t)
		d.k = failingReads{base: api, resource: "ciliumendpoints.cilium.io"}
		if d.classifyInventory(ctx, inventoryBindings(d, api)) == nil {
			t.Fatal("an unreadable endpoint was accepted")
		}
	})
}

type failingReads struct {
	base     kubernetes
	resource string
}

func (f failingReads) call(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if len(args) > 1 && args[0] == "get" && args[1] == f.resource {
		return nil, errors.New("read-failed")
	}
	return f.base.call(ctx, input, args...)
}

// A report about the reader stays attributable after the reader is removed.
func TestCompleteInventoryKeepsRemovedOwnedObjectsAttributable(t *testing.T) {
	d, api, ctx := completeInventoryFixture(t)
	reader := cleanupKey("Pod", d.i.namespace, "reader")
	delete(api.objects, reader)
	delete(api.objects, cleanupKey("ciliumendpoints.cilium.io", d.i.namespace, "reader"))
	delete(d.refs, reader)
	if err := d.classifyInventory(ctx, inventoryBindings(d, api)); err != nil {
		t.Fatalf("a report about the removed reader was refused: %s", err)
	}
	d.created = map[string]bool{}
	if d.classifyInventory(ctx, inventoryBindings(d, api)) == nil {
		t.Fatal("a report about an object this invocation never created was accepted")
	}
}

// No restore may start without the complete inventory that disposal needs, and
// the refusal comes before any controller or credential read.
func TestRecoveryRefusesToStartWithoutTheCompleteInventory(t *testing.T) {
	t.Chdir("../..")
	image, err := loadRecoveryImage()
	if err != nil {
		t.Fatal(err)
	}
	pod, pvc, pv := servingFixture()
	items(child(pod, "spec"), "containers")[0]["image"] = image
	reads := &sourceOnlyAPI{objects: map[string]object{"Pod": pod, "PersistentVolumeClaim": pvc, "PersistentVolume": pv}}
	env := confirmedInvocation()
	i, err := authorizeDataRecovery(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	d := &recoveryDriver{i: i, k: reads, env: func(k string) string { return env[k] }, refs: map[string]ownedReference{}, stage: "preflight"}
	if _, err := d.preflight(context.Background()); err == nil || err.Error() != "inventory-credential" {
		t.Fatalf("recovery started without the complete inventory credential: %v", err)
	}
	if reads.other != 0 || reads.source != 3 {
		t.Fatalf("preflight read beyond the source before refusing: source=%d other=%d", reads.source, reads.other)
	}
}

type sourceOnlyAPI struct {
	objects       map[string]object
	source, other int
}

func (a *sourceOnlyAPI) call(_ context.Context, _ []byte, args ...string) ([]byte, error) {
	if len(args) > 2 && args[0] == "get" {
		if o, found := a.objects[args[1]]; found {
			a.source++
			return json.Marshal(o)
		}
	}
	a.other++
	return nil, errors.New("unexpected-read")
}
