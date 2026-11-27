package main

import (
	"context"
	"encoding/json"
)

type openBaoPairPhase string

const (
	openBaoPairPlanned          openBaoPairPhase = "planned"
	openBaoPairStaged           openBaoPairPhase = "staged"
	openBaoPairCopied           openBaoPairPhase = "copy-verified"
	openBaoPairReleasing        openBaoPairPhase = "claim-release-requested"
	openBaoPairSourcesReleased  openBaoPairPhase = "sources-released"
	openBaoPairRebound          openBaoPairPhase = "pair-rebound"
	openBaoPairResuming         openBaoPairPhase = "member-release-requested"
	openBaoPairReady            openBaoPairPhase = "replica-ready"
	openBaoPairJournalNamespace                  = "longhorn-system"
)

type openBaoPairPlan struct {
	Run, Source string
	Input       openBaoPairInput
	Original    openBaoProviderStorage
	Survivors   [2]openBaoSurvivingMember
}

type openBaoPairRecord struct {
	Plan         openBaoPairPlan
	Phase        openBaoPairPhase
	StageAssets  [3]benchmarkResource
	StagePending string
	Destinations [2]openBaoStorageRebind
	FenceUser    string
	FenceAssets  [4]benchmarkResource
	FencePending string
	Evicting     bool
	CopyAssets   [3]benchmarkResource
	CopyPending  string
	CopyPod      benchmarkResource
	Successors   [2]benchmarkResource
	ClaimPending int
	Replacement  benchmarkResource
	RolledBack   bool
}

type openBaoPairJournal struct {
	client client
	id     benchmarkResource
	record openBaoPairRecord
}

func verifyOpenBaoPairPlan(plan openBaoPairPlan, run, source string) error {
	if plan.Run != run || plan.Source != source || !validBenchmarkOwner(run, source) ||
		!knownOpenBaoMember(plan.Input.Member) || !retentionDigest.MatchString(plan.Input.Digest) ||
		!openBaoPairIdentity(plan.Original.Member, openBaoNamespace, plan.Input.Member) ||
		!openBaoPairIdentity(plan.Original.StatefulSet, openBaoNamespace, openBaoNamespace) ||
		!kubeNamespace.MatchString(plan.Original.Node) {
		return errOpenBaoProviderStorage
	}
	digest, err := openBaoProviderStorageDigest(plan.Original)
	if err != nil || digest != plan.Input.Digest {
		return errOpenBaoProviderStorage
	}
	seen := map[string]bool{plan.Input.Member: true}
	seenNodes := map[string]bool{plan.Original.Node: true}
	for _, peer := range plan.Survivors {
		if !knownOpenBaoMember(peer.Pod.Name) || !openBaoPairIdentity(peer.Pod, openBaoNamespace, peer.Pod.Name) ||
			!openBaoPairIdentity(peer.Node, "", peer.Node.Name) ||
			seen[peer.Pod.Name] || seenNodes[peer.Node.Name] || peer.Pod.UID == plan.Original.Member.UID {
			return errOpenBaoProviderStorage
		}
		seen[peer.Pod.Name], seenNodes[peer.Node.Name] = true, true
	}
	if plan.Survivors[0].Pod.UID == plan.Survivors[1].Pod.UID || plan.Survivors[0].Node.UID == plan.Survivors[1].Node.UID {
		return errOpenBaoProviderStorage
	}
	return nil
}

func openBaoPairIdentity(id benchmarkResource, namespace, name string) bool {
	return id.Namespace == namespace && id.Name == name && kubeName.MatchString(name) && len(name) <= 253 &&
		isolationUID.MatchString(id.UID)
}

