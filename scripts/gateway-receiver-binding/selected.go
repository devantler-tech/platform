package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"

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
