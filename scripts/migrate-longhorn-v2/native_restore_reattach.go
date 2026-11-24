package main

import (
	"maps"
	"slices"
)

const nativeReattachWait = `printf 'CANARY_REATTACHED\n'
while :; do sleep 10; done
`

type nativeReattachRequest struct {
	Run, Token, FirstNode, Target string
	Targets                       map[string]string
}

// Pool labels describe the temporary capacity pools, not every prepared V2
// worker. Consumers use one member of the executor's qualified UID roster;
// the executor must rebind those native identities before and after attachment.
func nativeRestoreConsumerNode(targets map[string]string, selected string) (string, error) {
	if len(targets) != 3 {
		return "", errNativeRestoreEvidence
	}
	seen := map[string]bool{}
	for node, uid := range targets {
		if !kubeName.MatchString(node) || len(node) > 63 || !nativeRestoreUID(uid) || seen[uid] {
			return "", errNativeRestoreEvidence
		}
		seen[uid] = true
	}
	if selected == "" {
		selected = slices.Sorted(maps.Keys(targets))[0]
	}
	if targets[selected] == "" {
		return "", errNativeRestoreEvidence
	}
	return selected, nil
}

// The executor proves the original
// three qualified node UIDs live, the same V2 storage binding, and absence of
// the first owned Pod and its attachment. The first Pod UID remains journaled.
// Build a fresh consumer without Velero's one-time restore init container:
// the second attachment reads both payloads and cannot initiate another copy.
func nativeRestoreReattachPod(record benchmarkRecord, seed canarySeedState, request nativeReattachRequest) (object, error) {
	if seed.validate(record) != nil || seed.Phase != "source-cleaned" ||
		!benchmarkRunPattern.MatchString(request.Run) || request.Run == record.Run ||
		len(request.Targets) != 3 || request.FirstNode == request.Target || request.Targets[request.FirstNode] == "" || request.Targets[request.Target] == "" {
		return nil, errSeedEvidence
	}
	if _, err := nativeRestoreConsumerNode(request.Targets, request.Target); err != nil {
		return nil, err
	}
	verify, err := nativeRestoreVerifyScript(seed.PayloadSHA256, request.Token)
	if err != nil {
		return nil, err
	}
	assets, err := benchmarkResources("v2", request.Run, benchmarkNamespace)
	if err != nil {
		return nil, err
	}
	for _, asset := range assets {
		if text(asset, "kind") != "Pod" {
			continue
		}
		spec := value(asset, "spec").(object)
		spec["nodeSelector"] = object{"kubernetes.io/hostname": request.Target}
		spec["activeDeadlineSeconds"] = 2700
		value(spec, "containers").([]any)[0].(object)["args"] = []any{verify + nativeReattachWait}
		value(spec, "containers").([]any)[0].(object)["env"] = nativeRestorePodUIDEnvironment()
		return asset, nil
	}
	return nil, errSeedEvidence
}