func (r openBaoPairRecord) valid() bool {
	if verifyOpenBaoPairPlan(r.Plan, r.Plan.Run, r.Plan.Source) != nil {
		return false
	}
	staged, valid := r.stageProgress()
	if !valid {
		return false
	}
	copyAssets, valid := r.copyProgress()
	if !valid {
		return false
	}
	fences, valid := r.fenceProgress()
	if !valid || (r.FenceUser == "" && (fences != 0 || r.FencePending != "")) ||
		(r.FenceUser != "" && !validCorootKeeperFenceUser(r.FenceUser)) ||
		(r.Evicting && (fences != len(r.FenceAssets) || r.FencePending != "")) ||
		(!r.Evicting && (copyAssets != 0 || r.CopyPending != "")) {
		return false
	}
	if r.RolledBack && ((r.Phase != openBaoPairPlanned && r.Phase != openBaoPairStaged && r.Phase != openBaoPairCopied) ||
		r.StagePending != "" || r.FencePending != "" || r.CopyPending != "") {
		return false
	}
	if r.Phase == openBaoPairPlanned {
		return r.Destinations == [2]openBaoStorageRebind{} && r.CopyPod == (benchmarkResource{}) &&
			r.Successors == [2]benchmarkResource{} && r.ClaimPending == 0 && r.Replacement == (benchmarkResource{}) &&
			r.FenceUser == "" && !r.Evicting
	}
	if staged != len(r.StageAssets) || r.StagePending != "" || !openBaoRebindPairMatches(r.Plan.Input.Member, r.Destinations) {
		return false
	}
	for i, destination := range r.Destinations {
		if destination.Source != r.Plan.Original.Disks[i] || destination.Run != r.Plan.Run ||
			destination.SourceSHA != r.Plan.Source || destination.StagingClaim != r.StageAssets[i] ||
			!openBaoPairIdentity(destination.StagingClaim, openBaoNamespace, destination.StagingClaim.Name) ||
			!openBaoPairIdentity(destination.DestinationPV, "", destination.DestinationPV.Name) ||
			!openBaoPairIdentity(destination.DestinationVolume, "longhorn-system", destination.DestinationPV.Name) {
			return false
		}
	}
	if r.Phase == openBaoPairStaged {
		return r.CopyPod == (benchmarkResource{}) && r.Successors == [2]benchmarkResource{} &&
			r.ClaimPending == 0 && r.Replacement == (benchmarkResource{})
	}
	if !r.Evicting || copyAssets != len(r.CopyAssets) || r.CopyPending != "" || r.CopyPod != r.CopyAssets[2] {
		return false
	}
	if r.Phase == openBaoPairCopied || r.Phase == openBaoPairReleasing {
		return r.Successors == [2]benchmarkResource{} && r.ClaimPending == 0 && r.Replacement == (benchmarkResource{})
	}
	bound, valid := r.successorProgress()
	if !valid {
		return false
	}
	if r.Phase == openBaoPairSourcesReleased {
		return r.Replacement == (benchmarkResource{})
	}
	if bound != len(r.Successors) || r.ClaimPending != 0 {
		return false
	}
	if r.Phase == openBaoPairRebound || r.Phase == openBaoPairResuming {
		return r.Replacement == (benchmarkResource{})
	}
	return r.Phase == openBaoPairReady && openBaoPairIdentity(r.Replacement, openBaoNamespace, r.Plan.Input.Member) &&
		r.Replacement.UID != r.Plan.Original.Member.UID
}

