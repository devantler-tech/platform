package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"

	"gopkg.in/yaml.v3"
)

type transportProfile struct{ Fence, Proxy bool }

func sourceTransportProfile(infrastructure, controllers []byte) (transportProfile, error) {
	fenced, err := fenceSelected(infrastructure)
	if err != nil {
		return transportProfile{}, err
	}
	provider, err := sourceTransportValue(infrastructure, "Gateway", "platform", "gateway.networking.k8s.io/v1")
	if err != nil {
		return transportProfile{}, err
	}
	receiver, err := sourceTransportValue(controllers, "HelmRelease", "cilium", "helm.toolkit.fluxcd.io/v2")
	if err != nil {
		return transportProfile{}, err
	}
	if provider != receiver || (provider && !fenced) {
		return transportProfile{}, errors.New("transport-profile")
	}
	return transportProfile{Fence: fenced, Proxy: provider}, nil
}

func sourceTransportValue(raw []byte, kind, name, version string) (bool, error) {
	bad := errors.New("transport-profile")
	if len(raw) == 0 || len(raw) > 16<<20 {
		return false, bad
	}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	found, value := 0, false
	for {
		var object struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string
			Metadata   struct{ Name, Namespace string }
			Spec       struct {
				Infrastructure struct{ Annotations map[string]any }
				Values         map[string]any
				Listeners      []gatewayListener
			}
		}
		if err := d.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			return false, bad
		}
		if object.Kind != kind || object.Metadata.Name != name {
			continue
		}
		found++
		if found != 1 || object.Metadata.Namespace != "kube-system" || object.APIVersion != version {
			return false, bad
		}
		if kind == "Gateway" {
			v, ok := object.Spec.Infrastructure.Annotations["load-balancer.hetzner.cloud/uses-proxyprotocol"].(string)
			if !ok || (v != "true" && v != "false") {
				return false, bad
			}
			value = v == "true"
			if value && !httpRouteBoundary(object.Spec.Listeners) {
				return false, bad
			}
		} else {
			api, ok := object.Spec.Values["gatewayAPI"].(map[string]any)
			if !ok {
				return false, bad
			}
			value, ok = api["enableProxyProtocol"].(bool)
			if !ok {
				return false, bad
			}
		}
	}
	if found != 1 {
		return false, bad
	}
	return value, nil
}

func renderTransportProfile(ctx context.Context) (transportProfile, error) {
	infrastructure, err := renderLayer(ctx)
	if err != nil {
		return transportProfile{}, err
	}
	command := exec.CommandContext(ctx, "kubectl", "kustomize", "k8s/providers/hetzner/infrastructure/controllers")
	command.Stderr = io.Discard
	controllers, err := command.Output()
	if err != nil {
		return transportProfile{}, errors.New("transport-profile")
	}
	return sourceTransportProfile(infrastructure, controllers)
}
