package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	openBaoCopyWaiting  = "OPENBAO_OFFLINE_COPY=WAITING_FOR_ISOLATION"
	openBaoCopyVerified = "OPENBAO_OFFLINE_COPY=VERIFIED data=1 audit=1"
)

func (j openBaoPairJournal) waitAndReleaseCopyGate(ctx context.Context, copyPod, policy, gate object,
	token string, qualified []cnpgRecoveryDiskBinding) (recoveryCiliumDatapath, error) {
	var proof recoveryCiliumDatapath
	r := j.record
	if j.client.context != "admin@prod" || r.Phase != openBaoPairStaged || !r.valid() ||
		r.CopyPending != "" || r.CopyAssets[2].UID == "" || benchmarkIdentity(policy) != r.CopyAssets[0] ||
		benchmarkIdentity(gate) != r.CopyAssets[1] || benchmarkIdentity(copyPod) != r.CopyAssets[2] {
		return proof, errOpenBaoProviderStorage
	}
	plan := r.Plan
	desired := openBaoCopyPod(plan.Run, plan.Source, plan.Input.Member, plan.Original.Node)
	name := text(desired, "metadata", "name")
	err := waitReadyWithin(ctx, 3*time.Minute, func(ctx context.Context) error {
		pod, err := j.client.get(ctx, "pod", openBaoNamespace, name, false)
		if err != nil {
			return err
		}
		if !j.client.matchBoundOpenBaoVolumePod(ctx, pod, copyPod, desired, plan.Original.Node) {
			return errors.Join(errReadinessTerminal, errOpenBaoProviderStorage)
		}
		if text(pod, "spec", "nodeName") == "" {
			return errOpenBaoProviderStorage
		}
		raw, err := j.client.kubectl(ctx, nil, "logs", name, "--namespace="+openBaoNamespace, "--container=copy")
		if err != nil || text(pod, "status", "phase") != "Pending" ||
			text(openBaoCopyInitStatus(pod), "state", "running", "startedAt") == "" ||
			strings.TrimSpace(string(raw)) != openBaoCopyWaiting {
			return errOpenBaoProviderStorage
		}
		if err := j.verifyCopyInputs(ctx, pod, token, qualified); err != nil {
			return err
		}
		observed, err := openBaoCopyIsolation.observe(ctx, j.client, pod, policy, plan.Run, plan.Original.Node)
		if err != nil {
			return err
		}
		fresh, err := j.client.get(ctx, "pod", openBaoNamespace, name, false)
		if err != nil || !j.client.matchBoundOpenBaoVolumePod(ctx, fresh, pod, desired, plan.Original.Node) ||
			text(fresh, "status", "podIP") != text(pod, "status", "podIP") || text(fresh, "status", "phase") != "Pending" ||
			text(fresh, "spec", "nodeName") != plan.Original.Node || text(openBaoCopyInitStatus(fresh), "state", "running", "startedAt") == "" {
			return errors.Join(errReadinessTerminal, errOpenBaoProviderStorage)
		}
		if err := j.releaseCopyGate(ctx, gate); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		proof = observed
		return nil
	})
	return proof, err
}

func (j openBaoPairJournal) releaseCopyGate(ctx context.Context, gate object) error {
	if j.client.context != "admin@prod" || j.record.Phase != openBaoPairStaged ||
		benchmarkIdentity(gate) != j.record.CopyAssets[1] || j.record.CopyAssets[1].UID == "" {
		return errOpenBaoProviderStorage
	}
	plan := j.record.Plan
	if _, err := j.read(ctx); err != nil {
		return err
	}
	name := openBaoCopyGatePrefix + plan.Run
	bound, err := j.client.get(ctx, "configmap", openBaoNamespace, name, false)
	if err != nil || !matchOpenBaoCopyGate(bound, gate, plan, "hold") {
		return errOpenBaoProviderStorage
	}
	if err := sourceCurrent(ctx, j.client.run, plan.Source); err != nil {
		return err
	}
	state := plan.Run + ":" + plan.Source
	if err := j.client.patch(ctx, "configmap", openBaoNamespace, bound,
		object{"op": "test", "path": "/data/state", "value": "hold"},
		object{"op": "replace", "path": "/data/state", "value": state}); err != nil {
		return err
	}
	released, err := j.client.get(ctx, "configmap", openBaoNamespace, name, false)
	if err != nil || !matchOpenBaoCopyGate(released, bound, plan, state) {
		return errOpenBaoProviderStorage
	}
	return nil
}

