package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type cnpgStorageEnvironment struct {
	Activation benchmarkResource
	Targets    map[string]temporaryV2Binding
	Disks      []cnpgRecoveryDiskBinding
}

// recoveryStorage qualifies registered disks while healthy V2 volumes coexist with retained V1 data.
func (a temporaryV2Activation) recoveryStorage(ctx context.Context, source cnpgRecoverySource) (cnpgStorageEnvironment, error) {
	if !knownCNPGArchiveSource(source.Namespace, source.Cluster) {
		return cnpgStorageEnvironment{}, errors.New("database recovery source is unreviewed")
	}
	return a.recoveryClaimStorage(ctx, source.Size)
}

func (a temporaryV2Activation) recoveryClaimStorage(ctx context.Context, size string) (result cnpgStorageEnvironment, err error) {
	phase := "input"
	defer func() {
		if err != nil {
			result = cnpgStorageEnvironment{}
			err = fmt.Errorf("recovery storage held: phase=%s", phase)
		}
	}()
	refused := errors.New("registered recovery storage is incomplete or changed")
	if a.c.context != "admin@prod" || !commandContextActive(ctx) {
		return result, refused
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	phase = "current-source"
	if err = sourceCurrent(ctx, a.c.run, a.source); err != nil {
		return result, err
	}
	phase = "activation-receipt"
	cm, record, err := a.read(ctx)
	if err != nil || cm == nil || !record.Completed || record.OwnerRun == a.owner {
		return result, refused
	}
	raw, err := a.c.run(ctx, nil, "gh", "api", "repos/devantler-tech/platform/actions/runs/"+record.OwnerRun+"/attempts/1")
	var run object
	if err != nil || json.Unmarshal(raw, &run) != nil || text(run, "status") != "completed" || text(run, "conclusion") != "success" || text(run, "head_sha") != record.Source || text(run, "head_branch") != "main" || text(run, "event") != "workflow_dispatch" || value(run, "run_attempt") != float64(1) || text(run, "path") != ".github/workflows/prepare-longhorn-v2-node.yaml" {
		return result, refused
	}
	phase = "runtime"
	if err = verifyBudgetSettings(ctx, a.c); err != nil {
		return result, err
	}
	s, err := v2Environment(ctx, a.c, a.pools)
	if err != nil {
		return result, err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return result, err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return result, err
	}
	result.Activation, result.Targets = benchmarkIdentity(cm), map[string]temporaryV2Binding{}
	phase = "native-disks"
	for _, selection := range temporaryV2Selections {
		name, _, e := resolveTemporaryInventoryTarget(selection, s.nodes, a.pools)
		if e != nil {
			return result, refused
		}
		node, lh := nodes[name], storage[name]
		registered, e := temporaryRegistrationState(lh, text(cm, "metadata", "uid"), selection)
		if e != nil || !registered {
			return result, refused
		}
		role := text(node, "metadata", "labels", migrationPoolLabel)
		binding, e := a.readBinding(ctx, selection, node, lh, a.pools[role])
		if e != nil {
			return result, refused
		}
		expected := record.Targets[selection]
		binding, e = a.prepared(ctx, binding, expected)
		if e != nil || !expected.SameStorageIdentity(binding) {
			return result, refused
		}
		for _, r := range s.replicas {
			if text(r, "spec", "nodeID") == name && text(r, "spec", "dataEngine") != "v2" {
				return result, refused
			}
		}
		disk := value(lh, "status", "diskStatus", "longhorn-v2")
		d, ok := disk.(object)
		if !ok {
			return result, refused
		}
		result.Targets[selection] = binding
		result.Disks = append(result.Disks, cnpgRecoveryDiskBinding{Selection: selection, Node: name, NodeUID: binding.LonghornUID, DiskUUID: text(d, "diskUUID"), InstanceManager: text(d, "instanceManagerName")})
	}
	// The native reads may take time. Rebind the complete runtime and allocation
	// graph afterwards rather than charging capacity against the earlier snapshot.
	phase = "runtime-continuity"
	s, err = v2Environment(ctx, a.c, a.pools)
	if err != nil {
		return result, err
	}
	nodes, err = indexed(s.nodes)
	if err != nil {
		return result, err
	}
	storage, err = indexed(s.storageNodes)
	if err != nil {
		return result, err
	}
	for _, binding := range result.Targets {
		node, lh := nodes[binding.Name], storage[binding.Name]
		registered, e := temporaryRegistrationState(lh, result.Activation.UID, binding.Selection)
		if e != nil || !registered || text(node, "metadata", "uid") != binding.NodeUID || text(node, "spec", "providerID") != binding.ProviderID || text(node, "status", "nodeInfo", "systemUUID") != binding.MachineUUID || text(node, "status", "nodeInfo", "bootID") != binding.BootID || text(node, "status", "nodeInfo", "osImage") != binding.OSImage || text(lh, "metadata", "uid") != binding.LonghornUID || text(node, "metadata", "annotations", fenceAnnotation) != "" {
			return result, refused
		}
		for _, r := range s.replicas {
			if text(r, "spec", "nodeID") == binding.Name && text(r, "spec", "dataEngine") != "v2" {
				return result, refused
			}
		}
	}
	phase = "complete-allocations"
	backing, err := a.c.list(ctx, "backingimages.longhorn.io", "longhorn-system")
	if err != nil {
		return result, err
	}
	dataSources, err := a.c.list(ctx, "backingimagedatasources.longhorn.io", "longhorn-system")
	if err != nil || len(dataSources) != 0 {
		return result, refused
	}
	shards, err := a.c.list(ctx, "shards.longhorn.io", "longhorn-system")
	if err != nil {
		return result, err
	}
	groups, err := a.c.list(ctx, "shardgroups.longhorn.io", "longhorn-system")
	if err != nil || len(groups) != 0 {
		return result, refused
	}
	if err = verifyRecoveryClaimCapacity(size, s.storageNodes, s.replicas, backing, shards, result.Disks); err != nil {
		return result, err
	}
	phase = "continuity"
	after, current, err := a.read(ctx)
	if err != nil || benchmarkIdentity(after) != result.Activation || text(after, "metadata", "resourceVersion") != text(cm, "metadata", "resourceVersion") || !recoveryJSONEqual(record, current) {
		return result, refused
	}
	if err = sourceCurrent(ctx, a.c.run, a.source); err != nil || !commandContextActive(ctx) {
		return result, refused
	}
	return result, nil
}