func verifyOpenBaoPairTransition(previous, next openBaoPairRecord) error {
	if !previous.valid() || !next.valid() || previous.Plan != next.Plan || previous == next || previous.RolledBack {
		return errOpenBaoProviderStorage
	}
	expected := previous
	if next.RolledBack {
		expected.RolledBack = true
		if expected == next {
			return nil
		}
		return errOpenBaoProviderStorage
	}
	switch previous.Phase {
	case openBaoPairPlanned:
		if next.Phase == openBaoPairPlanned {
			index, _ := previous.stageProgress()
			if !advanceOpenBaoAssetCreation(expected.StageAssets[:], next.StageAssets[:], &expected.StagePending,
				index, openBaoStageAssetKinds[:]) {
				return errOpenBaoProviderStorage
			}
		} else {
			expected.Phase, expected.Destinations = openBaoPairStaged, next.Destinations
		}
	case openBaoPairStaged:
		switch {
		case next.Phase != openBaoPairStaged:
			expected.Phase, expected.CopyPod = openBaoPairCopied, next.CopyPod
		case previous.FenceUser == "":
			expected.FenceUser = next.FenceUser
		case previous.FenceAssets[3].UID == "":
			index, _ := previous.fenceProgress()
			if !advanceOpenBaoAssetCreation(expected.FenceAssets[:], next.FenceAssets[:], &expected.FencePending,
				index, openBaoFenceAssetKinds[:]) {
				return errOpenBaoProviderStorage
			}
		case !previous.Evicting:
			expected.Evicting = true
		default:
			index, _ := previous.copyProgress()
			if !advanceOpenBaoAssetCreation(expected.CopyAssets[:], next.CopyAssets[:], &expected.CopyPending,
				index, openBaoCopyAssetKinds[:]) {
				return errOpenBaoProviderStorage
			}
		}
	case openBaoPairCopied:
		expected.Phase = openBaoPairReleasing
	case openBaoPairReleasing:
		expected.Phase = openBaoPairSourcesReleased
	case openBaoPairSourcesReleased:
		index, _ := previous.successorProgress()
		switch {
		case next.Phase != openBaoPairSourcesReleased:
			expected.Phase = openBaoPairRebound
		case index >= len(previous.Successors):
			return errOpenBaoProviderStorage
		case previous.ClaimPending == 0:
			expected.ClaimPending = index + 1
		default:
			if next.Successors[index] == (benchmarkResource{}) {
				return errOpenBaoProviderStorage
			}
			expected.ClaimPending, expected.Successors[index] = 0, next.Successors[index]
		}
	case openBaoPairRebound:
		expected.Phase = openBaoPairResuming
	case openBaoPairResuming:
		expected.Phase, expected.Replacement = openBaoPairReady, next.Replacement
	default:
		return errOpenBaoProviderStorage
	}
	if expected != next {
		return errOpenBaoProviderStorage
	}
	return nil
}

func (r openBaoPairRecord) successorProgress() (int, bool) {
	count := 0
	for index, successor := range r.Successors {
		if successor == (benchmarkResource{}) {
			continue
		}
		disk := r.Destinations[index]
		if count != index || !openBaoPairIdentity(successor, openBaoNamespace, disk.Source.PVC.Name) ||
			successor.UID == disk.Source.PVC.UID || successor.UID == disk.StagingClaim.UID ||
			(index == 1 && successor.UID == r.Successors[0].UID) {
			return 0, false
		}
		count++
	}
	return count, r.ClaimPending == 0 || (count < len(r.Successors) && r.ClaimPending == count+1)
}

var openBaoStageAssetKinds = [3]string{"data", "audit", "pod"}
var openBaoCopyAssetKinds = [3]string{"policy", "gate", "pod"}
var openBaoFenceAssetKinds = [4]string{"pod-policy", "pod-binding", "claim-policy", "claim-binding"}

func (r openBaoPairRecord) stageProgress() (int, bool) {
	stageNames := [3]string{
		"lh-openbao-staging-data-" + r.Plan.Run, "lh-openbao-staging-audit-" + r.Plan.Run, "lh-openbao-stage-" + r.Plan.Run,
	}
	return openBaoAssetProgress(r.StageAssets[:], r.StagePending, openBaoNamespace, stageNames[:], openBaoStageAssetKinds[:])
}

func (r openBaoPairRecord) copyProgress() (int, bool) {
	copyNames := [3]string{
		openBaoCopyIsolation.Prefix + r.Plan.Run, openBaoCopyGatePrefix + r.Plan.Run, "lh-openbao-copy-" + r.Plan.Run,
	}
	return openBaoAssetProgress(r.CopyAssets[:], r.CopyPending, openBaoNamespace, copyNames[:], openBaoCopyAssetKinds[:])
}

