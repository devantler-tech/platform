package main

import (
	"context"
	"slices"
)

type nativeRestoreAdmission struct {
	Seed     nativeRestoreSeedReceipt
	State    nativeRestoreState
	Assets   []object
	Managers map[string]nativeRestoreV2ManagerIdentity
}

// Read the retained seed and the live three-disk pool before creating a
// journal or a scratch resource. The caller checks the same identities
// before progressing through any protected write.
func observeNativeRestoreAdmission(ctx context.Context, c client, source, run string, config []byte) (nativeRestoreAdmission, error) {
	seed, err := observeNativeRestoreSeed(ctx, c, source, run)
	if err != nil {
		return nativeRestoreAdmission{}, err
	}
	targets, managers, err := observeNativeRestorePool(ctx, c, config, run, nil)
	if err != nil {
		return nativeRestoreAdmission{}, err
	}
	uids := make(map[string]string, len(targets))
	for _, target := range targets {
		uids[target.Name] = target.UID
	}
	digest, err := nativeRestoreSeedDigest(seed.Record, seed.Seed)
	if err != nil {
		return nativeRestoreAdmission{}, err
	}
	state := nativeRestoreState{
		Version: 1, Source: source, Run: run, Phase: "created", SeedJournal: seed.Journal,
		SeedDigest: digest, Targets: targets, Resources: map[string]benchmarkResource{},
	}
	if state.validate(seed.Record, seed.Seed) != nil {
		return nativeRestoreAdmission{}, errNativeRestoreEvidence
	}
	assets, err := nativeRestoreAssets(seed.Record, seed.Seed, run, uids)
	if err != nil || sourceCurrent(ctx, c.run, source) != nil {
		return nativeRestoreAdmission{}, errNativeRestoreEvidence
	}
	return nativeRestoreAdmission{Seed: seed, State: state, Assets: assets, Managers: managers}, nil
}

func observeNativeRestorePool(ctx context.Context, c client, config []byte, run string, prior *nativeRestoreState) ([]nativeRestoreTarget, map[string]nativeRestoreV2ManagerIdentity, error) {
	pools, err := configuredMigrationPools(config)
	if err != nil {
		return nil, nil, err
	}
	var canary benchmarkResource
	if prior != nil {
		canary = prior.Resources["LonghornVolume"]
	}
	targets, memory, err := observeNativeRestorePreparedTargets(ctx, temporaryV2Activation{c: c, pools: pools}, canary)
	if err != nil {
		return nil, nil, err
	}
	if prior != nil && !slices.Equal(targets, prior.Targets) {
		return nil, nil, errNativeRestoreEvidence
	}
	s, err := v2Environment(ctx, c, pools)
	if err != nil {
		return nil, nil, err
	}
	if verifyTemporaryRegistrationVolumes(s.volumes, canary) != nil || verifyRedundancy(s) != nil || verifyBudgetSettings(ctx, c) != nil {
		return nil, nil, errNativeRestoreEvidence
	}
	tag := "longhorn-native-v2-" + run
	if _, err := planNativeRestoreDiskBudget(s, targets, tag, prior != nil, config); err != nil {
		return nil, nil, err
	}
	for _, target := range targets {
		report := memory[target.Name]
		if report.PageSizeKiB != 2048 || report.PersistentPages < 1024 || report.TotalPages < 1024 || report.AvailableKiB < 512<<10 {
			return nil, nil, errNativeRestoreEvidence
		}
	}
	managers, err := observeNativeRestoreManagers(ctx, c, targets, s)
	return targets, managers, err
}

