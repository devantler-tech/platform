package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// Exercise the actual source render and native decision without publishing or
// changing production. It does not claim the protected publisher ran locally.
func TestRuntimePublicationNativeReadOnlyDecision(t *testing.T) {
	if os.Getenv("LONGHORN_RUNTIME_NATIVE_READ") != "1" {
		t.Skip("native read-only opt-in is required")
	}
	source := os.Getenv("LONGHORN_RUNTIME_NATIVE_SOURCE")
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(source) {
		t.Fatal("current full main source is required")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := os.ReadFile(filepath.Join(root, "ksail.prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wrappers, err := inventoryExecuteCommand(t.Context(), "kubectl", "kustomize", filepath.Join(root, "k8s/clusters/prod"))
	if err != nil || runtimePublicationSource(configuration, wrappers) != nil {
		t.Fatal("actual producer or Flux wrapper configuration is unbound")
	}
	overlay := t.TempDir()
	// macOS /var is a symlink. Kustomize canonicalizes its root before
	// resolving relative bases, so derive those paths from the same root.
	overlay, err = filepath.EvalSymlinks(overlay)
	if err != nil {
		t.Fatal(err)
	}
	controllers, err := filepath.Rel(overlay, filepath.Join(root, "k8s/providers/hetzner/infrastructure/controllers"))
	if err != nil {
		t.Fatal(err)
	}
	component, err := filepath.Rel(overlay, filepath.Join(root, "k8s/bases/components/longhorn-v1-retirement-runtime"))
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := json.Marshal(object{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
		"resources":  []string{controllers},
		"components": []string{component}})
	if err != nil || os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), declaration, 0600) != nil {
		t.Fatal("native source overlay is unavailable")
	}
	var render []byte
	var paths []string
	for _, path := range runtimePublicationRoots() {
		if path == "providers/hetzner/infrastructure/controllers" {
			paths = append(paths, overlay)
		} else {
			paths = append(paths, filepath.Join(root, "k8s", path))
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		raw, err := inventoryExecuteCommand(t.Context(), "kubectl", "kustomize", path)
		if err != nil {
			t.Fatal("production source render failed")
		}
		if len(render) > 0 {
			render = append(render, []byte("\n---\n")...)
		}
		render = append(render, raw...)
	}
	if runtimePublicationFluxSource(render) != nil {
		t.Fatal("actual root Flux source is unbound")
	}
	image, err := runtimePublicationCandidate(render)
	if err != nil || image != runtimeRetirementImage {
		t.Fatal("effective production candidate is unbound")
	}
	receipt, err := observeRuntimeInventory(t.Context(), runtimeNativeReadClient(t), source)
	if err != nil {
		t.Fatal("native stable observation held:", err)
	}
	verdict, err := runtimePublicationVerdict(image, receipt)
	if err != nil || verdict != "FIT" {
		t.Fatal("native first-selection allocation does not qualify")
	}
	t.Logf("LONGHORN_RUNTIME_PUBLICATION_NATIVE=FIT storageNodes=%d overlapNodes=%d producer=RENDERED source=BOUND writes=0 adoption=UNVERIFIED", receipt.Nodes, receipt.OverlapNodes)
}
