package main

import (
	"context"
	"errors"
)

type admissionIdentity struct{ UID, Version string }
type receiverObservation struct {
	Binding     binding
	Provider    providerIdentity
	ReceiverUID string
	Bindings    [2]admissionIdentity
}

func (c receiverClient) awaitDisabledTransport(ctx context.Context, observe func(context.Context, binding, bool) (providerIdentity, error)) (receiverObservation, error) {
	bad := func(stage string) (receiverObservation, error) { return receiverObservation{}, errors.New(stage) }
	if c.context != "admin@prod" || c.command == nil || observe == nil {
		return bad("transport-capability")
	}
	var pinned receiverObservation
	seen := false
	for {
		if ctx.Err() != nil {
			return bad("transport-transition-unverified")
		}
		service, err := c.read(ctx, "service", "kube-system", "cilium-gateway-platform")
		if err != nil {
			return receiverObservation{}, err
		}
		gateway, err := c.read(ctx, "gateway", "kube-system", "platform")
		if err != nil {
			return receiverObservation{}, err
		}
		b, intent, err := deriveObserved(service, gateway)
		if err != nil {
			return receiverObservation{}, err
		}
		if seen && !sameReceiver(pinned.Binding, b) {
			return bad("source-changed")
		}
		secret, err := c.read(ctx, "secret", "flux-system", "variables-gateway-receiver")
		if err != nil || !secretMatches(secret, b) {
			return bad("source-secret")
		}
		if err = c.policies(ctx, b); err != nil {
			return receiverObservation{}, err
		}
		bindings, err := c.fenceIdentities(ctx, b)
		if err != nil {
			return receiverObservation{}, err
		}
		raw, err := c.read(ctx, "ciliumenvoyconfig", "kube-system", "cilium-gateway-platform")
		if err != nil {
			return receiverObservation{}, err
		}
		state, err := convergenceReceiver(raw, b)
		if err != nil {
			return receiverObservation{}, err
		}
		provider, providerErr := observe(ctx, b, false)
		pending := errors.Is(providerErr, errProviderMode)
		if providerErr != nil && !pending {
			return receiverObservation{}, providerErr
		}
		if provider.ID <= 0 || provider.NetworkID <= 0 {
			return bad("provider-state")
		}
		if seen && (pinned.Provider != provider || pinned.ReceiverUID != state.UID || pinned.Bindings != bindings) {
			return bad("transport-identity-changed")
		}
		pinned = receiverObservation{Binding: b, Provider: provider, ReceiverUID: state.UID, Bindings: bindings}
		seen = true
		if !intent.Proxy && !intent.StatusProxy && !pending && state.Mode != receiverFencedActive {
			return pinned, nil
		}
		if c.wait(ctx, fencePollEvery) != nil {
			return bad("transport-transition-unverified")
		}
	}
}

func (c receiverClient) fenceIdentities(ctx context.Context, b binding) ([2]admissionIdentity, error) {
	var observed [2]admissionIdentity
	if len(preparedBindings) != len(observed) {
		return observed, errors.New("selected-policy-binding")
	}
	for i, r := range preparedBindings {
		raw, err := c.read(ctx, r.Resource, "", "gateway-receiver")
		if err != nil || !admissionMatches(raw, r, b) {
			return observed, errors.New("selected-policy-binding")
		}
		var object admissionObject
		if strictJSON(raw, &object) != nil {
			return observed, errors.New("selected-policy-binding")
		}
		observed[i] = admissionIdentity{UID: object.Metadata.UID, Version: object.Metadata.ResourceVersion}
	}
	return observed, nil
}

// Keep the existing disabled-only stale-peer repair. An active listener is
// recognized only with its complete current fence and the exact known filters.
func convergenceReceiver(raw []byte, b binding) (receiverState, error) {
	state, err := inspectReceiver(raw, b)
	if err == nil {
		return state, nil
	}
	if _, err = recoveryPatch(raw, b); err != nil {
		return receiverState{}, errors.New("receiver-state-unverified")
	}
	var object receiverConfig
	if strictJSON(raw, &object) != nil {
		return receiverState{}, errors.New("receiver-state-unverified")
	}
	return receiverState{Mode: receiverDisabled, UID: object.Metadata.UID, Version: object.Metadata.ResourceVersion}, nil
}
