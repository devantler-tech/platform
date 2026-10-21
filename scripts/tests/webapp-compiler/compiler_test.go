package compiler_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	kroapi "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	// Keep the module that supplies the actual pinned HTTPRoute CRD fixture.
	_ "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"
)

// TestCommittedWebApp compiles the actual source against an isolated API server.
// No controller, scheduler, kubelet, ambient kubeconfig or production client is used.
func TestCommittedWebApp(t *testing.T) {
	root := os.Getenv("PLATFORM_TEST_ROOT")
	if root == "" || os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("explicit source root and verified API-server assets are required")
	}
	definition := readGraph(t, filepath.Join(root,
		"k8s/bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"))
	var release map[string]interface{}
	readYAML(t, filepath.Join(root, "k8s/bases/infrastructure/controllers/kro/helm-release.yaml"), &release)
	gates, found, err := unstructured.NestedMap(release, "spec", "values", "config", "featureGates")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		gates = map[string]interface{}{}
	}
	for name, value := range gates {
		enabled, ok := value.(bool)
		if !ok {
			t.Fatalf("feature gate %s is not boolean", name)
		}
		if err := features.FeatureGate.SetFromMap(map[string]bool{name: enabled}); err != nil {
			t.Fatal(err)
		}
	}
	if features.FeatureGate.Enabled(features.CELOmitFunction) {
		t.Fatal("review the omission characterization before enabling the controller feature")
	}

	var route extv1.CustomResourceDefinition
	readYAML(t, os.Getenv("GATEWAY_HTTPROUTE_CRD"), &route)
	if route.Name != "httproutes.gateway.networking.k8s.io" {
		t.Fatal("expected the pinned upstream HTTPRoute schema")
	}
	var graphCRD extv1.CustomResourceDefinition
	readYAML(t, os.Getenv("KRO_GRAPH_CRD"), &graphCRD)
	if graphCRD.Name != "resourcegraphdefinitions.kro.run" {
		t.Fatal("expected the pinned KRO graph admission schema")
	}
	existing := false
	environment := &envtest.Environment{
		UseExistingCluster:       &existing,
		BinaryAssetsDirectory:    os.Getenv("KUBEBUILDER_ASSETS"),
		ControlPlaneStartTimeout: time.Minute,
		ControlPlaneStopTimeout:  time.Minute,
		CRDs:                     []*extv1.CustomResourceDefinition{&route, &graphCRD},
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	config.Timeout = 15 * time.Second
	// CRD establishment precedes OpenAPI publication. Wait for that real schema,
	// not merely the CRD object; compiling earlier has no HTTPRoute type to check.
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	scheme := runtime.NewScheme()
	if err := kroapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	apiClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	// Admission supplies KRO's schema defaults, including its API group.
	// Never emulate those defaults in this fixture or mutate the source template.
	if err := apiClient.Create(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(context.Context) (bool, error) {
		paths, err := discoveryClient.OpenAPIV3().Paths()
		if err != nil {
			return false, err
		}
		path, ok := paths["apis/gateway.networking.k8s.io/v1"]
		if !ok {
			return false, nil
		}
		_, err = path.Schema("application/json")
		return err == nil, err
	}); err != nil {
		t.Fatalf("HTTPRoute OpenAPI publication did not complete: %v", err)
	}
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := graph.NewBuilder(config, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	compile := func(t *testing.T, input *kroapi.ResourceGraphDefinition) (*graph.Graph, error) {
		t.Helper()
		return builder.NewResourceGraphDefinition(input, graph.RGDConfig{MaxCollectionDimensionSize: 5})
	}
	t.Run("committed graph includes all three children and namespaced schema", func(t *testing.T) {
		got, err := compile(t, definition)
		if err != nil {
			t.Fatal(err)
		}
		if err := completeWebApp(got); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name, expression, want string
	}{
		{"unknown instance field", "${schema.spec.compilerFixtureMissing}", "undefined field"},
		{"wrong output type", "${schema.spec.port}", "type"},
		{"disabled conditional omission", "${omit()}", "omit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := definition.DeepCopy()
			// Image is a real typed string field in the committed Deployment.
			mutateImage(t, input, tc.expression)
			_, err := compile(t, input)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("expected %q compiler refusal, got %v", tc.want, err)
			}
		})
	}
	t.Run("empty graph is rejected", func(t *testing.T) {
		input := definition.DeepCopy()
		input.Spec.Resources = nil
		got, err := compile(t, input)
		if err == nil && completeWebApp(got) == nil {
			t.Fatal("empty input must not report a successful compile")
		}
	})
	t.Run("missing child is not complete coverage", func(t *testing.T) {
		input := definition.DeepCopy()
		input.Spec.Resources = input.Spec.Resources[:1]
		got, err := compile(t, input)
		// KRO accepts partial graphs. The platform's coverage check must not.
		if err != nil {
			t.Fatal(err)
		}
		if completeWebApp(got) == nil {
			t.Fatal("a partial graph must never claim all three children")
		}
	})
}

