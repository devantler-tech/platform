package main

import (
	"context"
	"encoding/json"
	"errors"
)

func (j *openBaoPairJournal) requestFollowerEviction(ctx context.Context, token string,
	proveFollower func(context.Context, benchmarkResource) error) error {
	r := j.record
	if j.client.context != "admin@prod" || r.Phase != openBaoPairStaged || !r.valid() || r.Evicting ||
		proveFollower == nil {
		return errOpenBaoProviderStorage
	}
	if err := j.proveAdmissionFences(ctx); err != nil {
		return err
	}
	if err := verifyOpenBaoDestinationsDetached(ctx, j.client, r.Destinations); err != nil {
		return err
	}
	if err := j.submitFollowerEviction(ctx, token, true); err != nil {
		return err
	}
	if err := proveFollower(ctx, r.Plan.Original.Member); err != nil {
		return err
	}
	r.Evicting = true
	if err := j.advance(ctx, r); err != nil {
		return err
	}
	return j.submitFollowerEviction(ctx, token, false)
}

func (j openBaoPairJournal) submitFollowerEviction(ctx context.Context, token string, dryRun bool) error {
	plan := j.record.Plan
	if j.client.context != "admin@prod" || j.record.Evicting == dryRun {
		return errOpenBaoProviderStorage
	}
	pod, err := j.readEvictionSource(ctx, token)
	if err != nil {
		return err
	}
	member := plan.Original.Member
	opts := object{"preconditions": object{"uid": member.UID, "resourceVersion": text(pod, "metadata", "resourceVersion")}}
	if dryRun {
		opts["dryRun"] = []any{"All"}
	}
	payload, err := json.Marshal(object{"apiVersion": "policy/v1", "kind": "Eviction",
		"metadata": object{"name": member.Name, "namespace": openBaoNamespace}, "deleteOptions": opts})
	if err != nil {
		return err
	}
	if err := sourceCurrent(ctx, j.client.run, plan.Source); err != nil {
		return err
	}
	path := "/api/v1/namespaces/" + openBaoNamespace + "/pods/" + member.Name + "/eviction"
	_, err = j.client.kubectl(ctx, payload, "create", "--raw="+path, "--filename=/dev/stdin")
	return err
}

func (j openBaoPairJournal) readEvictionSource(ctx context.Context, token string) (object, error) {
	plan := j.record.Plan
	storage, err := j.readCopyStorage(ctx, token, 3, false)
	if err != nil {
		return nil, err
	}
	bound, err := bindOpenBaoProviderStorage(plan.Input.Member, storage.stateful, storage.pods,
		storage.claims, storage.pvs, storage.tickets)
	server, ok := openBaoServerID(storage.node)
	if err != nil || bound != plan.Original || !ok {
		return nil, errors.Join(err, errOpenBaoProviderStorage)
	}
	for _, disk := range bound.Disks {
		if !openBaoProviderAssignedToServer(storage.provider[:], disk, server) {
			return nil, errOpenBaoProviderStorage
		}
	}
	pod := uniqueOpenBaoObject(storage.pods, bound.Member.Name)
	if benchmarkIdentity(pod) != bound.Member || !openBaoOriginalStorageMember(pod, storage.stateful) {
		return nil, errOpenBaoProviderStorage
	}
	return pod, nil
}