func (j *openBaoPairJournal) recordVerifiedCopy(ctx context.Context, copyPod, policy object, before recoveryCiliumDatapath,
	token string, qualified []cnpgRecoveryDiskBinding) error {
	r := j.record
	if j.client.context != "admin@prod" || r.Phase != openBaoPairStaged || !r.valid() ||
		r.CopyPending != "" || r.CopyAssets[2].UID == "" || before.PolicyUID != r.CopyAssets[0].UID ||
		benchmarkIdentity(policy) != r.CopyAssets[0] || benchmarkIdentity(copyPod) != r.CopyAssets[2] {
		return errOpenBaoProviderStorage
	}
	plan := r.Plan
	desired := openBaoCopyPod(plan.Run, plan.Source, plan.Input.Member, plan.Original.Node)
	name := text(desired, "metadata", "name")
	if err := waitReadyWithin(ctx, 12*time.Minute, func(ctx context.Context) error {
		pod, err := j.client.get(ctx, "pod", openBaoNamespace, name, false)
		if err != nil {
			return err
		}
		if !j.client.matchBoundOpenBaoVolumePod(ctx, pod, copyPod, desired, plan.Original.Node) ||
			text(pod, "spec", "nodeName") != plan.Original.Node || text(pod, "status", "phase") == "Failed" {
			return errors.Join(errReadinessTerminal, errOpenBaoProviderStorage)
		}
		if text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) ||
			value(openBaoCopyInitStatus(pod), "state", "terminated", "exitCode") != float64(0) {
			return errOpenBaoProviderStorage
		}
		log, err := j.client.kubectl(ctx, nil, "logs", name, "--namespace="+openBaoNamespace, "--container=copy")
		if err != nil || strings.TrimSpace(string(log)) != openBaoCopyWaiting+"\n"+openBaoCopyVerified {
			return errOpenBaoProviderStorage
		}
		after, err := openBaoCopyIsolation.observe(ctx, j.client, pod, policy, plan.Run, plan.Original.Node)
		if err != nil || !sameStorageCopyIsolation(before, after) {
			return errors.Join(errReadinessTerminal, errOpenBaoProviderStorage)
		}
		return j.verifyCopyInputs(ctx, pod, token, qualified)
	}); err != nil {
		return err
	}
	r.Phase, r.CopyPod = openBaoPairCopied, benchmarkIdentity(copyPod)
	return j.advance(ctx, r)
}

func matchOpenBaoCopyGate(observed, gate object, plan openBaoPairPlan, state string) bool {
	return recoveredNativeObject(observed, "v1", "ConfigMap", openBaoNamespace) &&
		benchmarkIdentity(observed) == benchmarkIdentity(gate) &&
		text(observed, "metadata", "name") == openBaoCopyGatePrefix+plan.Run &&
		text(observed, "metadata", "annotations", openBaoCopyFenceOwner) == plan.Run+":"+plan.Source &&
		text(observed, "data", "state") == state
}

func openBaoCopyInitStatus(pod object) object {
	statuses, _ := value(pod, "status", "initContainerStatuses").([]any)
	if len(statuses) != 1 {
		return nil
	}
	status, _ := statuses[0].(object)
	if text(status, "name") != "copy" || text(status, "imageID") != openBaoCopyImage || value(status, "restartCount") != float64(0) {
		return nil
	}
	return status
}