func (r openBaoPairRecord) fenceProgress() (int, bool) {
	pods, claims := "lh-openbao-storage-"+r.Plan.Run, "lh-openbao-claims-"+r.Plan.Run
	names := []string{pods, pods, claims, claims}
	return openBaoAssetProgress(r.FenceAssets[:], r.FencePending, "", names, openBaoFenceAssetKinds[:])
}

func openBaoAssetProgress(assets []benchmarkResource, pending, namespace string, names, kinds []string) (int, bool) {
	count := 0
	seen := map[string]bool{}
	for index, asset := range assets {
		if asset == (benchmarkResource{}) {
			continue
		}
		if index != count || seen[asset.UID] || !openBaoPairIdentity(asset, namespace, names[index]) {
			return 0, false
		}
		seen[asset.UID] = true
		count++
	}
	return count, pending == "" || count < len(assets) && pending == kinds[count]
}

func advanceOpenBaoAssetCreation(expected, next []benchmarkResource, pending *string, index int, kinds []string) bool {
	if index >= len(expected) {
		return false
	}
	if *pending == "" {
		*pending = kinds[index]
		return true
	}
	expected[index], *pending = next[index], ""
	return expected[index] != (benchmarkResource{})
}

func beginOpenBaoPairJournal(ctx context.Context, c client, plan openBaoPairPlan) (openBaoPairJournal, error) {
	initial := openBaoPairRecord{Plan: plan, Phase: openBaoPairPlanned}
	if c.context != "admin@prod" || !initial.valid() {
		return openBaoPairJournal{}, errOpenBaoProviderStorage
	}
	data, err := json.Marshal(initial)
	if err != nil {
		return openBaoPairJournal{}, err
	}
	name := openBaoPairJournalName(plan.Input.Member)
	created, err := c.create(ctx, plan.Source, object{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": object{
			"name": name, "namespace": openBaoPairJournalNamespace,
			"annotations": object{openBaoCopyFenceOwner: plan.Run + ":" + plan.Source},
		},
		"data": object{"record": string(data)},
	})
	if err != nil {
		return openBaoPairJournal{}, err
	}
	j := openBaoPairJournal{client: c, id: benchmarkIdentity(created), record: initial}
	if _, err := j.read(ctx); err != nil {
		return openBaoPairJournal{}, err
	}
	return j, nil
}

func openBaoPairJournalName(member string) string {
	return "longhorn-v2-storage-pair-" + member
}

func (j openBaoPairJournal) read(ctx context.Context) (object, error) {
	plan := j.record.Plan
	name := openBaoPairJournalName(plan.Input.Member)
	want, err := json.Marshal(j.record)
	if err != nil {
		return nil, err
	}
	current, err := j.client.get(ctx, "configmap", openBaoPairJournalNamespace, name, false)
	if err != nil || !recoveredNativeObject(current, "v1", "ConfigMap", openBaoPairJournalNamespace) ||
		text(current, "metadata", "name") != name || benchmarkIdentity(current) != j.id ||
		text(current, "metadata", "annotations", openBaoCopyFenceOwner) != plan.Run+":"+plan.Source ||
		text(current, "data", "record") != string(want) {
		return nil, errOpenBaoProviderStorage
	}
	return current, nil
}

func (j *openBaoPairJournal) advance(ctx context.Context, next openBaoPairRecord) error {
	if err := verifyOpenBaoPairTransition(j.record, next); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	current, err := j.read(ctx)
	if err != nil {
		return err
	}
	if err := sourceCurrent(ctx, j.client.run, j.record.Plan.Source); err != nil {
		return err
	}
	if err := j.client.patch(ctx, "configmap", openBaoPairJournalNamespace, current,
		object{"op": "test", "path": "/data/record", "value": text(current, "data", "record")},
		object{"op": "replace", "path": "/data/record", "value": string(data)}); err != nil {
		return err
	}
	rebound := *j
	rebound.record = next
	if _, err := rebound.read(ctx); err != nil {
		return err
	}
	*j = rebound
	return nil
}
