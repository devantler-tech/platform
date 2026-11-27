package main

import (
	"context"
	"fmt"
	"reflect"
)

type keeperAssetCreation struct {
	kind string
	body object
}

func isKeeperAssetCreation(phase string) bool {
	switch phase {
	case "staging-claim-created", "staging-pod-created", "member-fenced-policy", "member-fenced",
		"claim-fenced-policy", "claim-fenced", "copy-isolation-created", "copy-gate-created", "copy-pod-created":
		return true
	default:
		return false
	}
}

func keeperAssetFor(r corootKeeperCutoverRecord) (keeperAssetCreation, error) {
	if r.Input.Ordinal < 0 || r.Input.Ordinal >= len(r.Before.Members) || !validBenchmarkOwner(r.Run, r.Source) {
		return keeperAssetCreation{}, errCorootKeeperReceipt
	}
	member := r.Before.Members[r.Input.Ordinal]
	switch r.Creating {
	case "staging-claim-created":
		return keeperAssetCreation{"persistentvolumeclaim", corootKeeperStagingClaim(r.Run, r.Source)}, nil
	case "staging-pod-created":
		return keeperAssetCreation{"pod", corootKeeperStagePod(r.Run, r.Source, member.Node)}, nil
	case "copy-isolation-created":
		return keeperAssetCreation{"ciliumnetworkpolicy.cilium.io", corootKeeperCopyNetworkPolicy(r.Run, r.Source)}, nil
	case "copy-gate-created":
		return keeperAssetCreation{"configmap", corootKeeperGate(r, "hold")}, nil
	case "copy-pod-created":
		return keeperAssetCreation{"pod", corootKeeperCopyPod(r.Run, r.Source, r.Rebind.Original.Claim.Name, member.Node)}, nil
	}
	fences := keeperFencesFor(r)
	switch r.Creating {
	case "member-fenced-policy":
		return keeperAssetCreation{keeperPolicyKind, fences.pod}, nil
	case "claim-fenced-policy":
		return keeperAssetCreation{keeperPolicyKind, fences.claim}, nil
	case "member-fenced", "claim-fenced":
		policy, receipt := fences.pod, r.PodFence
		if r.Creating == "claim-fenced" {
			policy, receipt = fences.claim, r.ClaimFence
		}
		if !isolationUID.MatchString(receipt.PolicyUID) || receipt.BindingUID != "" {
			return keeperAssetCreation{}, errCorootKeeperReceipt
		}
		binding := benchmarkDenyBinding(text(policy, "metadata", "name"), receipt.PolicyUID)
		valueObject(binding, "metadata")["annotations"] = value(policy, "metadata", "annotations")
		return keeperAssetCreation{keeperBindingKind, binding}, nil
	default:
		return keeperAssetCreation{}, errCorootKeeperReceipt
	}
}

func (j *corootKeeperJournal) createKeeperAsset(ctx context.Context, phase string) (object, error) {
	next := j.record
	next.Creating = phase
	asset, err := keeperAssetFor(next)
	if err != nil {
		return nil, err
	}
	if err := j.advance(ctx, next); err != nil {
		return nil, err
	}
	created, err := j.c.create(ctx, j.source(), asset.body)
	if err != nil {
		return nil, err
	}
	if err := j.recordKeeperAsset(ctx, asset, created); err != nil {
		return nil, err
	}
	return created, nil
}

// Reconcile the one outstanding API request before rollback opens either fence.
// Only a subsequent protected run has proof the prior writer has stopped.
func (j *corootKeeperJournal) reconcileKeeperCreation(ctx context.Context, priorRunFinished bool) error {
	if j.record.Creating == "" {
		return nil
	}
	asset, err := keeperAssetFor(j.record)
	if err != nil {
		return err
	}
	current, err := j.c.get(ctx, asset.kind, text(asset.body, "metadata", "namespace"), text(asset.body, "metadata", "name"), true)
	if err != nil {
		return err
	}
	if current != nil {
		return j.recordKeeperAsset(ctx, asset, current)
	}
	if !priorRunFinished {
		return fmt.Errorf("unresolved Keeper asset creation in phase %s: %w", j.record.Creating, errCorootKeeperReceipt)
	}
	next := j.record
	next.Creating = ""
	return j.advance(ctx, next)
}

func (j *corootKeeperJournal) recordKeeperAsset(ctx context.Context, asset keeperAssetCreation, current object) error {
	if asset.kind == "pod" {
		var err error
		host := j.record.Before.Members[j.record.Input.Ordinal].Node
		asset.body, err = j.c.expectedCopyPodTopology(ctx, current, asset.body, host)
		if err != nil {
			return err
		}
	}
	next, err := keeperAssetReceipt(j.record, asset, current)
	if err != nil {
		return err
	}
	return j.advance(ctx, next)
}

func keeperAssetReceipt(r corootKeeperCutoverRecord, asset keeperAssetCreation, current object) (corootKeeperCutoverRecord, error) {
	expected := asset.body
	id := benchmarkIdentity(current)
	if !recoveredNativeObject(current, text(expected, "apiVersion"), text(expected, "kind"), text(expected, "metadata", "namespace")) ||
		id.Name != text(expected, "metadata", "name") ||
		!reflect.DeepEqual(value(current, "metadata", "annotations", corootKeeperFenceOwner),
			value(expected, "metadata", "annotations", corootKeeperFenceOwner)) ||
		!reflect.DeepEqual(value(current, "metadata", "ownerReferences"), value(expected, "metadata", "ownerReferences")) {
		return r, errCorootKeeperReceipt
	}
	switch r.Creating {
	case "staging-claim-created":
		if !matchCorootKeeperStageClaim(current, expected) {
			return r, errCorootKeeperStaging
		}
		r.StagedClaim = id
	case "staging-pod-created", "copy-pod-created":
		if !matchCorootKeeperVolumePod(current, current, expected) {
			return r, errCorootKeeperReceipt
		}
		if r.Creating == "staging-pod-created" {
			r.StagePod = id
		} else {
			r.CopyPod = id
		}
	case "copy-gate-created":
		if !verifyCorootKeeperGate(current, r, "hold") {
			return r, errCorootKeeperCopy
		}
		r.CopyGate = id
	case "copy-isolation-created", "member-fenced-policy", "claim-fenced-policy", "member-fenced", "claim-fenced":
		if !reflect.DeepEqual(value(current, "spec"), value(expected, "spec")) {
			return r, errCorootKeeperReceipt
		}
		switch r.Creating {
		case "copy-isolation-created":
			r.CopyIsolation = id
		case "member-fenced-policy":
			r.PodFence.PolicyUID = id.UID
		case "claim-fenced-policy":
			r.ClaimFence.PolicyUID = id.UID
		case "member-fenced":
			if id.UID == r.PodFence.PolicyUID {
				return r, errCorootKeeperReceipt
			}
			r.PodFence.BindingUID = id.UID
		case "claim-fenced":
			if id.UID == r.ClaimFence.PolicyUID {
				return r, errCorootKeeperReceipt
			}
			r.ClaimFence.BindingUID = id.UID
		}
	default:
		return r, errCorootKeeperReceipt
	}
	r.Phase, r.Creating = r.Creating, ""
	return r, nil
}
