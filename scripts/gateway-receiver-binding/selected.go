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
	identities, err := c.fenceIdentities(ctx, b)
	if err != nil {
		return err
	}
	if c.expected != nil && c.expected.Bindings != identities {
		return errors.New("receiver-observation-changed")
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
	if err := c.receiverObserved(raw, b); err != nil {
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
func restoreBaseline(profile transportProfile, restore func() error) (bool, error) {
	if profile.Proxy && !profile.Fence {
		return false, errors.New("transport-profile")
	}
	if profile.Fence {
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
	if c.expected != nil && !sameReceiver(c.expected.Binding, b) {
		return false, errors.New("receiver-observation-changed")
	}
	wrote := false
	var last error
	for {
		raw, err := c.read(ctx, "ciliumenvoyconfig", "kube-system", "cilium-gateway-platform")
		if c.expected != nil && err != nil {
			return wrote, err
		}
		if err == nil {
			if err := c.receiverObserved(raw, b); err != nil {
				return wrote, err
			}
			if _, err := recoveryPatch(raw, b); err != nil {
				return wrote, err
			}
			last = c.policies(ctx, b)
			if c.expected != nil && last != nil {
				return wrote, last
			}
			if last == nil {
				last = c.bindingsSelected(ctx, b)
				if c.expected != nil && last != nil {
					return wrote, last
				}
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
