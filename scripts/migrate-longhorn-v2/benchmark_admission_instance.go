package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const benchmarkInstanceFenceMessage = "Benchmark instance creation and attachment are fenced for retirement"

var benchmarkInstanceKinds = map[string]string{
	"engines": "Engine", "replicas": "Replica", "enginefrontends": "EngineFrontend", "volumeattachments": "VolumeAttachment", "snapshots": "Snapshot",
}

func readBenchmarkInstanceGroups(ctx context.Context, c client) (map[string][]object, map[string]object, error) {
	const version = "longhorn.io/v1beta2"
	groups := map[string][]object{}
	observations := map[string]object{}
	for _, resource := range slices.Sorted(maps.Keys(benchmarkInstanceKinds)) {
		path := "/apis/" + version + "/namespaces/longhorn-system/" + resource
		raw, err := c.kubectl(ctx, nil, "get", "--raw="+path)
		if err != nil {
			return nil, nil, errors.New("native benchmark instance inventory is unavailable")
		}
		items, err := decodeBenchmarkAdmissionList(raw, version, benchmarkInstanceKinds[resource]+"List")
		if err != nil {
			return nil, nil, err
		}
		groups[resource] = items
		for _, item := range items {
			observations[text(item, "metadata", "uid")] = item
		}
	}
	return groups, observations, nil
}

