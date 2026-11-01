package main

import (
	"context"
	"encoding/json"
	"errors"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestReviewedLayerSelectsTheFence(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	rendered, err := renderLayer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := fenceSelected(rendered)
	if err != nil || !selected {
		t.Fatalf("selected=%v err=%v: main deploys the fence, and unselecting it is a reviewed recovery", selected, err)
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

// fenceAPI stands in for the API server: reads answer from state, and a marker
// write installs the peer only while admission is live, as the bound policy does.
type fenceAPI struct {
	config        map[string]any
	policies      map[string][]byte
	bindings      map[string][]byte
	admissionLive bool
	refuseWrites  bool
	writes        [][]byte
}

func (a *fenceAPI) command(_ context.Context, input []byte, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, " patch ciliumenvoyconfig "):
		a.writes = append(a.writes, input)
		if a.refuseWrites {
			return nil, errors.New("denied")
		}
		if a.admissionLive {
			setFencePeer(a.config, "192.0.2.5", true)
		}
		return []byte("{}"), nil
	case strings.Contains(joined, " get ciliumenvoyconfig "):
		return json.Marshal(a.config)
	}
	for _, resource := range []map[string][]byte{a.policies, a.bindings} {
		for name, raw := range resource {
			if strings.Contains(joined, " get "+name+" ") {
				return raw, nil
			}
		}
	}
	return nil, nil
}

func setFencePeer(config map[string]any, peer string, marker bool) {
	meta := config["metadata"].(map[string]any)
	annotations := meta["annotations"].(map[string]any)
	delete(annotations, receiverOwner)
	if marker {
		annotations[receiverOwner] = "v1"
	}
	listener := config["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
	for _, item := range listener["filterChains"].([]any) {
		match := item.(map[string]any)["filterChainMatch"].(map[string]any)
		if match["transportProtocol"] != "tls" {
			continue
		}
		delete(match, "directSourcePrefixRanges")
		if peer != "" {
			match["directSourcePrefixRanges"] = []any{map[string]any{"addressPrefix": peer, "prefixLen": 32}}
		}
	}
}

func fenceAdmissionObject(t *testing.T, r admissionResource, b binding) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(receiverSourceDir, r.File))
	if err != nil {
		t.Fatal(err)
	}
	var o map[string]any
	if yaml.Unmarshal([]byte(strings.NewReplacer("${gateway_receiver_source_cidr}", b.CIDR, "${gateway_receiver_gateway_uid}", b.GatewayUID).Replace(string(raw))), &o) != nil {
		t.Fatal("source")
	}
	m := o["metadata"].(map[string]any)
	m["uid"], m["resourceVersion"], m["generation"] = "44444444-4444-4444-8444-444444444444", "10", 1
	o["status"] = map[string]any{"observedGeneration": 1}
	out, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newFenceAPI(t *testing.T) (*fenceAPI, binding) {
	t.Helper()
	config, b := disabledFixture()
	setFencePeer(config, "", false)
	api := &fenceAPI{config: config, policies: map[string][]byte{}, bindings: map[string][]byte{}, admissionLive: true}
	for _, r := range preparedPolicies {
		api.policies[r.Resource] = fenceAdmissionObject(t, r, b)
	}
	for _, r := range preparedBindings {
		api.bindings[r.Resource] = fenceAdmissionObject(t, r, b)
	}
	return api, b
}

func TestInstallFenceWritesOnlyTheConditionalMarker(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	api, b := newFenceAPI(t)
	var settled time.Duration
	wrote, err := receiverClient{context: "admin@prod", command: api.command}.installFence(context.Background(), b, func(_ context.Context, d time.Duration) error { settled = d; return nil })
	if err != nil || !wrote {
		t.Fatalf("wrote=%v err=%v", wrote, err)
	}
	if len(api.writes) != 1 {
		t.Fatalf("writes=%d", len(api.writes))
	}
	var touch map[string]map[string]any
	if json.Unmarshal(api.writes[0], &touch) != nil || len(touch) != 1 || len(touch["metadata"]) != 3 {
		t.Fatalf("the write reaches beyond the marker: %s", api.writes[0])
	}
	annotations, _ := touch["metadata"]["annotations"].(map[string]any)
	if len(annotations) != 1 || annotations[receiverOwner] != "v1" {
		t.Fatalf("the write does not carry only the marker: %s", api.writes[0])
	}
	if touch["metadata"]["uid"] != "22222222-2222-4222-8222-222222222222" || touch["metadata"]["resourceVersion"] != "100" {
		t.Fatalf("the write is not conditional on the observed object: %s", api.writes[0])
	}
	if settled != fenceSettle {
		t.Fatalf("last pause %v: the public probes must follow the listener", settled)
	}
	again, err := receiverClient{context: "admin@prod", command: api.command}.installFence(context.Background(), b, func(context.Context, time.Duration) error { return nil })
	if err != nil || again || len(api.writes) != 1 {
		t.Fatalf("an installed fence was written again: wrote=%v err=%v writes=%d", again, err, len(api.writes))
	}
}

func TestInstallFenceWaitsForTheFluxLayerWithoutWriting(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	api, b := newFenceAPI(t)
	held := api.bindings
	api.bindings = map[string][]byte{}
	pauses := 0
	wrote, err := receiverClient{context: "admin@prod", command: api.command}.installFence(context.Background(), b, func(_ context.Context, d time.Duration) error {
		pauses++
		if pauses <= 3 && len(api.writes) != 0 {
			t.Fatal("wrote before the bindings arrived")
		}
		if pauses == 3 {
			api.bindings = held
		}
		return nil
	})
	if err != nil || !wrote || len(api.writes) != 1 {
		t.Fatalf("wrote=%v err=%v writes=%d", wrote, err, len(api.writes))
	}
}

func TestInstallFenceRetriesAnAPIServerBehindOnThePolicy(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	api, b := newFenceAPI(t)
	api.admissionLive = false
	wrote, err := receiverClient{context: "admin@prod", command: api.command}.installFence(context.Background(), b, func(context.Context, time.Duration) error {
		if len(api.writes) == 2 {
			api.admissionLive = true
		}
		return nil
	})
	if err != nil || !wrote || len(api.writes) != 3 {
		t.Fatalf("wrote=%v err=%v writes=%d", wrote, err, len(api.writes))
	}
}

func TestInstallFenceStopsAtTheDeadlineAsUnverified(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	api, b := newFenceAPI(t)
	api.admissionLive = false
	pauses := 0
	_, err := receiverClient{context: "admin@prod", command: api.command}.installFence(context.Background(), b, func(context.Context, time.Duration) error {
		pauses++
		if pauses == 3 {
			return errors.New("cancelled")
		}
		return nil
	})
	if err == nil || err.Error() != "receiver-not-fenced" {
		t.Fatalf("a listener that never took the fence reported %v", err)
	}
}

func TestInstallFenceRefusesStateAWriteCannotCorrect(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	for name, change := range map[string]func(map[string]any){
		"wider prefix": func(c map[string]any) {
			setFencePeer(c, "192.0.2.5", true)
			listener := c["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
			match := listener["filterChains"].([]any)[1].(map[string]any)["filterChainMatch"].(map[string]any)
			match["directSourcePrefixRanges"].([]any)[0].(map[string]any)["prefixLen"] = 24
		},
		"foreign gateway": func(c map[string]any) {
			c["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "99999999-9999-4999-8999-999999999999"
		},
		"active proxy": func(c map[string]any) {
			c["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)["listenerFilters"] = []any{map[string]any{"name": "envoy.filters.listener.proxy_protocol"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			api, b := newFenceAPI(t)
			change(api.config)
			_, err := receiverClient{context: "admin@prod", command: api.command}.installFence(context.Background(), b, func(context.Context, time.Duration) error {
				t.Fatal("waited on a state no write can correct")
				return nil
			})
			if err == nil || len(api.writes) != 0 {
				t.Fatalf("err=%v writes=%d", err, len(api.writes))
			}
		})
	}
}

func TestInstallFenceRewritesAFenceBoundToAnEarlierPeer(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	api, b := newFenceAPI(t)
	setFencePeer(api.config, "192.0.2.6", true)
	wrote, err := receiverClient{context: "admin@prod", command: api.command}.installFence(context.Background(), b, func(context.Context, time.Duration) error { return nil })
	if err != nil || !wrote || len(api.writes) != 1 {
		t.Fatalf("wrote=%v err=%v writes=%d", wrote, err, len(api.writes))
	}
	raw, _ := json.Marshal(api.config)
	if fenceActive(raw, b) != nil {
		t.Fatal("the earlier peer survived")
	}
}

func TestInstallFenceNamesARefusedWrite(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	api, b := newFenceAPI(t)
	api.refuseWrites = true
	pauses := 0
	wrote, err := receiverClient{context: "admin@prod", command: api.command}.installFence(context.Background(), b, func(context.Context, time.Duration) error {
		pauses++
		if pauses == 2 {
			return errors.New("cancelled")
		}
		return nil
	})
	if wrote || err == nil || err.Error() != "receiver-write-refused" {
		t.Fatalf("wrote=%v err=%v", wrote, err)
	}
}

func TestSameReceiverIgnoresOnlyResourceVersions(t *testing.T) {
	a := binding{CIDR: "192.0.2.5/32", ServiceUID: "s", GatewayUID: "g", ServiceVersion: "1", GatewayVersion: "1", PublicIPv4: "198.51.100.1"}
	later := a
	later.ServiceVersion, later.GatewayVersion = "2", "2"
	if !sameReceiver(a, later) {
		t.Fatal("a status write read as a changed receiver")
	}
	for name, change := range map[string]func(*binding){
		"peer":    func(x *binding) { x.CIDR = "192.0.2.6/32" },
		"service": func(x *binding) { x.ServiceUID = "other" },
		"gateway": func(x *binding) { x.GatewayUID = "other" },
		"public":  func(x *binding) { x.PublicIPv4 = "198.51.100.2" },
	} {
		other := a
		change(&other)
		if sameReceiver(a, other) {
			t.Fatalf("%s change was ignored", name)
		}
	}
}

// restoreFake models an API server: a patch applies unless a binding it still
// holds reapplies the fence, for the first lag attempts.
func restoreFake(t *testing.T, peer string, lag int) (receiverClient, map[string]any, *int) {
	t.Helper()
	config, _ := disabledFixture()
	setFencePeer(config, peer, true)
	patches := 0
	return receiverClient{context: "admin@prod", pause: func(context.Context, time.Duration) error { return nil }, command: func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, " patch ciliumenvoyconfig "):
			patches++
			if patches <= lag {
				return []byte("{}"), nil
			}
			patch, err := jsonpatch.DecodePatch(input)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(config)
			out, err := patch.Apply(raw)
			if err != nil {
				return nil, err
			}
			for key := range config {
				delete(config, key)
			}
			if err := json.Unmarshal(out, &config); err != nil {
				t.Fatal(err)
			}
			return out, nil
		case strings.Contains(joined, " get ciliumenvoyconfig "):
			return json.Marshal(config)
		}
		t.Fatalf("unexpected command: %s", joined)
		return nil, nil
	}}, config, &patches
}

