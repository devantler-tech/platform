package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func renderedResources(kinds ...string) []byte {
	var out strings.Builder
	out.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\n")
	for _, kind := range kinds {
		out.WriteString("---\nkind: " + kind + "\nmetadata:\n  name: gateway-receiver\n")
		if strings.HasSuffix(kind, "Binding") {
			out.WriteString("spec:\n  policyName: gateway-receiver\n")
		}
	}
	return []byte(out.String())
}

const (
	mutatingPolicy   = "MutatingAdmissionPolicy"
	validatingPolicy = "ValidatingAdmissionPolicy"
	mutatingBinding  = "MutatingAdmissionPolicyBinding"
	validBinding     = "ValidatingAdmissionPolicyBinding"
)

func TestFenceSelectionFollowsTheRenderedLayer(t *testing.T) {
	policies := []string{mutatingPolicy, validatingPolicy}
	foreignName := append(renderedResources(append(policies, mutatingBinding)...), []byte("---\nkind: "+validBinding+"\nmetadata:\n  name: other\nspec:\n  policyName: gateway-receiver\n")...)
	foreignPolicy := append(renderedResources(append(policies, mutatingBinding)...), []byte("---\nkind: "+validBinding+"\nmetadata:\n  name: gateway-receiver\nspec:\n  policyName: other\n")...)
	for name, tc := range map[string]struct {
		rendered []byte
		selected bool
		fails    bool
	}{
		"unselected":       {rendered: renderedResources(policies...)},
		"selected":         {rendered: renderedResources(append(policies, mutatingBinding, validBinding)...), selected: true},
		"one binding":      {rendered: renderedResources(append(policies, mutatingBinding)...), fails: true},
		"repeated binding": {rendered: renderedResources(append(policies, mutatingBinding, validBinding, validBinding)...), fails: true},
		"binding renamed":  {rendered: foreignName, fails: true},
		"foreign policy":   {rendered: foreignPolicy, fails: true},
		"no policies":      {rendered: renderedResources(), fails: true},
		"one policy":       {rendered: renderedResources(mutatingPolicy), fails: true},
		"bindings only":    {rendered: renderedResources(mutatingBinding, validBinding), fails: true},
		"empty render":     {rendered: nil, fails: true},
		"malformed render": {rendered: []byte("kind: ["), fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			selected, err := fenceSelected(tc.rendered)
			if (err != nil) != tc.fails || selected != tc.selected {
				t.Fatalf("selected=%v err=%v", selected, err)
			}
		})
	}
}

