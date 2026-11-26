package main

import (
	"context"
	"errors"
	"time"
)

type cnpgRecoveredDataObservation struct {
	storage    cnpgRecoveredStorage
	filesystem cnpgFilesystemBinding
	history    postgresState
	datapath   recoveryCiliumDatapath
}

// Correlate physical recovery and filesystem placement with the same primary,
// durable V2 graph and realized Cilium policy across a live network probe.
func (p *cnpgProofRun) observeRecoveredDatabaseData(ctx context.Context) (cnpgRecoveredDataObservation, error) {
	var empty cnpgRecoveredDataObservation
	if !commandContextActive(ctx) {
		return empty, errors.New("recovered database data observation deadline is inactive")
	}
	ctx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()

	before, err := p.observeRecoveredStorage(ctx)
	if err != nil {
		return empty, err
	}
	datapath, err := p.observeRecoveredDataCilium(ctx, before.pod)
	if err != nil {
		return empty, err
	}
	barrier := p.record.Plan.Barrier
	readHistory := func() (postgresState, error) {
		raw, readErr := p.c.readRecoveredPostgres(ctx, before.cluster, before.pod)
		if readErr != nil || verifyCNPGRecoveredHistory(raw, barrier) != nil {
			return postgresState{}, errors.New("the isolated primary's physical history is unverified")
		}
		return parsePostgresState(raw)
	}

	first, err := readHistory()
	if err != nil {
		return empty, err
	}
	filesystem, err := p.c.readRecoveredFilesystem(ctx, before.cluster, before.pod)
	if err != nil {
		return empty, err
	}
	if err := p.observeRecoveredNetworkDenial(ctx, before.cluster, before.pod, datapath); err != nil {
		return empty, err
	}
	last, err := readHistory()
	if err != nil {
		return empty, err
	}
	firstWAL, _ := walPosition(first.FlushLSN)
	lastWAL, _ := walPosition(last.FlushLSN)
	if first.SystemID != last.SystemID || first.Timeline != last.Timeline || first.PostmasterStart != last.PostmasterStart ||
		first.ReplayLSN != last.ReplayLSN || lastWAL < firstWAL {
		return empty, errors.New("recovered PostgreSQL history changed during the filesystem observation")
	}

	after, err := p.observeRecoveredStorage(ctx)
	if err != nil {
		return empty, err
	}
	if !commandContextActive(ctx) || !sameRecoveredStorageObservation(before, after) {
		return empty, errors.New("recovered V2 storage changed during the data observation")
	}
	currentDatapath, err := p.observeRecoveredDataCilium(ctx, after.pod)
	if err != nil || !commandContextActive(ctx) || currentDatapath != datapath {
		return empty, errors.New("realized recovery isolation changed during the data observation")
	}
	return cnpgRecoveredDataObservation{storage: after.binding, filesystem: filesystem, history: last, datapath: currentDatapath}, nil
}

func (p *cnpgProofRun) observeRecoveredDataCilium(ctx context.Context, pod object) (recoveryCiliumDatapath, error) {
	var empty recoveryCiliumDatapath
	prepared := p.record.Preparation
	if prepared == nil {
		return empty, errors.New("recovery isolation has no preparation checkpoint")
	}
	input, err := p.recoveryPolicyInput(ctx)
	if err != nil {
		return empty, err
	}
	input.Pod = pod
	current, err := p.c.observeRecoveryCilium(ctx, input)
	if err != nil {
		return empty, err
	}
	if current.Agent != prepared.Agent || current.Runtime != prepared.Runtime || current.PolicyUID != prepared.PolicyUID ||
		current.PolicyGeneration != prepared.Generation || current.PolicyRevision < prepared.Revision {
		return empty, errors.New("realized recovery isolation differs from the preparation checkpoint")
	}
	return current, nil
}