func (j openBaoPairJournal) verifyCopyInputs(ctx context.Context, copyPod object, token string, qualified []cnpgRecoveryDiskBinding) error {
	r := j.record
	storage, err := j.readCopyStorage(ctx, token, 2, false)
	if err != nil {
		return err
	}
	if err := verifyOpenBaoCopySources(r.Plan, copyPod, storage.node, storage.pods,
		storage.claims, storage.pvs, storage.tickets, storage.provider[:]); err != nil {
		return err
	}
	for _, destination := range r.Destinations {
		claim := uniqueOpenBaoObject(storage.claims, destination.StagingClaim.Name)
		if benchmarkIdentity(claim) != destination.StagingClaim {
			return errOpenBaoProviderStorage
		}
		size := strconv.FormatUint(destination.Source.Bytes>>30, 10) + "Gi"
		pv, volume, err := readQualifiedV2Attachment(ctx, j.client, claim, r.Plan.Original.Node, openBaoNamespace, size,
			strconv.FormatUint(destination.Source.Bytes, 10), qualified, errOpenBaoProviderStorage)
		if err != nil || pv != destination.DestinationPV || volume != destination.DestinationVolume {
			return errOpenBaoProviderStorage
		}
	}
	return nil
}

type openBaoCopyStorage struct {
	stateful, node             object
	pods, claims, pvs, tickets []object
	provider                   [2]object
}

func (j openBaoPairJournal) readCopyStorage(ctx context.Context, token string, healthy int, allowMemberActive bool) (openBaoCopyStorage, error) {
	r := j.record
	c := j.client
	var storage openBaoCopyStorage
	observation, err := readOpenBaoPairObservation(ctx, c)
	if err != nil || !openBaoDisruptionBudget(observation, healthy) {
		return storage, errOpenBaoProviderStorage
	}
	peers, err := bindOpenBaoSurvivors(r.Plan.Original, observation.stateful, observation.pods, observation.nodes)
	if err != nil || peers != r.Plan.Survivors || !openBaoExpectedActiveRoute(observation, r.Plan, allowMemberActive) {
		return storage, errOpenBaoProviderStorage
	}
	storage.stateful, storage.pods = observation.stateful, observation.pods
	storage.node = uniqueOpenBaoObject(observation.nodes, r.Plan.Original.Node)
	storage.claims, err = c.list(ctx, "persistentvolumeclaims", openBaoNamespace)
	if err != nil {
		return storage, err
	}
	storage.pvs, err = c.list(ctx, "persistentvolumes", "")
	if err != nil {
		return storage, err
	}
	storage.tickets, err = c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return storage, err
	}
	for i, disk := range r.Plan.Original.Disks {
		storage.provider[i], err = readOpenBaoProviderVolume(ctx, nil, token, disk.ProviderHandle)
		if err != nil {
			return storage, err
		}
	}
	return storage, nil
}

func openBaoExpectedActiveRoute(current openBaoPairObservation, plan openBaoPairPlan, allowMemberActive bool) bool {
	if openBaoActiveRoute(current.service, current.routes, current.pods, plan.Survivors) {
		return true
	}
	if !allowMemberActive {
		return false
	}
	pod := uniqueOpenBaoObject(current.pods, plan.Input.Member)
	node := uniqueOpenBaoObject(current.nodes, text(pod, "spec", "nodeName"))
	if !openBaoOriginalStorageMember(pod, current.stateful) || !recoveredNativeObject(node, "v1", "Node", "") ||
		!ready(valueObject(node, "status")) || value(node, "spec", "unschedulable") == true {
		return false
	}
	return openBaoActiveRoute(current.service, current.routes, current.pods,
		[2]openBaoSurvivingMember{{Pod: benchmarkIdentity(pod), Node: benchmarkIdentity(node)}, plan.Survivors[0]})
}
