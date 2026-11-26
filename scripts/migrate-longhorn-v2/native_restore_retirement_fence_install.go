package main

import (
	"context"
	"net/http"
	"net/url"
)

const reclaimPolicyKind = "validatingadmissionpolicies.admissionregistration.k8s.io"
const reclaimBindingKind = "validatingadmissionpolicybindings.admissionregistration.k8s.io"

func (r *nativeRestoreReclaim) installReclaimFence(ctx context.Context, desired object) error {
	name := text(desired, "metadata", "name")
	for _, kind := range []string{reclaimPolicyKind, reclaimBindingKind} {
		existing, err := r.c.get(ctx, kind, "", name, true)
		if err != nil || existing != nil {
			return errNativeRestoreEvidence
		}
	}
	if err := r.advance(ctx, "create-policy/"+name, "", benchmarkFenceReceipt{}); err != nil {
		return err
	}
	policy, err := r.c.create(ctx, r.progress.Source, desired)
	receipt := benchmarkFenceReceipt{PolicyUID: text(policy, "metadata", "uid")}
	if err != nil || !matchesOwnedBenchmarkPolicy(policy, desired, receipt.PolicyUID) {
		return errNativeRestoreEvidence
	}
	if err := r.advance(ctx, "create-binding/"+name, name, receipt); err != nil {
		return err
	}
	binding, err := r.c.create(ctx, r.progress.Source, benchmarkDenyBinding(name, receipt.PolicyUID))
	receipt.BindingUID = text(binding, "metadata", "uid")
	if err != nil || !matchesOwnedBenchmarkFence(policy, binding, desired, receipt) {
		return errNativeRestoreEvidence
	}
	if err := r.advance(ctx, "bound/"+name, name, receipt); err != nil {
		return err
	}
	_, err = observeBenchmarkReferenceFence(ctx, r.c, desired, receipt)
	return err
}

func (r nativeRestoreReclaim) proveFences(ctx context.Context, policies []object, detached nativeRestoreDetachedObservation) error {
	state := r.inputs.state
	for i, policy := range policies {
		receipt := r.progress.Fences[text(policy, "metadata", "name")]
		probe := func(ctx context.Context, direct *http.Client, endpoint *url.URL) error {
			if i == 0 {
				return probeNativeRestoreReclaimPV(ctx, direct, endpoint, state, detached.PV, policy)
			}
			return probeNativeRestoreReclaimClaim(ctx, direct, endpoint, state, detached.Claim, policy)
		}
		if err := proveReferenceFenceOnServingAPIs(ctx, r.c, r.progress.Source, state.Run, policy, receipt, probe); err != nil {
			return err
		}
	}
	return nil
}

func (r nativeRestoreReclaim) rebindFences(ctx context.Context, policies []object) error {
	for _, policy := range policies {
		if _, err := observeBenchmarkReferenceFence(ctx, r.c, policy, r.progress.Fences[text(policy, "metadata", "name")]); err != nil {
			return err
		}
	}
	return nil
}
