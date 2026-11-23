package compiler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	kroapi "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	gecompiler "github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// A tenant follows its own stable releases (ADR 0019): a semver range, stable `X.Y.Z` only,
// signed by its own repository's release workflow on a release tag.
const stableReleases = `^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`
const defaultTenantSubject = `^https://github\.com/devantler-tech/tenant\-fixture/\.github/workflows/[^/]+\.ya?ml@refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`

func releaseRef() map[string]interface{} {
	return map[string]interface{}{"semver": ">=0.0.0", "semverFilter": stableReleases}
}

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
	// The instance controller renders children with the graph-engine compiler.
	engine, err := gecompiler.NewCompiler(config, httpClient)
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
			objects, ignored := desiredTenant(t, engine, &definition, input)
			assertTenantChildren(t, objects, ignored, true, sops)
			source := objects["ociRepository"]
			ref, _, _ := unstructured.NestedMap(source.Object, "spec", "ref")
			if !reflect.DeepEqual(ref, releaseRef()) {
				t.Fatalf("omitted source must follow every stable release: %v", ref)
			}
			assertTenantIdentity(t, source, defaultTenantSubject)
			assertDefaultSubjectRefusesSharedPublishers(t, source)
			assertDefaultSubjectTrustsOnlyOwnRelease(t, source, "tenant-fixture")
			writeTenantFixture(t, fmt.Sprintf("default-%t", sops), objects)
		})
		t.Run(fmt.Sprintf("source-only-sops-%t", sops), func(t *testing.T) {
			input := instance("source-only-fixture", map[string]interface{}{
				"sops":   sops,
				"source": map[string]interface{}{"reconcileApplication": false},
			})
			if err := admit(input); err != nil {
				t.Fatalf("valid source-only input must be admitted: %v", err)
			}
			objects, ignored := desiredTenant(t, engine, &definition, input)
			assertTenantChildren(t, objects, ignored, false, sops)
			if !sops {
				t.Run("controller-ownership", func(t *testing.T) {
					assertTenantNetworkFloor(ctx, t, config, root, objects)
				})
			}
			source := objects["ociRepository"]
			ref, _, _ := unstructured.NestedMap(source.Object, "spec", "ref")
			if !reflect.DeepEqual(ref, releaseRef()) {
				t.Fatalf("source-only tenant must still follow every stable release: %v", ref)
			}
			assertTenantIdentity(t, source, defaultTenantSubject)
		})
		// The own-release alternative is built from the tenant name, regex-escaped. A
		// name carrying `.` must not become a wildcard: compile one directly (the
		// Namespace would refuse it, but the subject is checked on its own here).
		t.Run(fmt.Sprintf("own-release-escapes-tenant-name-sops-%t", sops), func(t *testing.T) {
			input := instance("escaped-fixture", map[string]interface{}{"sops": sops})
			if err := unstructured.SetNestedField(input.Object, "tenant.fixture", "spec", "name"); err != nil {
				t.Fatal(err)
			}
			// Admission supplies the schema defaults the graph reads.
			if err := admit(input); err != nil {
				t.Fatal(err)
			}
			objects, _ := desiredTenant(t, engine, &definition, input)
			matcher := compiledSubject(t, objects["ociRepository"])
			for identity, want := range map[string]bool{
				"https://github.com/devantler-tech/tenant.fixture/.github/workflows/release.yaml@refs/tags/v1.2.3": true,
				"https://github.com/devantler-tech/tenantXfixture/.github/workflows/release.yaml@refs/tags/v1.2.3": false,
				"https://github.com/devantler-tech/tenant-fixture/.github/workflows/release.yaml@refs/tags/v1.2.3": false,
			} {
				if got := matcher.MatchString(identity); got != want {
					t.Fatalf("compiled subject for a dotted tenant name accepts %s = %t, want %t", identity, got, want)
				}
			}
		})
		t.Run(fmt.Sprintf("all-opt-ins-sops-%t", sops), func(t *testing.T) {
			input := instance("opt-in-fixture", map[string]interface{}{
				"sops": sops, "externalDns": true, "webApp": true,
				"resourceQuota": map[string]interface{}{"enabled": true},
				"limitRange":    map[string]interface{}{"enabled": true},
			})
			if err := admit(input); err != nil {
				t.Fatal(err)
			}
			objects, ignored := desiredTenant(t, engine, &definition, input)
			optional := []string{"externalDnsGateway", "externalDnsHttproutes", "externalDnsNamespaces", "limitRange", "resourceQuota", "webAppBinding"}
			want := append([]string{"defaultDeny", "ociRepository", "roleBinding", "serviceAccount", "tenantNamespace"}, optional...)
			active, inactive := "kustomization", "kustomizationSops"
			if sops {
				active, inactive = inactive, active
			}
			want = append(want, active)
			slices.Sort(want)
			var got []string
			for id := range objects {
				got = append(got, id)
			}
			slices.Sort(got)
			if !slices.Equal(got, want) || !slices.Equal(ignored, []string{inactive}) {
				t.Fatalf("opt-ins included=%v ignored=%v; want included=%v ignored=[%s]", got, ignored, want, inactive)
			}
			assertTenantControllerMetadata(t, objects)
			for _, id := range append(optional, active) {
				t.Run("reject-obsolete-ownership-"+id, func(t *testing.T) {
					changed := objects[id].DeepCopy()
					labels := changed.GetLabels()
					labels["app.kubernetes.io/managed-by"] = "ksail"
					changed.SetLabels(labels)
					if tenantControllerMetadataError(map[string]*unstructured.Unstructured{id: changed}) == nil {
						t.Fatal("obsolete ownership was not detected on the actual compiled child")
					}
				})
			}
		})
	}
	for _, tc := range []struct {
		name string
		spec map[string]interface{}
	}{
		{"obsolete top-level lifecycle", map[string]interface{}{"reconcileApplication": false}},
		{"obsolete digest pin", map[string]interface{}{"source": map[string]interface{}{"digest": "sha256:" + strings.Repeat("0", 64)}}},
		{"obsolete publisher pin", map[string]interface{}{"source": map[string]interface{}{"publisherRevision": "a76d5df9371cf712a8f60307fde060825a34cf35"}}},
		{"release-range override", map[string]interface{}{"source": map[string]interface{}{"version": "3.12.0"}}},
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
			if err := unstructured.SetNestedMap(input.Object, map[string]interface{}{"reconcileApplication": false}, "spec", "source"); err != nil {
				t.Fatal(err)
			}
			err := apiClient.Update(ctx, input, &client.UpdateOptions{FieldValidation: "Strict"})
			if !apierrors.IsInvalid(err) {
				t.Fatalf("disabling an active child must be refused, not pruned: %v", err)
			}
		})
	}
	// Every committed Tenant, as written and with an omitted source, accepts only its
	// own repository's release workflow (#6265, ADR 0019). A repository named `actions`
	// is no tenant's own, and neither `.github` tags nor shared-publisher revisions are
	// ever accepted.
	t.Run("own-release-for-every-committed-tenant", func(t *testing.T) {
		tenants := committedTenants(t, root)
		if len(tenants) < 2 {
			t.Fatalf("found %d committed Tenant instances; the repository holds at least two", len(tenants))
		}
		names := make([]string, 0, len(tenants))
		for _, committed := range tenants {
			name, _, _ := unstructured.NestedString(committed.Object, "spec", "name")
			names = append(names, name)
		}
		for _, committed := range tenants {
			name, _, _ := unstructured.NestedString(committed.Object, "spec", "name")
			for _, mode := range []string{"as-committed", "unpinned"} {
				t.Run(name+"/"+mode, func(t *testing.T) {
					input := committed.DeepCopy()
					input.SetNamespace("default")
					input.SetName(name + "-" + mode)
					if mode == "unpinned" {
						unstructured.RemoveNestedField(input.Object, "spec", "source")
					}
					if err := admit(input); err != nil {
						t.Fatal(err)
					}
					objects, _ := desiredTenant(t, engine, &definition, input)
					matcher := compiledSubject(t, objects["ociRepository"])
					want := map[string]bool{
						"https://github.com/devantler-tech/" + name + "/.github/workflows/release.yaml@refs/tags/v1.2.3":                        true,
						"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35": false,
						"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@refs/tags/v1.2.3":                         false,
						"https://github.com/devantler-tech/actions/.github/workflows/publish-manifests.yaml@refs/tags/v5.4.0":                   false,
						"https://github.com/devantler-tech/actions/.github/workflows/release.yaml@refs/tags/v1.2.3":                             false,
						"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@" + strings.Repeat("e", 40):               false,
						"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@refs/tags/v1.2.3":                         false,
					}
					for _, other := range names {
						if other != name {
							want["https://github.com/devantler-tech/"+other+"/.github/workflows/release.yaml@refs/tags/v1.2.3"] = false
						}
					}
					for identity, accepted := range want {
						if got := matcher.MatchString(identity); got != accepted {
							t.Fatalf("committed tenant %s (%s) accepts %s = %t, want %t", name, mode, identity, got, accepted)
						}
					}
				})
			}
		}
	})
	t.Run("source-only-can-activate", func(t *testing.T) {
		input := instance("activation-fixture", map[string]interface{}{
			"source": map[string]interface{}{"reconcileApplication": false},
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
		objects, ignored := desiredTenant(t, engine, &definition, input)
		assertTenantChildren(t, objects, ignored, true, false)
		ref, _, _ := unstructured.NestedMap(objects["ociRepository"].Object, "spec", "ref")
		if !reflect.DeepEqual(ref, releaseRef()) {
			t.Fatal("activation must not change the selected source")
		}
	})
	t.Run("upgrade-clears-stored-digest-pin", func(t *testing.T) {
		key := client.ObjectKey{Name: compiled.CRD.Name}
		var legacy extv1.CustomResourceDefinition
		if err := apiClient.Get(ctx, key, &legacy); err != nil {
			t.Fatal(err)
		}
		spec := legacy.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
		source := spec.Properties["source"]
		// Before the upgrade, the schema admitted paired digest and publisher pins.
		source.XValidations = []extv1.ValidationRule{
			{Rule: "(!has(self.digest) || self.digest == '') == (!has(self.publisherRevision) || self.publisherRevision == '')"},
			{Rule: "!has(oldSelf.digest) || oldSelf.digest == '' || (has(self.digest) && self.digest != '')"},
		}
		spec.Properties["source"] = source
		legacy.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
		if err := apiClient.Update(ctx, &legacy); err != nil {
			t.Fatal(err)
		}
		pin := map[string]interface{}{"digest": "sha256:" + strings.Repeat("0", 64), "publisherRevision": strings.Repeat("1", 40)}
		legacyInput := func(name string) *unstructured.Unstructured {
			return instance(name, map[string]interface{}{"source": pin})
		}
		if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(context.Context) (bool, error) {
			return admit(legacyInput("legacy-pin-dry-run")) == nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		input := legacyInput("legacy-pin")
		if err := apiClient.Create(ctx, input, &client.CreateOptions{FieldValidation: "Strict", FieldManager: "legacy-pin-owner"}); err != nil {
			t.Fatal(err)
		}
		var upgraded extv1.CustomResourceDefinition
		if err := apiClient.Get(ctx, key, &upgraded); err != nil {
			t.Fatal(err)
		}
		upgraded.Spec = *compiled.CRD.Spec.DeepCopy()
		if err := apiClient.Update(ctx, &upgraded); err != nil {
			t.Fatal(err)
		}
		if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(context.Context) (bool, error) {
			return apierrors.IsInvalid(admit(legacyInput("new-pin-dry-run"))), nil
		}); err != nil {
			t.Fatal(err)
		}
		var committed unstructured.Unstructured
		readYAML(t, filepath.Join(root, "k8s/providers/hetzner/apps/business-site/tenant.yaml"), &committed)
		migratedSource, found, err := unstructured.NestedMap(committed.Object, "spec", "source")
		if err != nil || !found {
			t.Fatalf("read the committed Business Site source: found=%t, error=%v", found, err)
		}
		desired := instance(input.GetName(), map[string]interface{}{"source": migratedSource})
		payload, err := json.Marshal(desired)
		if err != nil {
			t.Fatal(err)
		}
		patch := client.RawPatch(types.ApplyPatchType, payload)
		options := []client.PatchOption{client.FieldOwner("kustomize-controller"), client.ForceOwnership, &client.PatchOptions{FieldValidation: "Strict"}}
		if err := apiClient.Patch(ctx, desired, patch, append(options, client.DryRunAll)...); err != nil {
			t.Fatalf("Flux dry-run must clear a pin owned by another manager: %v", err)
		}
		if err := apiClient.Patch(ctx, desired, patch, options...); err != nil {
			t.Fatalf("Flux apply must clear a stored pin: %v", err)
		}
		for _, field := range []string{"digest", "publisherRevision"} {
			value, _, err := unstructured.NestedString(desired.Object, "spec", "source", field)
			if err != nil || value != "" {
				t.Fatalf("Flux apply left %s set: value=%q, error=%v", field, value, err)
			}
		}
		objects, ignored := desiredTenant(t, engine, &definition, desired)
		assertTenantChildren(t, objects, ignored, true, false)
		assertTenantIdentity(t, objects["ociRepository"], defaultTenantSubject)
	})
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
			return admit(instance("upgrade-readiness", map[string]interface{}{"source": map[string]interface{}{"reconcileApplication": false}})) == nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := apiClient.Get(ctx, client.ObjectKeyFromObject(input), input); err != nil {
			t.Fatal(err)
		}
		objects, ignored := desiredTenant(t, engine, &definition, input)
		assertTenantChildren(t, objects, ignored, true, false)
		if err := unstructured.SetNestedMap(input.Object, map[string]interface{}{"reconcileApplication": false}, "spec", "source"); err != nil {
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
	want := []string{"defaultDeny", "externalDnsGateway", "externalDnsHttproutes", "externalDnsNamespaces", "kustomization", "kustomizationSops", "limitRange", "ociRepository", "resourceQuota", "roleBinding", "serviceAccount", "tenantNamespace", "webAppBinding"}
	if !slices.Equal(ids, want) {
		return fmt.Errorf("incomplete Tenant graph: %v", ids)
	}
	return nil
}

// desiredTenant renders the Tenant's children through the controller's instance
// runtime and reports the children its inclusion conditions leave out.
func desiredTenant(t *testing.T, engine *gecompiler.Compiler, definition *kroapi.ResourceGraphDefinition, input *unstructured.Unstructured) (map[string]*unstructured.Unstructured, []string) {
	t.Helper()
	return resolveChildren(t, engine, definition, input)
}

func assertTenantChildren(t *testing.T, objects map[string]*unstructured.Unstructured, ignored []string, reconcile, sops bool) {
	t.Helper()
	assertTenantControllerMetadata(t, objects)
	want := []string{"defaultDeny", "ociRepository", "roleBinding", "serviceAccount", "tenantNamespace"}
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

// assertDefaultSubjectRefusesDeletedPublisher evaluates the compiled default
// subject as the regex cosign applies: the reviewed maintained revisions are
// accepted and the deleted legacy publisher repository is refused at any
// commit, including the one its final release was cut from (#6264).
func assertDefaultSubjectRefusesSharedPublishers(t *testing.T, source *unstructured.Unstructured) {
	t.Helper()
	identities, _, _ := unstructured.NestedSlice(source.Object, "spec", "verify", "matchOIDCIdentity")
	if len(identities) != 1 {
		t.Fatalf("default source must carry exactly one identity, got %d", len(identities))
	}
	subject, _ := identities[0].(map[string]interface{})["subject"].(string)
	matcher, err := regexp.Compile(subject)
	if err != nil {
		t.Fatalf("compiled default subject is not a regex: %v", err)
	}
	for identity, want := range map[string]bool{
		"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812": false,
		"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35": false,
		"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328": false,
		"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@" + strings.Repeat("b", 40):               false,
		"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812": false,
	} {
		if got := matcher.MatchString(identity); got != want {
			t.Fatalf("compiled default subject accepts %s = %t, want %t", identity, got, want)
		}
	}
}

// committedTenants returns every Tenant instance committed under k8s/, read from
// each YAML document's own apiVersion and kind.
func committedTenants(t *testing.T, root string) []*unstructured.Unstructured {
	t.Helper()
	separator := regexp.MustCompile(`(?m)^---[ \t]*$`)
	var tenants []*unstructured.Unstructured
	err := filepath.WalkDir(filepath.Join(root, "k8s"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // Repository-owned manifests.
		if err != nil {
			return err
		}
		for _, document := range separator.Split(string(data), -1) {
			// A document may be a list, such as a patch file; only a mapping can be a Tenant.
			var parsed interface{}
			if err := yaml.Unmarshal([]byte(document), &parsed); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			object, ok := parsed.(map[string]interface{})
			if ok && object["apiVersion"] == "kro.run/v1alpha1" && object["kind"] == "Tenant" {
				tenants = append(tenants, &unstructured.Unstructured{Object: object})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tenants
}

// compiledSubject returns the one compiled source subject as the regex Flux
// applies to the signing certificate's identity.
func compiledSubject(t *testing.T, source *unstructured.Unstructured) *regexp.Regexp {
	t.Helper()
	identities, _, _ := unstructured.NestedSlice(source.Object, "spec", "verify", "matchOIDCIdentity")
	if len(identities) != 1 {
		t.Fatalf("source must carry exactly one identity, got %d", len(identities))
	}
	subject, _ := identities[0].(map[string]interface{})["subject"].(string)
	matcher, err := regexp.Compile(subject)
	if err != nil {
		t.Fatalf("compiled subject is not a regex: %v", err)
	}
	return matcher
}

// assertDefaultSubjectTrustsOnlyOwnRelease evaluates the compiled default
// subject against release identities (#6265): the tenant's own repository's
// release workflow on a release tag verifies, pre-releases included, and the
// shared publisher keeps its reviewed revisions. A lookalike or another
// tenant's repository, a branch, a non-release or suffixed tag, a workflow
// outside .github/workflows/ and a tag of the shared publisher all fail.
func assertDefaultSubjectTrustsOnlyOwnRelease(t *testing.T, source *unstructured.Unstructured, tenant string) {
	t.Helper()
	matcher := compiledSubject(t, source)
	own := "https://github.com/devantler-tech/" + tenant
	for identity, want := range map[string]bool{
		own + "/.github/workflows/release.yaml@refs/tags/v1.2.3":                                                                         true,
		own + "/.github/workflows/cd.yml@refs/tags/v1.2.3-rc.1":                                                                          true,
		own + "/.github/workflows/release.yaml@refs/tags/v0.0.0-alpha.0.beta-2":                                                          true,
		"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35":          false,
		own + "-x/.github/workflows/release.yaml@refs/tags/v1.2.3":                                                                       false,
		"https://github.com/devantler-tech/x" + tenant + "/.github/workflows/release.yaml@refs/tags/v1.2.3":                              false,
		"https://github.com/devantler-tech/" + strings.ReplaceAll(tenant, "-", "X") + "/.github/workflows/release.yaml@refs/tags/v1.2.3": false,
		"https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3":                                  false,
		"https://github.com/devantler-tech/ascoachingogvaner/.github/workflows/release.yaml@refs/tags/v1.2.3":                            false,
		"https://github.com/another-org/" + tenant + "/.github/workflows/release.yaml@refs/tags/v1.2.3":                                  false,
		own + "/.github/workflows/release.yaml@refs/heads/main":                                                                          false,
		own + "/.github/workflows/release.yaml@refs/pull/1/merge":                                                                        false,
		own + "/.github/workflows/release.yaml@" + strings.Repeat("a", 40):                                                               false,
		own + "/.github/workflows/release.yaml@refs/tags/v1.2":                                                                           false,
		own + "/.github/workflows/release.yaml@refs/tags/1.2.3":                                                                          false,
		own + "/.github/workflows/release.yaml@refs/tags/latest":                                                                         false,
		own + "/.github/workflows/release.yaml@refs/tags/v01.2.3":                                                                        false,
		own + "/.github/workflows/release.yaml@refs/tags/v1.2.3+build.1":                                                                 false,
		own + "/.github/workflows/release.yaml@refs/tags/v1.2.3-":                                                                        false,
		own + "/.github/workflows/release.yaml@refs/tags/v1.2.3/../evil":                                                                 false,
		own + "/.github/workflows/release.yaml@refs/tags/v1.2.3-rc.1/x":                                                                  false,
		own + "/.github/workflows/release.yaml@refs/tags/v1.2.3x":                                                                        false,
		own + "/.github/workflows/nested/release.yaml@refs/tags/v1.2.3":                                                                  false,
		own + "/.github/workflows/../actions/release.yaml@refs/tags/v1.2.3":                                                              false,
		own + "/.github/actions/release.yaml@refs/tags/v1.2.3":                                                                           false,
		own + "/release.yaml@refs/tags/v1.2.3":                                                                                           false,
		own + "/.github/workflows/release.json@refs/tags/v1.2.3":                                                                         false,
		"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@refs/tags/v21.3.4":                                 false,
		"https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@refs/tags/v1.2.3":                                  false,
		// A repository named `actions` is not this tenant's own: its release tags and
		// the deleted publisher's commits are refused here, whatever the image rules do.
		"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@refs/tags/v1.2.3":                         false,
		"https://github.com/devantler-tech/actions/.github/workflows/publish-manifests.yaml@refs/tags/v5.4.0":                   false,
		"https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328": false,
	} {
		if got := matcher.MatchString(identity); got != want {
			t.Fatalf("compiled default subject accepts %s = %t, want %t", identity, got, want)
		}
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
