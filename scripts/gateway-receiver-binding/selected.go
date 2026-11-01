package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// receiverLayerDir is the Flux layer that deploys the receiver in production.
const receiverLayerDir = "k8s/providers/hetzner/infrastructure"

func renderLayer(ctx context.Context) ([]byte, error) {
	command := exec.CommandContext(ctx, "kubectl", "kustomize", receiverLayerDir)
	command.Stderr = io.Discard
	out, err := command.Output()
	if err != nil {
		return nil, errors.New("fence-selection")
	}
	return out, nil
}

// fenceSelected reads what the checked-out layer renders, so selecting the
// component from any level of the overlay counts. An unreadable render, one
// without the prepared policies, or a partial or foreign binding is an error,
// never "unselected", because that answer would remove a selected fence.
func fenceSelected(rendered []byte) (bool, error) {
	bad := errors.New("fence-selection")
	seen := map[string]int{}
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var o struct {
			Kind     string
			Metadata struct{ Name string }
			Spec     struct {
				PolicyName string `yaml:"policyName"`
			}
		}
		if err := decoder.Decode(&o); err == io.EOF {
			break
		} else if err != nil {
			return false, bad
		}
		for _, r := range preparedPolicies {
			if o.Kind == r.Kind && o.Metadata.Name == "gateway-receiver" {
				seen[o.Kind]++
			}
		}
		for _, r := range preparedBindings {
			if o.Kind != r.Kind || (o.Metadata.Name != "gateway-receiver" && o.Spec.PolicyName != "gateway-receiver") {
				continue
			}
			if o.Metadata.Name != "gateway-receiver" || o.Spec.PolicyName != "gateway-receiver" {
				return false, bad
			}
			seen[o.Kind]++
		}
	}
	for _, r := range preparedPolicies {
		if seen[r.Kind] != 1 {
			return false, bad
		}
	}
	bindings := 0
	for _, r := range preparedBindings {
		if seen[r.Kind] > 1 {
			return false, bad
		}
		bindings += seen[r.Kind]
	}
	switch bindings {
	case 0:
		return false, nil
	case len(preparedBindings):
		return true, nil
	}
	return false, bad
}

// fenceActive requires the marker and the exact peer on every TLS chain. It
// reuses disabledPatch's validation: what restoration would remove is the fence.
func fenceActive(raw []byte, b binding) error {
	bad := errors.New("receiver-not-fenced")
	patch, err := disabledPatch(raw, b)
	if err != nil || len(patch) == 0 {
		return bad
	}
	var ops []struct{ Path string }
	if json.Unmarshal(patch, &ops) != nil {
		return bad
	}
	fenced, marker := 0, false
	for _, op := range ops {
		switch {
		case strings.HasSuffix(op.Path, "/directSourcePrefixRanges"):
			fenced++
		case op.Path == "/metadata/annotations/platform.devantler.tech~1gateway-receiver-owner":
			marker = true
		}
	}
	var o receiverConfig
	if strictJSON(raw, &o) != nil {
		return bad
	}
	tls := 0
	for _, r := range o.Spec.Resources {
		if r["@type"] != listenerType {
			continue
		}
		chains, _ := r["filterChains"].([]any)
		for _, item := range chains {
			chain, _ := item.(map[string]any)
			match, _ := chain["filterChainMatch"].(map[string]any)
			if match["transportProtocol"] == "tls" {
				tls++
			}
		}
	}
	if !marker || tls == 0 || fenced != tls {
		return bad
	}
	return nil
}

func (c receiverClient) bindingsSelected(ctx context.Context, b binding) error {
	for _, r := range preparedBindings {
		raw, err := c.read(ctx, r.Resource, "", "gateway-receiver")
		if err != nil || !admissionMatches(raw, r, b) {
			return errors.New("selected-policy-binding")
		}
	}
	return nil
}

// receiverState compares the live bindings and listener with what the reviewed
// source selects. It only reads.
func (c receiverClient) receiverState(ctx context.Context, b binding, selected bool) error {
	raw, err := c.read(ctx, "ciliumenvoyconfig", "kube-system", "cilium-gateway-platform")
	if err != nil {
		return err
	}
	if selected {
		if err := c.bindingsSelected(ctx, b); err != nil {
			return err
		}
		return fenceActive(raw, b)
	}
	if err := c.bindingsAbsent(ctx); err != nil {
		return err
	}
	patch, err := disabledPatch(raw, b)
	if err != nil || len(patch) != 0 {
		return errors.New("receiver-not-disabled")
	}
	return nil
}

// restoreBaseline runs restoration only when the reviewed source leaves the
// fence unselected, so main's ordinary recovery keeps answering for main.
func restoreBaseline(rendered []byte, restore func() error) (bool, error) {
	selected, err := fenceSelected(rendered)
	if err != nil {
		return false, err
	}
	if selected {
		return true, nil
	}
	return false, restore()
}

