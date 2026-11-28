package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runtimePublicationSourceFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	config := []byte("# The source file has comments before its document marker.\n---\nspec:\n  cluster:\n    localRegistry: {registry: ghcr.io/devantler-tech/platform/manifests}\n  workload: {sourceDirectory: k8s, kustomizationFile: clusters/prod, tag: latest}\n")
	var stream []byte
	for name, path := range runtimePublicationRoots() {
		encoded, err := json.Marshal(object{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": object{"name": name, "namespace": "flux-system"},
			"spec": object{"path": path, "sourceRef": object{"kind": "OCIRepository", "name": "flux-system"}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(stream) > 0 {
			stream = append(stream, []byte("\n---\n")...)
		}
		stream = append(stream, encoded...)
	}
	return config, stream
}

func TestRuntimePublicationBindsRenderedProductionRoots(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to render the production roots")
	}
	root := filepath.Join("..", "..")
	config, err := os.ReadFile(filepath.Join(root, "ksail.prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wrappers, err := inventoryExecuteCommand(t.Context(), "kubectl", "kustomize", filepath.Join(root, "k8s", "clusters", "prod"))
	if err != nil {
		t.Fatalf("render production roots: %v", err)
	}
	if err := runtimePublicationSource(config, wrappers); err != nil {
		t.Fatalf("publication guard rejected the rendered production roots: %v", err)
	}
}

func TestRuntimePublicationUsesActualProducerAndFluxLayers(t *testing.T) {
	config, wrappers := runtimePublicationSourceFixture(t)
	if runtimePublicationSource(config, wrappers) != nil {
		t.Fatal("canonical producer rejected")
	}
	for _, pair := range [][2][]byte{
		{[]byte(strings.ReplaceAll(string(config), "sourceDirectory: k8s", "sourceDirectory: other")), wrappers},
		{[]byte(strings.ReplaceAll(string(config), "clusters/prod", "clusters/other")), wrappers},
		{[]byte(strings.ReplaceAll(string(config), "tag: latest", "tag: other")), wrappers},
		{config, []byte(strings.ReplaceAll(string(wrappers), `"path":"providers/hetzner/infrastructure/controllers"`, `"patches":[{"patch":"indirect"}],"path":"providers/hetzner/infrastructure/controllers"`))},
		{config, []byte(strings.ReplaceAll(string(wrappers), `"path":"providers/hetzner/infrastructure/controllers"`, `"components":["indirect"],"path":"providers/hetzner/infrastructure/controllers"`))},
		{config, []byte(strings.ReplaceAll(string(wrappers), `"name":"flux-system"`, `"name":"other-source"`))},
		{config, append(append([]byte{}, wrappers...), append([]byte("\n---\n"), wrappers...)...)},
	} {
		if runtimePublicationSource(pair[0], pair[1]) == nil {
			t.Fatal("redirected producer or Flux transformation accepted")
		}
	}
}

func TestRuntimePublicationRefusesTheRetiredPracticeRoot(t *testing.T) {
	config, wrappers := runtimePublicationSourceFixture(t)
	wrappers = append(wrappers, []byte(`
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: business-site-preview
  namespace: flux-system
spec:
  path: providers/hetzner/business-site-preview-workload
  sourceRef:
    kind: OCIRepository
    name: flux-system
`)...)
	if runtimePublicationSource(config, wrappers) == nil {
		t.Fatal("the retired Platform pin for the practice migration was accepted")
	}
}

func TestRuntimePublicationRootSourceCannotBeRedirected(t *testing.T) {
	instance := object{"apiVersion": "fluxcd.controlplane.io/v1", "kind": "FluxInstance", "metadata": object{"name": "flux", "namespace": "flux-system"},
		"spec": object{"kustomize": object{"patches": []any{object{"target": object{"kind": "OCIRepository", "name": "flux-system", "namespace": "flux-system"}, "patch": "- op: add\n  path: /spec/verify\n  value: {provider: cosign}\n"}}}}}
	raw, err := json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	if runtimePublicationFluxSource(raw) != nil {
		t.Fatal("canonical Flux source rejected")
	}
	for _, bad := range [][]byte{[]byte(strings.ReplaceAll(string(raw), "/spec/verify", "/spec/url")), append(append([]byte{}, raw...), append([]byte("\n---\n"), raw...)...), []byte(strings.ReplaceAll(string(raw), `"spec":{`, `"spec":{"sync":{"kind":"OCIRepository","url":"oci://other","ref":"latest","path":"./clusters/prod"},`))} {
		if runtimePublicationFluxSource(bad) == nil {
			t.Fatal("indirect root source accepted")
		}
	}
}
