package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/devantler-tech/platform/internal/longhornmigration"
)

func observeNativeRestorePreparedTargets(ctx context.Context, a temporaryV2Activation) ([]nativeRestoreTarget, map[string]hostMemoryReport, error) {
	activation, err := a.accepted(ctx)
	if err != nil {
		return nil, nil, errNativeRestoreEvidence
	}
	targets := make([]nativeRestoreTarget, 0, len(activation.Targets))
	memories := make(map[string]hostMemoryReport, len(activation.Targets))
	for _, selection := range longhornmigration.ActivationSelections() {
		binding := activation.Targets[selection]
		target, memory, err := observeNativeRestorePreparedTarget(ctx, a, activation, binding)
		if err != nil {
			return nil, nil, err
		}
		targets = append(targets, target)
		memories[binding.Name] = memory
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	journal, _, err := a.read(ctx)
	if err != nil || !sameNativeRestoreActivation(activation, journal) || !nativeRestoreFixedRoster(targets) {
		return nil, nil, errNativeRestoreEvidence
	}
	return targets, memories, nil
}

func sameNativeRestoreActivation(activation temporaryActivationReceipt, journal object) bool {
	return benchmarkIdentity(journal) == activation.Journal &&
		fmt.Sprintf("%x", sha256.Sum256([]byte(text(journal, "data", "record")))) == activation.RecordSHA256
}

func observeNativeRestorePreparedTarget(ctx context.Context, a temporaryV2Activation, activation temporaryActivationReceipt, binding temporaryV2Binding) (nativeRestoreTarget, hostMemoryReport, error) {
	hold := errNativeRestoreEvidence
	if activation.Targets[binding.Selection] != binding || binding.PreparationUID == "" {
		return nativeRestoreTarget{}, hostMemoryReport{}, hold
	}
	name := "longhorn-v2-prepare-" + binding.Selection
	cm, err := a.c.get(ctx, "configmap", "longhorn-system", name, false)
	if err != nil || text(cm, "metadata", "uid") != binding.PreparationUID {
		return nativeRestoreTarget{}, hostMemoryReport{}, hold
	}
	preparationRecord, err := preparedTemporaryBinding(cm, binding)
	if err != nil {
		return nativeRestoreTarget{}, hostMemoryReport{}, hold
	}
	node, err := a.c.get(ctx, "node", "", binding.Name, false)
	if err != nil || text(node, "status", "nodeInfo", "bootID") != binding.BootID {
		return nativeRestoreTarget{}, hostMemoryReport{}, hold
	}
	address, err := verifyNode(node, preparationRecord, true)
	if err != nil {
		return nativeRestoreTarget{}, hostMemoryReport{}, hold
	}
	memory, err := nativeRestorePreparedMemory(ctx, a.c, address)
	if err != nil {
		return nativeRestoreTarget{}, hostMemoryReport{}, hold
	}
	profile := nativeRestoreUnusedProfile
	if binding.Selection == "temporary-v2-1" {
		profile = nativeRestoreRetainedProfile
		p := preparation{c: a.c, target: binding.Name, name: name, temporarySelection: binding.Selection, pools: a.pools, j: preparationRecord}
		retained := temporaryPreparation{preparation: p, reader: a.reader, staging: true}
		identity, err := retained.readRetainedRecovery(ctx, address, binding.BootID, false)
		if err != nil || identity.StagePartitionUUID != binding.RawPartitionUUID {
			return nativeRestoreTarget{}, hostMemoryReport{}, hold
		}
	}
	if err := recheckNativeRestorePreparedTarget(ctx, a, activation, binding, cm); err != nil {
		return nativeRestoreTarget{}, hostMemoryReport{}, err
	}
	prepared := nativeRestorePreparationReceipt{
		Profile: profile, Journal: benchmarkIdentity(cm),
		JournalSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(text(cm, "data", "record")))),
		BootID:        binding.BootID, RawPartitionUUID: binding.RawPartitionUUID, ProviderID: binding.ProviderID,
		MachineUUID: binding.MachineUUID, LonghornUID: binding.LonghornUID, DiskUUID: activation.DiskUUIDs[binding.Selection],
		Activation: activation.Journal, ActivationSHA256: activation.RecordSHA256,
	}
	if _, err := prepared.mapper(binding.Name); err != nil {
		return nativeRestoreTarget{}, hostMemoryReport{}, hold
	}
	return nativeRestoreTarget{Name: binding.Name, UID: binding.NodeUID, Preparation: prepared}, memory, nil
}

func recheckNativeRestorePreparedTarget(ctx context.Context, a temporaryV2Activation, activation temporaryActivationReceipt, binding temporaryV2Binding, journal object) error {
	node, err := a.c.get(ctx, "node", "", binding.Name, false)
	if err != nil {
		return errNativeRestoreEvidence
	}
	storage, err := a.c.get(ctx, "nodes.longhorn.io", "longhorn-system", binding.Name, false)
	if err != nil {
		return errNativeRestoreEvidence
	}
	registered, err := temporaryRegistrationState(storage, activation.Journal.UID, binding.Selection)
	disk := valueObject(storage, "status", "diskStatus", "longhorn-v2")
	if err != nil || !registered || !nativeRestoreDiskIdentityUsable(storage, disk) ||
		text(disk, "diskUUID") != activation.DiskUUIDs[binding.Selection] {
		return errNativeRestoreEvidence
	}
	observed, err := a.readBinding(ctx, binding.Selection, node, storage, a.pools[text(node, "metadata", "labels", migrationPoolLabel)])
	if err != nil {
		return errNativeRestoreEvidence
	}
	current, err := a.c.get(ctx, "configmap", "longhorn-system", text(journal, "metadata", "name"), false)
	if err != nil || benchmarkIdentity(current) != benchmarkIdentity(journal) ||
		text(current, "data", "record") != text(journal, "data", "record") {
		return errNativeRestoreEvidence
	}
	observed.PreparationUID = text(current, "metadata", "uid")
	if observed != binding {
		return errNativeRestoreEvidence
	}
	return nil
}

// Compare the current kernel hugepage total on either side of the memory read.
func nativeRestorePreparedMemory(ctx context.Context, c client, address string) (hostMemoryReport, error) {
	pages, err := c.talosCommand(ctx, talosReadLimit, address, "read", "/proc/sys/vm/nr_hugepages")
	if err != nil {
		return hostMemoryReport{}, errNativeRestoreEvidence
	}
	memory, err := c.talosCommand(ctx, talosReadLimit, address, "read", "/proc/meminfo")
	if err != nil {
		return hostMemoryReport{}, errNativeRestoreEvidence
	}
	after, err := c.talosCommand(ctx, talosReadLimit, address, "read", "/proc/sys/vm/nr_hugepages")
	if err != nil || !bytes.Equal(pages, after) {
		return hostMemoryReport{}, errNativeRestoreEvidence
	}
	return parseHostMemory(pages, memory)
}
