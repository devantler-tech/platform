package compiler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	kroapi "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	kroruntime "github.com/kubernetes-sigs/kro/pkg/runtime"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const tenantDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const tenantPublisher = "467f07582436a26b62cf4aa756423512f85ba0c0"
const legacyTenantSubject = `^https://github\.com/devantler-tech/(actions/\.github/workflows/publish-app\.yaml@[0-9a-f]{40}|\.github/\.github/workflows/publish-app\.yaml@(0c7b6c3509b73adbc7537e030935bb01987e625b|467f07582436a26b62cf4aa756423512f85ba0c0))$`

// TestCommittedTenant exercises the actual graph compiler and generated API.
// Desired objects are expression evidence, not Flux or signature verification.
func TestCommittedTenant(t *testing.T) {
	root := os.Getenv("PLATFORM_TEST_ROOT")
	if root == "" || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("explicit source root and verified API-server assets are required")
	}
	if features.FeatureGate.Enabled(features.CELOmitFunction) {
		t.Fatal("Tenant compilation must use the deployed omission feature gate")
	}
	var definition kroapi.ResourceGraphDefinition
	readYAML(t, filepath.Join(root, "k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml"), &definition)
	if definition.APIVersion != "kro.run/v1alpha1" || definition.Kind != "ResourceGraphDefinition" || definition.Name != "tenant.kro.run" {
		t.Fatal("expected the committed Tenant graph")
	}
	var crds []*extv1.CustomResourceDefinition
	for _, fixture := range []struct {
		variable, name, group, version string
		scope                          extv1.ResourceScope
	}{
		{"KRO_GRAPH_CRD", "resourcegraphdefinitions.kro.run", "kro.run", "v1alpha1", extv1.ClusterScoped},
		{"FLUX_OCI_CRD", "ocirepositories.source.toolkit.fluxcd.io", "source.toolkit.fluxcd.io", "v1", extv1.NamespaceScoped},
		{"FLUX_KUSTOMIZATION_CRD", "kustomizations.kustomize.toolkit.fluxcd.io", "kustomize.toolkit.fluxcd.io", "v1", extv1.NamespaceScoped},
		{"EXTERNAL_SECRET_CRD", "externalsecrets.external-secrets.io", "external-secrets.io", "v1", extv1.NamespaceScoped},
	} {
		var crd extv1.CustomResourceDefinition
		readYAML(t, os.Getenv(fixture.variable), &crd)
		if crd.Name != fixture.name || crd.Spec.Group != fixture.group || crd.Spec.Scope != fixture.scope {
			t.Fatalf("wrong actual schema for %s: %s", fixture.variable, crd.Name)
		}
		if !slices.ContainsFunc(crd.Spec.Versions, func(version extv1.CustomResourceDefinitionVersion) bool {
			return version.Name == fixture.version && version.Served && version.Schema != nil && version.Schema.OpenAPIV3Schema != nil
		}) {
			t.Fatalf("actual schema %s does not serve the compiled version %s", fixture.name, fixture.version)
		}
		crds = append(crds, &crd)
	}
	environment := newCompilerEnvironment(t, crds)
	config, err := startCompilerEnvironment(environment)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	config.Timeout = 15 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	scheme := runtime.NewScheme()
	if err := kroapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := extv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	apiClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Create(ctx, &definition); err != nil {
		t.Fatal(err)
	}
	discover, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"source.toolkit.fluxcd.io/v1", "kustomize.toolkit.fluxcd.io/v1", "external-secrets.io/v1"} {
		if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(context.Context) (bool, error) {
			paths, err := discover.OpenAPIV3().Paths()
			if err != nil {
				return false, err
			}
			path, ok := paths["apis/"+group]
			if !ok {
				return false, nil
			}
			_, err = path.Schema("application/json")
			return err == nil, err
		}); err != nil {
			t.Fatalf("actual dependency OpenAPI %s: %v", group, err)
		}
	}
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := graph.NewBuilder(config, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := builder.NewResourceGraphDefinition(&definition, graph.Config{MaxCollectionDimensionSize: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := completeTenant(compiled); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Create(ctx, compiled.CRD); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(context.Context) (bool, error) {
		resources, err := discover.ServerResourcesForGroupVersion("kro.run/v1alpha1")
		if err != nil {
			return false, err
		}
		for _, resource := range resources.APIResources {
			if resource.Name == "tenants" && resource.Kind == "Tenant" && resource.Namespaced {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	apiClient, err = client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	instance := func(name string, spec map[string]interface{}) *unstructured.Unstructured {
		spec["name"] = "tenant-fixture"
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kro.run/v1alpha1", "kind": "Tenant",
			"metadata": map[string]interface{}{"name": name, "namespace": "default"}, "spec": spec,
		}}
	}
	admit := func(input *unstructured.Unstructured) error {
		return apiClient.Create(ctx, input, &client.CreateOptions{FieldValidation: "Strict", DryRun: []string{metav1.DryRunAll}})
	}
	for _, sops := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitted-source-sops-%t", sops), func(t *testing.T) {
			input := instance("default-fixture", map[string]interface{}{"sops": sops})
			if err := admit(input); err != nil {
				t.Fatal(err)
			}
			objects, ignored := desiredTenant(t, compiled, input)
			assertTenantChildren(t, objects, ignored, true, sops)
			source := objects["ociRepository"]
			ref, _, _ := unstructured.NestedMap(source.Object, "spec", "ref")
			if !reflect.DeepEqual(ref, map[string]interface{}{"semver": ">=1.0.0"}) {
				t.Fatalf("omitted source changed the existing selector: %v", ref)
			}
			assertTenantIdentity(t, source, legacyTenantSubject)
			writeTenantFixture(t, fmt.Sprintf("default-%t", sops), objects)
		})
		t.Run(fmt.Sprintf("pinned-source-only-sops-%t", sops), func(t *testing.T) {
			input := instance("pinned-fixture", map[string]interface{}{
				"sops":   sops,
				"source": map[string]interface{}{"digest": tenantDigest, "publisherRevision": tenantPublisher, "reconcileApplication": false},
			})
			if err := admit(input); err != nil {
				t.Fatalf("valid source-only input must be admitted: %v", err)
			}
			objects, ignored := desiredTenant(t, compiled, input)
			assertTenantChildren(t, objects, ignored, false, sops)
			source := objects["ociRepository"]
			ref, _, _ := unstructured.NestedMap(source.Object, "spec", "ref")
			if !reflect.DeepEqual(ref, map[string]interface{}{"digest": tenantDigest}) {
				t.Fatalf("pinned source must have exactly the selected digest: %v", ref)
			}
			assertTenantIdentity(t, source, `^https://github\.com/devantler-tech/\.github/\.github/workflows/publish-app\.yaml@467f07582436a26b62cf4aa756423512f85ba0c0$`)
		})
	}
	for _, tc := range []struct {
		name string
		spec map[string]interface{}
	}{
		{"source-only without exact publication", map[string]interface{}{"source": map[string]interface{}{"reconcileApplication": false}}},
		{"source-only with empty publication", map[string]interface{}{"source": map[string]interface{}{"digest": "", "publisherRevision": "", "reconcileApplication": false}}},
		{"obsolete top-level lifecycle", map[string]interface{}{"reconcileApplication": false}},
		{"digest without publisher", map[string]interface{}{"source": map[string]interface{}{"digest": tenantDigest}}},
		{"publisher without digest", map[string]interface{}{"source": map[string]interface{}{"publisherRevision": tenantPublisher}}},
		{"tag selector", map[string]interface{}{"source": map[string]interface{}{"digest": "v1.0.3", "publisherRevision": tenantPublisher}}},
		{"uppercase digest", map[string]interface{}{"source": map[string]interface{}{"digest": strings.ToUpper(tenantDigest), "publisherRevision": tenantPublisher}}},
		{"short digest", map[string]interface{}{"source": map[string]interface{}{"digest": "sha256:abc", "publisherRevision": tenantPublisher}}},
		{"branch publisher", map[string]interface{}{"source": map[string]interface{}{"digest": tenantDigest, "publisherRevision": "main"}}},
		{"publisher regex", map[string]interface{}{"source": map[string]interface{}{"digest": tenantDigest, "publisherRevision": "[0-9a-f]{40}"}}},
		{"source URL override", map[string]interface{}{"source": map[string]interface{}{"url": "oci://example.invalid/other"}}},
		{"identity override", map[string]interface{}{"source": map[string]interface{}{"subject": ".*"}}},
		{"wrong lifecycle type", map[string]interface{}{"source": map[string]interface{}{"reconcileApplication": "false"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := admit(instance("invalid-fixture", tc.spec))
			if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
				t.Fatalf("expected strict API refusal, not success or an operational error: %v", err)
			}
		})
	}
	for _, mode := range []string{"explicit", "omitted"} {
		t.Run("cannot-prune-active-"+mode, func(t *testing.T) {
			spec := map[string]interface{}{}
			if mode == "explicit" {
				spec["source"] = map[string]interface{}{"reconcileApplication": true}
			}
			input := instance("active-"+mode, spec)
			if err := apiClient.Create(ctx, input, &client.CreateOptions{FieldValidation: "Strict"}); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedMap(input.Object, map[string]interface{}{"digest": tenantDigest, "publisherRevision": tenantPublisher, "reconcileApplication": false}, "spec", "source"); err != nil {
				t.Fatal(err)
			}
			err := apiClient.Update(ctx, input, &client.UpdateOptions{FieldValidation: "Strict"})
			if !apierrors.IsInvalid(err) {
				t.Fatalf("disabling an active child must be refused, not pruned: %v", err)
			}
		})
	}
	t.Run("source-only-can-activate", func(t *testing.T) {
		input := instance("activation-fixture", map[string]interface{}{
			"source": map[string]interface{}{"digest": tenantDigest, "publisherRevision": tenantPublisher, "reconcileApplication": false},
		})
		if err := apiClient.Create(ctx, input, &client.CreateOptions{FieldValidation: "Strict"}); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(input.Object, true, "spec", "source", "reconcileApplication"); err != nil {
			t.Fatal(err)
		}
		if err := apiClient.Update(ctx, input, &client.UpdateOptions{FieldValidation: "Strict"}); err != nil {
			t.Fatal(err)
		}
		objects, ignored := desiredTenant(t, compiled, input)
		assertTenantChildren(t, objects, ignored, true, false)
		ref, _, _ := unstructured.NestedMap(objects["ociRepository"].Object, "spec", "ref")
		if !reflect.DeepEqual(ref, map[string]interface{}{"digest": tenantDigest}) {
			t.Fatal("activation must not change the selected source")
		}
	})
	for _, mode := range []string{"omitted", "empty object", "null", "empty pair"} {
		t.Run("pinned-source-cannot-become-floating-"+mode, func(t *testing.T) {
			input := instance("pin-removal-"+strings.ReplaceAll(mode, " ", "-"), map[string]interface{}{
				"source": map[string]interface{}{"digest": tenantDigest, "publisherRevision": tenantPublisher},
			})
			if err := apiClient.Create(ctx, input, &client.CreateOptions{FieldValidation: "Strict"}); err != nil {
				t.Fatal(err)
			}
			spec := input.Object["spec"].(map[string]interface{})
			switch mode {
			case "omitted":
				delete(spec, "source")
			case "empty object":
				spec["source"] = map[string]interface{}{}
			case "null":
				spec["source"] = nil
			case "empty pair":
				spec["source"] = map[string]interface{}{"digest": "", "publisherRevision": ""}
			}
			if err := apiClient.Update(ctx, input, &client.UpdateOptions{FieldValidation: "Strict"}); !apierrors.IsInvalid(err) {
				t.Fatalf("removing the selected publication via %s must be refused: %v", mode, err)
			}
		})
	}
	t.Run("pre-upgrade-active-tenant-cannot-be-disabled", func(t *testing.T) {
		// Store an actual legacy object under a schema without the new fields.
		// Creating an omitted field under the new schema would only test defaults.
		var current extv1.CustomResourceDefinition
		if err := apiClient.Get(ctx, client.ObjectKey{Name: compiled.CRD.Name}, &current); err != nil {
			t.Fatal(err)
		}
		legacy := current.DeepCopy()
		for index := range legacy.Spec.Versions {
			spec := legacy.Spec.Versions[index].Schema.OpenAPIV3Schema.Properties["spec"]
			delete(spec.Properties, "source")
			legacy.Spec.Versions[index].Schema.OpenAPIV3Schema.Properties["spec"] = spec
		}
		if err := apiClient.Update(ctx, legacy); err != nil {
			t.Fatal(err)
		}
		if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(context.Context) (bool, error) {
			err := admit(instance("legacy-readiness", map[string]interface{}{"source": map[string]interface{}{"reconcileApplication": true}}))
			return apierrors.IsBadRequest(err), nil
		}); err != nil {
			t.Fatal(err)
		}
		input := instance("pre-upgrade-fixture", map[string]interface{}{})
		if err := apiClient.Create(ctx, input, &client.CreateOptions{FieldValidation: "Strict"}); err != nil {
			t.Fatal(err)
		}
		if _, found, err := unstructured.NestedBool(input.Object, "spec", "source", "reconcileApplication"); err != nil || found {
			t.Fatalf("fixture was not stored before the new default existed: found=%t error=%v", found, err)
		}
		if err := apiClient.Get(ctx, client.ObjectKey{Name: compiled.CRD.Name}, &current); err != nil {
			t.Fatal(err)
		}
		current.Spec = *compiled.CRD.Spec.DeepCopy()
		if err := apiClient.Update(ctx, &current); err != nil {
			t.Fatal(err)
		}
		if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(context.Context) (bool, error) {
			return admit(instance("upgrade-readiness", map[string]interface{}{"source": map[string]interface{}{"digest": tenantDigest, "publisherRevision": tenantPublisher, "reconcileApplication": false}})) == nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := apiClient.Get(ctx, client.ObjectKeyFromObject(input), input); err != nil {
			t.Fatal(err)
		}
		objects, ignored := desiredTenant(t, compiled, input)
		assertTenantChildren(t, objects, ignored, true, false)
		if err := unstructured.SetNestedMap(input.Object, map[string]interface{}{"digest": tenantDigest, "publisherRevision": tenantPublisher, "reconcileApplication": false}, "spec", "source"); err != nil {
			t.Fatal(err)
		}
		if err := apiClient.Update(ctx, input, &client.UpdateOptions{FieldValidation: "Strict"}); !apierrors.IsInvalid(err) {
			t.Fatalf("legacy active tenant must not become a pruning toggle: %v", err)
		}
	})
	t.Run("partial-graph-is-not-complete", func(t *testing.T) {
		input := definition.DeepCopy()
		input.Spec.Resources = input.Spec.Resources[:1]
		partial, err := builder.NewResourceGraphDefinition(input, graph.Config{MaxCollectionDimensionSize: 5})
		if err != nil {
			t.Fatal(err)
		}
		if completeTenant(partial) == nil {
			t.Fatal("one examined child cannot claim complete Tenant coverage")
		}
	})
}

