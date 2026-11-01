package main

import (
	"context"
	"errors"
)

func applyTransportProfile(ctx context.Context, kubeContext string, profile transportProfile) (bool, error) {
	return (receiverClient{context: kubeContext, command: executeReceiver}).applyProfile(ctx, profile, readProvider, probePublic)
}

// Source propagation may precede the layer's Ready condition. No receiver write
// happens until its complete source and actual transport have been observed.
func (c receiverClient) applyProfile(ctx context.Context, profile transportProfile, observe func(context.Context, binding, bool) (providerIdentity, error), probe func(context.Context, string) error) (bool, error) {
	if c.context != "admin@prod" || c.command == nil || observe == nil || probe == nil || !profile.Fence {
		return false, errors.New("transport-capability")
	}
	before, _, err := c.observedBinding(ctx)
	if err != nil {
		return false, err
	}
	for {
		secret, readErr := c.read(ctx, "secret", "flux-system", "variables-gateway-receiver")
		last := errors.New("source-secret")
		if readErr == nil && secretMatches(secret, before) {
			last = c.policies(ctx, before)
			if last == nil {
				last = c.bindingsSelected(ctx, before)
			}
		}
		if last == nil {
			break
		}
		if c.wait(ctx, fencePollEvery) != nil {
			return false, last
		}
	}
	observed, err := c.awaitTransport(ctx, profile.Proxy, observe)
	if err != nil {
		return false, err
	}
	if !sameReceiver(before, observed.Binding) {
		return false, errors.New("source-changed")
	}
	before = observed.Binding
	c.expected = &observed
	boundProvider := func(ctx context.Context, b binding, want bool) (providerIdentity, error) {
		id, err := observe(ctx, b, want)
		if err == nil && id != observed.Provider {
			return providerIdentity{}, errors.New("provider-changed")
		}
		return id, err
	}
	wrote := false
	err = withProviderMode(ctx, before, profile.Proxy, boundProvider, func() error {
		if profile.Proxy {
			if err := c.receiverProfile(ctx, before, profile); err != nil {
				return err
			}
		} else {
			var err error
			wrote, err = c.installFence(ctx, before, c.wait)
			if err != nil {
				return err
			}
		}
		// Failed public requests are not retried. Fresh connections sample the
		// load balancer after a listener write or an enabled transport profile.
		for round := 0; (wrote || profile.Proxy) && round < fenceProbeRounds; round++ {
			if err := probe(ctx, before.PublicIPv4); err != nil {
				return err
			}
		}
		after, err := c.expectedBinding(ctx, profile.Proxy)
		if err != nil || !sameReceiver(before, after) {
			return errors.New("source-changed")
		}
		return c.receiverProfile(ctx, after, profile)
	})
	return wrote, err
}

func (c receiverClient) observedBinding(ctx context.Context) (binding, transportIntent, error) {
	service, err := c.read(ctx, "service", "kube-system", "cilium-gateway-platform")
	if err != nil {
		return binding{}, transportIntent{}, err
	}
	gateway, err := c.read(ctx, "gateway", "kube-system", "platform")
	if err != nil {
		return binding{}, transportIntent{}, err
	}
	return deriveObserved(service, gateway)
}

func (c receiverClient) expectedBinding(ctx context.Context, want bool) (binding, error) {
	service, err := c.read(ctx, "service", "kube-system", "cilium-gateway-platform")
	if err != nil {
		return binding{}, err
	}
	gateway, err := c.read(ctx, "gateway", "kube-system", "platform")
	if err != nil {
		return binding{}, err
	}
	return deriveExpectedTransport(service, gateway, want, false)
}