func completeWebApp(got *graph.Graph) error {
	if got == nil {
		return fmt.Errorf("no compiled graph")
	}
	var ids []string
	for id := range got.Resources {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"deployment", "httpRoute", "service"}) {
		return fmt.Errorf("incomplete graph: %v", ids)
	}
	if got.CRD == nil || got.CRD.Spec.Scope != extv1.NamespaceScoped ||
		got.CRD.Spec.Group != "kro.run" || got.CRD.Spec.Names.Kind != "WebApp" ||
		len(got.CRD.Spec.Versions) != 1 || got.CRD.Spec.Versions[0].Name != "v1alpha1" {
		return fmt.Errorf("missing or wrong generated WebApp API")
	}
	schema := got.CRD.Spec.Versions[0].Schema
	if schema == nil || schema.OpenAPIV3Schema == nil ||
		len(schema.OpenAPIV3Schema.Properties["spec"].Properties) == 0 {
		return fmt.Errorf("generated schema did not examine the instance fields")
	}
	return nil
}

func readYAML(t *testing.T, path string, target interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(data, target); err != nil {
		t.Fatal(err)
	}
}

func readGraph(t *testing.T, path string) *kroapi.ResourceGraphDefinition {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := decodeGraph(data)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func decodeGraph(data []byte) (*kroapi.ResourceGraphDefinition, error) {
	reader := yamlutil.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	document, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read WebApp graph: %w", err)
	}
	if _, err := reader.Read(); err != io.EOF {
		return nil, fmt.Errorf("WebApp graph must contain exactly one YAML document")
	}
	var definition kroapi.ResourceGraphDefinition
	if err := yaml.UnmarshalStrict(document, &definition); err != nil {
		return nil, err
	}
	if definition.APIVersion != "kro.run/v1alpha1" ||
		definition.Kind != "ResourceGraphDefinition" || definition.Name != "webapp.kro.run" {
		return nil, fmt.Errorf("expected the committed WebApp graph")
	}
	return &definition, nil
}

func TestDecodeGraph(t *testing.T) {
	valid := "apiVersion: kro.run/v1alpha1\nkind: ResourceGraphDefinition\nmetadata:\n  name: webapp.kro.run\nspec: {}\n"
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{"valid identity", valid, false},
		{"empty", "", true},
		{"malformed YAML", "metadata: [", true},
		{"unknown typed field", valid + "unexpected: true\n", true},
		{"second document", valid + "---\nother: ignored\n", true},
		{"duplicate field", valid + "kind: Unexpected\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeGraph([]byte(tc.body))
			if (err != nil) != tc.wantError {
				t.Fatalf("want error=%v, got %v", tc.wantError, err)
			}
		})
	}
}

func mutateImage(t *testing.T, input *kroapi.ResourceGraphDefinition, expression string) {
	t.Helper()
	if len(input.Spec.Resources) == 0 || input.Spec.Resources[0].ID != "deployment" {
		t.Fatal("Deployment fixture must be present")
	}
	var body map[string]interface{}
	if err := json.Unmarshal(input.Spec.Resources[0].Template.Raw, &body); err != nil {
		t.Fatal(err)
	}
	containers, found, err := unstructured.NestedSlice(body, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatal("expected one actual application container")
	}
	container, ok := containers[0].(map[string]interface{})
	if !ok {
		t.Fatal("invalid application container")
	}
	container["image"] = expression
	if err := unstructured.SetNestedSlice(body, containers, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	input.Spec.Resources[0].Template.Raw = data
}