func proveAndCheckpointBenchmarkInstances(ctx context.Context, c client, record *benchmarkRecord) error {
	if !activeBenchmarkV1FenceOwner(c, *record) || record.FenceCheckpoint != "" {
		return errors.New("benchmark instance checkpoint has no active workflow owner or is already recorded")
	}
	groups, observations, err := readBenchmarkInstanceGroups(ctx, c)
	if err != nil {
		return err
	}
	children, err := qualifyRetirementInstances(*record, groups)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, child := range children {
		counts[child.Resource]++
	}
	if counts["engines"] != 1 || counts["replicas"] != 3 || counts["volumeattachments"] != 1 || counts["enginefrontends"] != 0 {
		return errors.New("benchmark V1 instance roster differs from one engine, three replicas and one attachment")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	next := *record
	next.FenceCheckpoint = record.Run + "-" + hex.EncodeToString(nonce[:])
	policy, err := benchmarkRetirementInstanceFence(next, groups, next.FenceCheckpoint)
	if err != nil {
		return err
	}
	if err := advanceBenchmarkFenceJournal(ctx, c, record, next); err != nil {
		return err
	}
	if err := installBenchmarkFence(ctx, c, record, policy); err != nil {
		return err
	}
	name := text(policy, "metadata", "name")
	receipt := record.Fences[name]
	if err := proveBenchmarkFenceOnServingAPIs(ctx, c, record.Source, policy, receipt, func(ctx context.Context, client *http.Client, endpoint *url.URL) error {
		for _, child := range children {
			if err := probeBenchmarkInstanceAdmission(ctx, client, endpoint, child, observations[child.Identity.UID], name, record.FenceCheckpoint); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	afterActivation, _, err := readBenchmarkInstanceGroups(ctx, c)
	if err != nil {
		return err
	}
	qualified, err := qualifyRetirementInstances(*record, afterActivation)
	if err != nil || !reflect.DeepEqual(children, qualified) {
		return errors.New("benchmark instances changed while their admission fence activated")
	}
	for _, child := range children {
		original := observations[child.Identity.UID]
		kind := child.Resource + ".longhorn.io"
		if text(original, "metadata", "annotations", retirementCheckpointAnnotation) == record.FenceCheckpoint {
			return errors.New("benchmark instance checkpoint is already on an unqualified original")
		}
		if err := sourceCurrent(ctx, c.run, record.Source); err != nil {
			return err
		}
		if err := c.patch(ctx, kind, "longhorn-system", original, metadataChanges(original, "annotations", map[string]string{retirementCheckpointAnnotation: record.FenceCheckpoint})...); err != nil {
			return err
		}
		current, err := c.get(ctx, kind, "longhorn-system", child.Identity.Name, false)
		if err != nil || benchmarkIdentity(current) != child.Identity ||
			text(current, "metadata", "resourceVersion") == child.ResourceVersion ||
			text(current, "metadata", "annotations", retirementCheckpointAnnotation) != record.FenceCheckpoint ||
			!reflect.DeepEqual(value(current, "spec"), value(original, "spec")) {
			return errors.New("benchmark instance checkpoint was not confirmed")
		}
	}
	afterCheckpoints, current, err := readBenchmarkInstanceGroups(ctx, c)
	if err != nil {
		return err
	}
	qualified, err = qualifyRetirementInstances(*record, afterCheckpoints)
	if err != nil || len(qualified) != len(children) {
		return errors.New("the checkpointed benchmark instance roster changed")
	}
	for index, child := range qualified {
		old := children[index]
		if child.Identity != old.Identity || child.Resource != old.Resource ||
			text(current[child.Identity.UID], "metadata", "annotations", retirementCheckpointAnnotation) != record.FenceCheckpoint ||
			!reflect.DeepEqual(value(current[child.Identity.UID], "spec"), value(observations[old.Identity.UID], "spec")) {
			return errors.New("a native benchmark checkpoint identity or spec changed")
		}
	}
	_, err = observeBenchmarkReferenceFence(ctx, c, policy, receipt)
	return err
}

func probeBenchmarkInstanceAdmission(ctx context.Context, client *http.Client, endpoint *url.URL, child retirementChild, actual object, policy, checkpoint string) error {
	unverified := errors.New("the original instance retirement fence is unverified on this API server")
	const group = "longhorn.io"
	const version = group + "/v1beta2"
	kind, known := benchmarkInstanceKinds[child.Resource]
	if !known {
		return unverified
	}
	name := child.Identity.Name
	if child.Identity.Namespace != "longhorn-system" || !kubeName.MatchString(name) || len(name) > 253 ||
		child.Identity.UID == "" || child.ResourceVersion == "" || !benchmarkCheckpointPattern.MatchString(checkpoint) ||
		benchmarkIdentity(actual) != child.Identity || text(actual, "metadata", "resourceVersion") != child.ResourceVersion ||
		text(actual, "kind") != kind || text(actual, "apiVersion") != version ||
		value(actual, "metadata", "deletionTimestamp") != nil {
		return unverified
	}
	path := "/apis/" + version + "/namespaces/longhorn-system/" + child.Resource + "/" + name
	tested := []object{
		{"op": "test", "path": "/metadata/uid", "value": child.Identity.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": child.ResourceVersion},
	}
	positive := append(tested, metadataChanges(actual, "annotations", map[string]string{retirementCheckpointAnnotation: checkpoint})...)
	code, raw, err := benchmarkAdmissionDryRun(ctx, client, endpoint, path, positive)
	var admitted object
	if err != nil || code != http.StatusOK || strictjson.Validate(raw, nil) != nil || json.Unmarshal(raw, &admitted) != nil ||
		benchmarkIdentity(admitted) != child.Identity || text(admitted, "apiVersion") != version || text(admitted, "kind") != kind ||
		text(admitted, "metadata", "annotations", retirementCheckpointAnnotation) != checkpoint ||
		!reflect.DeepEqual(value(admitted, "spec"), value(actual, "spec")) ||
		!reflect.DeepEqual(value(admitted, "status"), value(actual, "status")) {
		return unverified
	}
	// A different metadata checkpoint must be rejected, including after a
	// legitimate earlier write already installed this checkpoint on the child.
	negative := append(tested, metadataChanges(actual, "annotations", map[string]string{retirementCheckpointAnnotation: "unverified"})...)
	code, raw, err = benchmarkAdmissionDryRun(ctx, client, endpoint, path, negative)
	if err != nil || !benchmarkDenialMatches(code, raw, group, child.Resource, name, policy, policy, benchmarkInstanceFenceMessage) {
		return unverified
	}
	return nil
}
