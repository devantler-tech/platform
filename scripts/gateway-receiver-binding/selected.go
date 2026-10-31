package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const fenceComponent = "components/physical-peer-fence"

// fenceSelected reads the reviewed source the deployment checked out. An
// unreadable or unexpected selection is an error, never "unselected", because
// that answer would remove a selected fence.
func fenceSelected(dir string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		return false, errors.New("fence-selection")
	}
	var source struct{ Components []string }
	if yaml.Unmarshal(raw, &source) != nil {
		return false, errors.New("fence-selection")
	}
	switch {
	case len(source.Components) == 0:
		return false, nil
	case len(source.Components) == 1 && source.Components[0] == fenceComponent:
		return true, nil
	}
	return false, errors.New("fence-selection")
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
func restoreBaseline(dir string, restore func() error) (bool, error) {
	selected, err := fenceSelected(dir)
	if err != nil {
		return false, err
	}
	if selected {
		return true, nil
	}
	return false, restore()
}