func TestRestorationRetriesAnAPIServerStillHoldingABinding(t *testing.T) {
	c, config, patches := restoreFake(t, "192.0.2.5", 2)
	_, b := disabledFixture()
	if err := c.restoreConfig(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(config)
	if p, err := disabledPatch(raw, b); err != nil || len(p) != 0 || *patches != 3 {
		t.Fatalf("patch=%s err=%v patches=%d", p, err, *patches)
	}
}

func TestRestorationGivesUpAsUnverified(t *testing.T) {
	c, _, patches := restoreFake(t, "192.0.2.5", restoreAttempts+1)
	_, b := disabledFixture()
	err := c.restoreConfig(context.Background(), b)
	if err == nil || err.Error() != "receiver-restoration-unverified" || *patches != restoreAttempts {
		t.Fatalf("err=%v patches=%d", err, *patches)
	}
}

func TestRestorationRemovesAFenceBoundToAnEarlierPeer(t *testing.T) {
	c, config, _ := restoreFake(t, "192.0.2.6", 0)
	_, b := disabledFixture()
	if err := c.restoreConfig(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(config)
	if p, err := disabledPatch(raw, b); err != nil || len(p) != 0 {
		t.Fatalf("patch=%s err=%v", p, err)
	}
}

func TestRecoveryRemovesOnlyTheShapeThePolicyInstalls(t *testing.T) {
	host := map[string]any{"addressPrefix": "192.0.2.6", "prefixLen": float64(32)}
	if !singleHostPeer([]any{host}) {
		t.Fatal("a single host was refused")
	}
	for name, value := range map[string]any{
		"none":       []any{},
		"two":        []any{host, host},
		"extra key":  []any{map[string]any{"addressPrefix": "192.0.2.6", "prefixLen": float64(32), "other": true}},
		"wider":      []any{map[string]any{"addressPrefix": "192.0.2.6", "prefixLen": float64(24)}},
		"ipv6":       []any{map[string]any{"addressPrefix": "2001:db8::1", "prefixLen": float64(32)}},
		"not an ip":  []any{map[string]any{"addressPrefix": "example.invalid", "prefixLen": float64(32)}},
		"not a list": host,
	} {
		if singleHostPeer(value) {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// The policies are read after the removal, because the fence may be what stops
// Flux from delivering them.
func TestRestorationRemovesTheFenceBeforeReportingStalePolicies(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	c, config, _ := restoreFake(t, "192.0.2.6", 0)
	_, b := disabledFixture()
	inner := c.command
	c.command = func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "ciliumenvoyconfig") {
			return inner(ctx, input, args...)
		}
		return nil, nil // no bindings, and the policies have not arrived
	}
	err := c.restoreReceiver(context.Background(), b)
	if !isPolicyDrift(err) {
		t.Fatalf("err=%v", err)
	}
	raw, _ := json.Marshal(config)
	if p, perr := disabledPatch(raw, b); perr != nil || len(p) != 0 {
		t.Fatalf("the fence stayed: patch=%s err=%v", p, perr)
	}
}