func completeTenant(compiled *graph.Graph) error {
	if compiled == nil || compiled.CRD == nil || compiled.CRD.Spec.Scope != extv1.NamespaceScoped ||
		compiled.CRD.Spec.Group != "kro.run" || compiled.CRD.Spec.Names.Kind != "Tenant" {
		return fmt.Errorf("missing generated namespaced Tenant API")
	}
	var ids []string
	for id := range compiled.Resources {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	want := []string{"defaultDeny", "externalDnsGateway", "externalDnsHttproutes", "externalDnsNamespaces", "ghcrAuth", "kustomization", "kustomizationSops", "limitRange", "ociRepository", "resourceQuota", "roleBinding", "serviceAccount", "tenantNamespace", "webAppBinding"}
	if !slices.Equal(ids, want) {
		return fmt.Errorf("incomplete Tenant graph: %v", ids)
	}
	return nil
}

func desiredTenant(t *testing.T, compiled *graph.Graph, input *unstructured.Unstructured) (map[string]*unstructured.Unstructured, []string) {
	t.Helper()
	resolved, err := kroruntime.FromGraph(compiled, graph.Config{MaxCollectionDimensionSize: 5}, kroruntime.WithInstance(input))
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]*unstructured.Unstructured{}
	var ignored []string
	for _, node := range resolved.Nodes() {
		ignore, err := node.IsIgnored()
		if err != nil {
			t.Fatalf("evaluate inclusion %s: %v", node.Spec.Meta.ID, err)
		}
		if ignore {
			ignored = append(ignored, node.Spec.Meta.ID)
			continue
		}
		desired, err := node.GetDesired()
		if err != nil || len(desired) != 1 {
			t.Fatalf("resolve actual Tenant child %s: count=%d error=%v", node.Spec.Meta.ID, len(desired), err)
		}
		objects[node.Spec.Meta.ID] = desired[0]
		node.SetObserved(desired)
	}
	slices.Sort(ignored)
	return objects, ignored
}