// Only a single healthy V2 manager may serve each registered restore target.
// The selection is tied to the actual Pod identity, including its container
// epoch, and does not borrow a manager or Pod from another engine or node.
func observeNativeRestoreManagers(ctx context.Context, c client, targets []nativeRestoreTarget, s inventory) (map[string]nativeRestoreV2ManagerIdentity, error) {
	managers, err := c.list(ctx, "instancemanagers.longhorn.io", "longhorn-system")
	if err != nil {
		return nil, err
	}
	pods, err := c.list(ctx, "pods", "longhorn-system")
	if err != nil {
		return nil, err
	}
	bound, err := qualifyNativeRestoreManagers(targets, s.storageNodes, managers, pods)
	if err != nil {
		return nil, err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return nil, err
	}
	storage, _ := indexed(s.storageNodes)
	byManager, _ := indexed(managers)
	byPod, _ := indexed(pods)
	settings, err := c.list(ctx, "settings.longhorn.io", "longhorn-system")
	if err != nil {
		return nil, err
	}
	bySetting, err := indexed(settings)
	if err != nil {
		return nil, err
	}
	limits, err := c.list(ctx, "limitranges", "longhorn-system")
	if err != nil {
		return nil, err
	}
	quotas, err := c.list(ctx, "resourcequotas", "longhorn-system")
	if err != nil {
		return nil, err
	}
	account, err := c.get(ctx, "serviceaccounts", "longhorn-system", "longhorn-service-account", false)
	if err != nil {
		return nil, err
	}
	selectSettings := func(names ...string) []object {
		selected := make([]object, 0, len(names))
		for _, name := range names {
			selected = append(selected, bySetting[name])
		}
		return selected
	}
	resources := selectSettings("guaranteed-instance-manager-cpu", "data-engine-number-of-cpu-cores", "data-engine-memory-size", "data-engine-hugepage-enabled")
	argv := selectSettings("data-engine-cpu-mask", "data-engine-number-of-cpu-cores", "data-engine-memory-size", "data-engine-hugepage-enabled",
		"data-engine-iobuf-large-pool-size", "data-engine-iobuf-small-pool-size", "data-engine-log-flags",
		"data-engine-interrupt-mode-enabled", "data-engine-cpu-isolation-enabled")
	process := selectSettings("instance-manager-pod-liveness-probe-timeout", "system-managed-pods-image-pull-policy")
	for _, target := range targets {
		name := bound[target.Name].Pod
		manager, pod, lh := byManager[name], byPod[name], storage[target.Name]
		if _, err := qualifyNativeRestoreV2ManagerResources(target, nodes[target.Name], lh, manager, pod, resources, limits, quotas); err != nil {
			return nil, err
		}
		if _, err := qualifyNativeRestoreV2ManagerArgv(target, lh, manager, pod, argv); err != nil {
			return nil, err
		}
		if _, err := qualifyNativeRestoreV2ManagerMounts(target, lh, manager, pod, account, bySetting["log-path"]); err != nil {
			return nil, err
		}
		if _, err := qualifyNativeRestoreV2ManagerProcess(target, lh, manager, pod, process); err != nil {
			return nil, err
		}
	}
	return bound, nil
}

func qualifyNativeRestoreManagers(targets []nativeRestoreTarget, storage, managers, pods []object) (map[string]nativeRestoreV2ManagerIdentity, error) {
	if !nativeRestoreFixedRoster(targets) {
		return nil, errNativeRestoreEvidence
	}
	byStorage, err := indexed(storage)
	if err != nil {
		return nil, err
	}
	byManager, err := indexed(managers)
	if err != nil {
		return nil, err
	}
	byPod, err := indexed(pods)
	if err != nil {
		return nil, err
	}
	result := make(map[string]nativeRestoreV2ManagerIdentity, len(targets))
	seen := make(map[string]bool, 2*len(targets))
	for _, target := range targets {
		for name, manager := range byManager {
			if text(manager, "spec", "nodeID") != target.Name || text(manager, "spec", "dataEngine") != "v2" {
				continue
			}
			identity, err := qualifyNativeRestoreV2ManagerIdentity(target, byStorage[target.Name], manager, byPod[name])
			if err != nil || result[target.Name].ManagerUID != "" || seen[identity.ManagerUID] || seen[identity.PodUID] {
				return nil, errNativeRestoreEvidence
			}
			result[target.Name] = identity
			seen[identity.ManagerUID], seen[identity.PodUID] = true, true
		}
		if result[target.Name].ManagerUID == "" {
			return nil, errNativeRestoreEvidence
		}
	}
	return result, nil
}