func writeOverlay(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// Selecting the component from a parent overlay must read as selected: only
// the rendered layer can see it, and reading one file's key could not.
func TestRealRendersSeeASelectionFromAnyLevel(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	copyOf := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(receiverSourceDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	fence := "components/physical-peer-fence/"
	files := map[string]string{}
	for _, name := range []string{"kustomization.yaml", "mutating-admission-policy-gateway-receiver.yaml", "validating-admission-policy-gateway-receiver.yaml", fence + "kustomization.yaml", fence + "mutating-admission-policy-binding-gateway-receiver.yaml", fence + "validating-admission-policy-binding-gateway-receiver.yaml"} {
		files["child/"+name] = copyOf(name)
	}
	render := func(parent string) []byte {
		dir := t.TempDir()
		writeOverlay(t, dir, files)
		writeOverlay(t, dir, map[string]string{"kustomization.yaml": parent})
		out, err := exec.Command("kubectl", "kustomize", dir).Output()
		if err != nil {
			t.Fatal("native render")
		}
		return out
	}
	head := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - child\n"
	if selected, err := fenceSelected(render(head)); err != nil || selected {
		t.Fatalf("unselected parent: selected=%v err=%v", selected, err)
	}
	if selected, err := fenceSelected(render(head + "components:\n  - child/components/physical-peer-fence\n")); err != nil || !selected {
		t.Fatalf("parent selection: selected=%v err=%v", selected, err)
	}
}

func TestReviewedLayerStillLeavesTheFenceUnselected(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	rendered, err := renderLayer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := fenceSelected(rendered)
	if err != nil || selected {
		t.Fatalf("selected=%v err=%v: selecting the fence is a separate reviewed activation", selected, err)
	}
}

func TestFenceActiveRequiresEveryTLSChainAndTheOwnershipMarker(t *testing.T) {
	m, b := disabledFixture()
	fenced, _ := json.Marshal(m)
	if err := fenceActive(fenced, b); err != nil {
		t.Fatalf("a fully fenced receiver was refused: %v", err)
	}
	resources := m["spec"].(map[string]any)["resources"].([]any)
	listener := resources[1].(map[string]any)
	chains := listener["filterChains"].([]any)
	second := map[string]any{"filterChainMatch": map[string]any{"transportProtocol": "tls"}}
	listener["filterChains"] = append(chains, second)
	partial, _ := json.Marshal(m)
	if err := fenceActive(partial, b); err == nil {
		t.Fatal("a TLS chain without the peer fence passed as fenced")
	}
	listener["filterChains"] = chains

	meta := m["metadata"].(map[string]any)
	anns := meta["annotations"].(map[string]any)
	delete(anns, receiverOwner)
	unowned, _ := json.Marshal(m)
	if err := fenceActive(unowned, b); err == nil {
		t.Fatal("a fence without its ownership marker passed")
	}
	anns[receiverOwner] = "v1"

	other := b
	other.CIDR = "192.0.2.6/32"
	if err := fenceActive(fenced, other); err == nil {
		t.Fatal("a fence for another peer passed")
	}
}

func TestFenceActiveRefusesADisabledReceiver(t *testing.T) {
	m, b := disabledFixture()
	raw, _ := json.Marshal(m)
	patch, err := disabledPatch(raw, b)
	if err != nil || len(patch) == 0 {
		t.Fatal("fixture is not fenced")
	}
	resources := m["spec"].(map[string]any)["resources"].([]any)
	for _, item := range resources[1].(map[string]any)["filterChains"].([]any) {
		delete(item.(map[string]any)["filterChainMatch"].(map[string]any), "directSourcePrefixRanges")
	}
	delete(m["metadata"].(map[string]any)["annotations"].(map[string]any), receiverOwner)
	disabled, _ := json.Marshal(m)
	if err := fenceActive(disabled, b); err == nil {
		t.Fatal("a disabled receiver passed as fenced")
	}
}

func fakeReceiver(state map[string][]byte, writes *int) receiverClient {
	return receiverClient{context: "admin@prod", command: func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
		for i, arg := range args {
			if arg == "get" {
				return state[args[i+1]], nil
			}
			if arg == "delete" || arg == "patch" {
				*writes++
				return nil, nil
			}
		}
		return nil, errors.New("unexpected")
	}}
}

func TestBindingsSelectedRequiresBothReviewedBindings(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	b := binding{CIDR: "192.0.2.5/32"}
	writes := 0
	state := map[string][]byte{}
	if fakeReceiver(state, &writes).bindingsSelected(context.Background(), b) == nil {
		t.Fatal("absent bindings passed as selected")
	}
	state[preparedBindings[0].Resource] = registryFixture(t, preparedBindings[0], b)
	if fakeReceiver(state, &writes).bindingsSelected(context.Background(), b) == nil {
		t.Fatal("one binding passed as selected")
	}
	state[preparedBindings[1].Resource] = registryFixture(t, preparedBindings[1], b)
	if err := fakeReceiver(state, &writes).bindingsSelected(context.Background(), b); err != nil {
		t.Fatalf("the reviewed bindings were refused: %v", err)
	}
	var foreign map[string]any
	_ = json.Unmarshal(state[preparedBindings[1].Resource], &foreign)
	foreign["metadata"].(map[string]any)["annotations"].(map[string]any)[receiverOwner] = "foreign"
	state[preparedBindings[1].Resource], _ = json.Marshal(foreign)
	if fakeReceiver(state, &writes).bindingsSelected(context.Background(), b) == nil {
		t.Fatal("a foreign binding passed as selected")
	}
	if writes != 0 {
		t.Fatal("the selected check wrote to the cluster")
	}
}

func TestReceiverStateFollowsTheSourceSelection(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	m, b := disabledFixture()
	fenced, _ := json.Marshal(m)
	resources := m["spec"].(map[string]any)["resources"].([]any)
	for _, item := range resources[1].(map[string]any)["filterChains"].([]any) {
		delete(item.(map[string]any)["filterChainMatch"].(map[string]any), "directSourcePrefixRanges")
	}
	delete(m["metadata"].(map[string]any)["annotations"].(map[string]any), receiverOwner)
	disabled, _ := json.Marshal(m)
	bound := map[string][]byte{}
	for _, r := range preparedBindings {
		bound[r.Resource] = registryFixture(t, r, b)
	}
	for name, tc := range map[string]struct {
		selected, bindings bool
		config             []byte
		fails              bool
	}{
		"disabled baseline":              {config: disabled},
		"fenced and selected":            {selected: true, bindings: true, config: fenced},
		"selected but not applied":       {selected: true, bindings: true, config: disabled, fails: true},
		"selected without bindings":      {selected: true, config: fenced, fails: true},
		"unselected with a live fence":   {config: fenced, fails: true},
		"unselected with stale bindings": {bindings: true, config: disabled, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			state := map[string][]byte{"ciliumenvoyconfig": tc.config}
			if tc.bindings {
				for k, v := range bound {
					state[k] = v
				}
			}
			writes := 0
			err := fakeReceiver(state, &writes).receiverState(context.Background(), b, tc.selected)
			if (err != nil) != tc.fails || writes != 0 {
				t.Fatalf("err=%v writes=%d", err, writes)
			}
		})
	}
}

func TestRestoreBaselineNeverRemovesASelectedFence(t *testing.T) {
	policies := []string{mutatingPolicy, validatingPolicy}
	unselected := renderedResources(policies...)
	for name, tc := range map[string]struct {
		rendered []byte
		restored bool
		selected bool
		fails    bool
	}{
		"selected":   {rendered: renderedResources(append(policies, mutatingBinding, validBinding)...), selected: true},
		"unselected": {rendered: unselected, restored: true},
		"unreadable": {rendered: nil, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			restored := false
			got, err := restoreBaseline(tc.rendered, func() error { restored = true; return nil })
			if (err != nil) != tc.fails || got != tc.selected || restored != tc.restored {
				t.Fatalf("selected=%v restored=%v err=%v", got, restored, err)
			}
		})
	}
	failure := errors.New("restore")
	if _, err := restoreBaseline(unselected, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal("a failed restore was not reported")
	}
}