func assertTenantChildren(t *testing.T, objects map[string]*unstructured.Unstructured, ignored []string, reconcile, sops bool) {
	t.Helper()
	want := []string{"defaultDeny", "ghcrAuth", "ociRepository", "roleBinding", "serviceAccount", "tenantNamespace"}
	wantIgnored := []string{"externalDnsGateway", "externalDnsHttproutes", "externalDnsNamespaces", "limitRange", "resourceQuota", "webAppBinding"}
	if reconcile {
		if sops {
			want = append(want, "kustomizationSops")
			wantIgnored = append(wantIgnored, "kustomization")
		} else {
			want = append(want, "kustomization")
			wantIgnored = append(wantIgnored, "kustomizationSops")
		}
	} else {
		wantIgnored = append(wantIgnored, "kustomization", "kustomizationSops")
	}
	slices.Sort(want)
	slices.Sort(wantIgnored)
	var got []string
	for id := range objects {
		got = append(got, id)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) || !slices.Equal(ignored, wantIgnored) {
		t.Fatalf("included=%v ignored=%v; want included=%v ignored=%v", got, ignored, want, wantIgnored)
	}
	for id, object := range objects {
		if id == "tenantNamespace" {
			if object.GetName() != "tenant-fixture" || object.GetLabels()["pod-security.kubernetes.io/enforce"] != "restricted" {
				t.Fatal("tenant namespace or restricted policy changed")
			}
		} else if object.GetNamespace() != "tenant-fixture" {
			t.Fatalf("child %s escaped the exact tenant namespace", id)
		}
	}
	for _, id := range []string{"kustomization", "kustomizationSops"} {
		if object := objects[id]; object != nil {
			prune, _, _ := unstructured.NestedBool(object.Object, "spec", "prune")
			force, _, _ := unstructured.NestedBool(object.Object, "spec", "force")
			sa, _, _ := unstructured.NestedString(object.Object, "spec", "serviceAccountName")
			if !prune || force || sa != "tenant-fixture" {
				t.Fatal("activation changed prune, force or impersonation")
			}
		}
	}
}

func assertTenantIdentity(t *testing.T, source *unstructured.Unstructured, subject string) {
	t.Helper()
	url, _, _ := unstructured.NestedString(source.Object, "spec", "url")
	verify, _, _ := unstructured.NestedMap(source.Object, "spec", "verify")
	want := map[string]interface{}{"provider": "cosign", "matchOIDCIdentity": []interface{}{map[string]interface{}{
		"issuer": `^https://token\.actions\.githubusercontent\.com$`, "subject": subject,
	}}}
	if url != "oci://ghcr.io/devantler-tech/tenant-fixture/manifests" || !reflect.DeepEqual(verify, want) {
		t.Fatalf("source escaped fixed URL or signer boundary: url=%s verify=%v", url, verify)
	}
}

func writeTenantFixture(t *testing.T, name string, objects map[string]*unstructured.Unstructured) {
	t.Helper()
	directory := os.Getenv("PLATFORM_COMPILED_TENANT_DIR")
	if directory == "" {
		return
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("an explicit absolute fixture output directory is required")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(objects)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