const (
	// The deployment waits for infrastructure-controllers, and the bindings
	// arrive later with the infrastructure layer.
	fenceApplyBudget = 10 * time.Minute
	fencePollEvery   = 5 * time.Second
	// Each node's Envoy receives the listener after the object is written, and
	// the public probes that follow must exercise the fenced listener.
	fenceSettle = 20 * time.Second
	// Two hosts per round, one connection each.
	fenceProbeRounds = 20
)

// sameReceiver ignores resource versions: routes landing during the wait update
// the Gateway's status, which does not change what the fence is bound to.
func sameReceiver(a, b binding) bool {
	a.ServiceVersion, a.GatewayVersion, b.ServiceVersion, b.GatewayVersion = "", "", "", ""
	return a == b
}

// fenceTouch writes only the ownership marker, conditionally. Admission applies
// only to writes and the controller does not rewrite an unchanged listener, so
// this is what lets the reviewed policy install the fence on the existing object.
func fenceTouch(raw []byte) ([]byte, error) {
	var o receiverConfig
	if strictJSON(raw, &o) != nil {
		return nil, errors.New("receiver-input")
	}
	return json.Marshal(map[string]any{"metadata": map[string]any{
		"uid":             o.Metadata.UID,
		"resourceVersion": o.Metadata.ResourceVersion,
		"annotations":     map[string]string{receiverOwner: "v1"},
	}})
}

// installFence retries until the reviewed policies, the bindings and the fence
// all read as current. Policies or bindings behind the source are the normal lag
// behind Flux, so they wait and write nothing. A fence for an earlier peer is
// rewritten by the same write. A listener this helper does not own or an active
// PROXY listener fails at once, because a write could not make either correct.
func (c receiverClient) installFence(ctx context.Context, b binding, pause func(context.Context, time.Duration) error) (bool, error) {
	wrote := false
	var last error
	for {
		raw, err := c.read(ctx, "ciliumenvoyconfig", "kube-system", "cilium-gateway-platform")
		if err == nil {
			if _, err := recoveryPatch(raw, b); err != nil {
				return wrote, err
			}
			last = c.policies(ctx, b)
			if last == nil {
				last = c.bindingsSelected(ctx, b)
			}
			if last == nil && fenceActive(raw, b) == nil {
				if wrote {
					return true, pause(ctx, fenceSettle)
				}
				return false, nil
			}
			if last == nil {
				touch, err := fenceTouch(raw)
				if err != nil {
					return wrote, err
				}
				// A conflict or an API server still catching up on the policy
				// shows up in the next read.
				if _, err := c.command(ctx, touch, "--context="+c.context, "--request-timeout=20s", "-n", "kube-system", "patch", "ciliumenvoyconfig", "cilium-gateway-platform", "--type=merge", "--patch-file=/dev/stdin", "-o=json"); err != nil {
					last = errors.New("receiver-write-refused")
				} else {
					wrote = true
					last = errors.New("receiver-not-fenced")
				}
			}
		} else {
			last = err
		}
		if pause(ctx, fencePollEvery) != nil {
			return wrote, last
		}
	}
}

func pauseFor(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return errors.New("cancelled")
	case <-time.After(d):
		return nil
	}
}

// applySelected installs the fence the reviewed source selects. It runs only in
// the protected deployment and never rebinds or edits anything but the marker.
func applySelected(ctx context.Context, kubeContext string) (bool, error) {
	if kubeContext != "admin@prod" {
		return false, errors.New("arguments")
	}
	before, err := collect(ctx, kubeContext)
	if err != nil {
		return false, err
	}
	// A replacement binding reaches the cluster through Flux, so a stale one waits too.
	for {
		secret, err := readNamespace(ctx, kubeContext, "flux-system", "secret", "variables-gateway-receiver")
		if err == nil && secretMatches(secret, before) {
			break
		}
		if pauseFor(ctx, fencePollEvery) != nil {
			return false, errors.New("source-secret")
		}
	}
	c := receiverClient{context: kubeContext, command: executeReceiver}
	wrote := false
	err = withProviderBaseline(ctx, before, readProvider, func() error {
		var installErr error
		wrote, installErr = c.installFence(ctx, before, pauseFor)
		if installErr != nil {
			return installErr
		}
		// Enough fresh connections to reach most nodes behind the load balancer, once,
		// when the listeners change. This shows the fence does not refuse the load
		// balancer; it cannot show that a listener took the fence.
		for round := 0; wrote && round < fenceProbeRounds; round++ {
			if err := probePublic(ctx, before.PublicIPv4); err != nil {
				return err
			}
		}
		after, err := collect(ctx, kubeContext)
		if err != nil || !sameReceiver(before, after) {
			return errors.New("source-changed")
		}
		return nil
	})
	return wrote, err
}
