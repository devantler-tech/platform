package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
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

type nativeRestoreFenceProof struct {
	roster []benchmarkAPIServer
	epochs map[string]benchmarkAdmissionEpoch
}

func (r nativeRestoreReclaim) proveFences(ctx context.Context, policies []object, detached nativeRestoreDetachedObservation) (nativeRestoreFenceProof, error) {
	roster, err := observeBenchmarkAPIServers(ctx, r.c)
	if err != nil {
		return nativeRestoreFenceProof{}, err
	}
	proof := nativeRestoreFenceProof{roster: roster, epochs: map[string]benchmarkAdmissionEpoch{}}
	state := r.inputs.state
	for i, policy := range policies {
		name := text(policy, "metadata", "name")
		receipt := r.progress.Fences[name]
		probe := func(ctx context.Context, direct *http.Client, endpoint *url.URL) error {
			if i == 0 {
				pv, err := r.rebindProbeReference(ctx, "persistentvolume", detached.PV)
				if err != nil {
					return err
				}
				return probeNativeRestoreReclaimPV(ctx, direct, endpoint, state, pv, policy)
			}
			claim, err := r.rebindProbeReference(ctx, "persistentvolumeclaim", detached.Claim)
			if err != nil {
				return err
			}
			return probeNativeRestoreReclaimClaim(ctx, direct, endpoint, state, claim, policy)
		}
		epoch, err := proveReferenceFenceForRoster(ctx, r.c, r.progress.Source, state.Run, policy, receipt, roster, probe)
		if err != nil {
			return nativeRestoreFenceProof{}, err
		}
		proof.epochs[name] = epoch
	}
	return proof, r.rebindFences(ctx, policies, proof)
}

func (r nativeRestoreReclaim) rebindFences(ctx context.Context, policies []object, proof nativeRestoreFenceProof) error {
	if len(policies) != 2 || len(proof.epochs) != len(policies) {
		return errNativeRestoreEvidence
	}
	for _, policy := range policies {
		name := text(policy, "metadata", "name")
		observed, err := observeBenchmarkReferenceFence(ctx, r.c, policy, r.progress.Fences[name])
		if err != nil || observed != proof.epochs[name] {
			return errNativeRestoreEvidence
		}
	}
	return rebindBenchmarkAPIServers(ctx, r.c, proof.roster)
}

func (r nativeRestoreReclaim) rebindProbeReference(ctx context.Context, kind string, original object) (object, error) {
	id := benchmarkIdentity(original)
	fresh, err := r.c.get(ctx, kind, id.Namespace, id.Name, false)
	if err != nil || benchmarkIdentity(fresh) != id || value(fresh, "metadata", "deletionTimestamp") != nil ||
		text(fresh, "metadata", "resourceVersion") == "" || !reflect.DeepEqual(value(fresh, "spec"), value(original, "spec")) {
		return nil, errors.New("the detached reference changed before the native admission check")
	}
	return fresh, nil
}
