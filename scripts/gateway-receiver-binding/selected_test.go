package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeSelection(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFenceSelectionReadsOnlyTheExactComponent(t *testing.T) {
	base := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - a.yaml\n"
	for name, tc := range map[string]struct {
		content  string
		selected bool
		fails    bool
	}{
		"unselected": {content: base},
		"selected":   {content: base + "components:\n  - components/physical-peer-fence\n", selected: true},
		"foreign":    {content: base + "components:\n  - components/other\n", fails: true},
		"mixed":      {content: base + "components:\n  - components/physical-peer-fence\n  - components/other\n", fails: true},
		"repeated":   {content: base + "components:\n  - components/physical-peer-fence\n  - components/physical-peer-fence\n", fails: true},
		"malformed":  {content: "components: [", fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			selected, err := fenceSelected(writeSelection(t, tc.content))
			if (err != nil) != tc.fails || selected != tc.selected {
				t.Fatalf("selected=%v err=%v", selected, err)
			}
		})
	}
	if _, err := fenceSelected(t.TempDir()); err == nil {
		t.Fatal("an unreadable source read as unselected, which would remove a selected fence")
	}
}

func TestReviewedSourceStillLeavesTheFenceUnselected(t *testing.T) {
	selected, err := fenceSelected(filepath.Join("..", "..", receiverSourceDir))
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
	selected := writeSelection(t, "components:\n  - components/physical-peer-fence\n")
	unselected := writeSelection(t, "resources:\n  - a.yaml\n")
	for name, tc := range map[string]struct {
		dir      string
		restored bool
		selected bool
		fails    bool
	}{
		"selected":   {dir: selected, selected: true},
		"unselected": {dir: unselected, restored: true},
		"unreadable": {dir: t.TempDir(), fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			restored := false
			got, err := restoreBaseline(tc.dir, func() error { restored = true; return nil })
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
