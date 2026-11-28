package main

import "context"

func (j openBaoPairJournal) removeAdmissionFence(ctx context.Context, index int) error {
	if index < 0 || index > 1 {
		return errOpenBaoProviderStorage
	}
	r := j.record
	policies, err := r.admissionPolicies()
	if err != nil || !r.valid() || r.FencePending != "" || r.FenceAssets[index*2].UID == "" {
		return errOpenBaoProviderStorage
	}
	desired := policies[index]
	name := text(desired, "metadata", "name")
	owner := text(desired, "metadata", "annotations", openBaoCopyFenceOwner)
	policy, err := j.client.get(ctx, keeperPolicyKind, "", name, true)
	if err != nil {
		return err
	}
	binding, err := j.client.get(ctx, keeperBindingKind, "", name, true)
	if err != nil {
		return err
	}
	if policy == nil && binding == nil {
		return nil
	}
	policyID, bindingID := r.FenceAssets[index*2], r.FenceAssets[index*2+1]
	if benchmarkIdentity(policy) != policyID || !matchesOwnedBenchmarkPolicy(policy, desired, policyID.UID) ||
		text(policy, "metadata", "annotations", openBaoCopyFenceOwner) != owner {
		return errOpenBaoProviderStorage
	}
	if binding != nil {
		if benchmarkIdentity(binding) != bindingID ||
			text(binding, "metadata", "annotations", openBaoCopyFenceOwner) != owner ||
			!matchesOwnedBenchmarkFence(policy, binding, desired, benchmarkFenceReceipt{PolicyUID: policyID.UID, BindingUID: bindingID.UID}) {
			return errOpenBaoProviderStorage
		}
		if err := removeScratch(ctx, j.client, r.Plan.Source, keeperBindingKind, "",
			keeperPolicyAPI+"validatingadmissionpolicybindings/"+name, binding); err != nil {
			return err
		}
	}
	return removeScratch(ctx, j.client, r.Plan.Source, keeperPolicyKind, "", keeperPolicyAPI+"validatingadmissionpolicies/"+name, policy)
}

func (j openBaoPairJournal) removeCopyIsolation(ctx context.Context) error {
	r := j.record
	plan := r.Plan
	pod, err := j.client.get(ctx, "pod", openBaoNamespace, r.CopyPod.Name, true)
	if err != nil || pod != nil {
		return errOpenBaoProviderStorage
	}
	gate, err := j.client.get(ctx, "configmap", openBaoNamespace, r.CopyAssets[1].Name, true)
	if err != nil {
		return err
	}
	if gate != nil {
		if benchmarkIdentity(gate) != r.CopyAssets[1] || !matchOpenBaoCopyGate(gate, gate, plan, plan.Run+":"+plan.Source) {
			return errOpenBaoProviderStorage
		}
		if err := removeScratch(ctx, j.client, plan.Source, "configmap", openBaoNamespace,
			"/api/v1/namespaces/"+openBaoNamespace+"/configmaps/"+r.CopyAssets[1].Name, gate); err != nil {
			return err
		}
	}
	const kind = "ciliumnetworkpolicies.cilium.io"
	policy, err := j.client.get(ctx, kind, openBaoNamespace, r.CopyAssets[0].Name, true)
	if err != nil || policy == nil {
		return err
	}
	desired := openBaoCopyIsolation.denyPolicy(plan.Run, plan.Source, openBaoCopyFenceOwner)
	if benchmarkIdentity(policy) != r.CopyAssets[0] || !matchOpenBaoNewCopyAsset(0, policy, desired, plan) {
		return errOpenBaoProviderStorage
	}
	return removeScratch(ctx, j.client, plan.Source, kind, openBaoNamespace,
		"/apis/cilium.io/v2/namespaces/"+openBaoNamespace+"/ciliumnetworkpolicies/"+r.CopyAssets[0].Name, policy)
}
