package main

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

var providerStatsWorker = regexp.MustCompile(`^prod-worker-[123]$`)
var providerStatsHost = regexp.MustCompile(`^hcloud://[1-9][0-9]{0,18}$`)

type providerVolumeUsageReceipt struct {
	Claims, Measured, Unmeasured                                int
	RequestedBytes, MeasuredUsedBytes, UnmeasuredRequestedBytes uint64
}

type providerStatsEpoch struct{ UID, ProviderID, BootID, Hardware, Address string }

func providerUsageGiBUpper(bytes uint64) uint64 {
	if bytes == 0 {
		return 0
	}
	return ((bytes - 1) >> 30) + 1
}

func authorizeProviderVolumeFootprint(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != "inventory-provider-volume-footprint" {
		return errors.New("the explicit provider-volume footprint operation is required")
	}
	return authorizeWorkflow(getenv, "inventory-provider-volume-footprint")
}

// Keep the production credentials behind fixed Kubernetes and Talos reads.
func providerFootprintReadCommand(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if input != nil || !providerFootprintReadAllowed(program, args...) {
		return nil, errors.New("the provider footprint operation permits only fixed reads")
	}
	return inventoryExecuteCommand(ctx, program, args...)
}

func providerFootprintReadAllowed(program string, args ...string) bool {
	if (program == "git" || program == "talosctl") && inventoryReadAllowed(program, args...) {
		return true
	}
	if program == "talosctl" && len(args) == 4 && slices.Equal(args[1:], []string{"get", "disks", "--output=json"}) {
		target, ok := strings.CutPrefix(args[0], "--nodes=")
		address, err := netip.ParseAddr(target)
		return ok && err == nil && netip.MustParsePrefix("10.0.0.0/16").Contains(address)
	}
	if program != "kubectl" || len(args) < 4 ||
		!slices.Equal(args[:3], []string{"--context=admin@prod", "--request-timeout=20s", "get"}) {
		return false
	}
	switch tail := args[3:]; len(tail) {
	case 2:
		return slices.Equal(tail, []string{"persistentvolumes", "--output=json"})
	case 3:
		if slices.Equal(tail, []string{"persistentvolumeclaims", "--output=json", "--all-namespaces"}) {
			return true
		}
		if tail[0] == "node" {
			return providerStatsWorker.MatchString(tail[1]) && tail[2] == "--output=json"
		}
		namespace, ok := strings.CutPrefix(tail[2], "--namespace=")
		return ok && tail[0] == "pods" && tail[1] == "--output=json" && slices.Contains([]string{"kube-system", "observability", "openbao"}, namespace)
	default:
		return false
	}
}

func observeProviderVolumeFootprint(ctx context.Context, c client, source string) (providerVolumeUsageReceipt, error) {
	if c.context != "admin@prod" {
		return providerVolumeUsageReceipt{}, errors.New("protected production provider-volume observation is required")
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return providerVolumeUsageReceipt{}, err
	}
	talosClient, err := openInventoryTalosClient(ctx)
	if err != nil {
		return providerVolumeUsageReceipt{}, errors.New("the protected Talos reader is unavailable")
	}
	receipt, err := observeProviderVolumeUsage(ctx, c, talosClient)
	closeErr := talosClient.Close()
	if err != nil {
		return providerVolumeUsageReceipt{}, err
	}
	if closeErr != nil {
		return providerVolumeUsageReceipt{}, errors.New("the protected Talos reader could not close")
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return providerVolumeUsageReceipt{}, err
	}
	return receipt, nil
}

func observeProviderVolumeUsage(ctx context.Context, c client, api providerTalosMountAPI) (providerVolumeUsageReceipt, error) {
	claims, mounts, err := observeProviderVolumeProjection(ctx, c)
	if err != nil {
		return providerVolumeUsageReceipt{}, err
	}
	byNode := map[string][]providerPodMount{}
	for _, mount := range mounts {
		if !providerStatsWorker.MatchString(mount.Node) {
			return providerVolumeUsageReceipt{}, errors.New("a provider claim is outside the reviewed worker roster")
		}
		byNode[mount.Node] = append(byNode[mount.Node], mount)
	}
	nodes := slices.Sorted(maps.Keys(byNode))
	epochs := make(map[string]providerStatsEpoch, len(nodes))
	devices := make(map[string]map[string]string, len(nodes))
	used := make(map[string]uint64, len(mounts))
	for _, node := range nodes {
		epoch, err := readProviderStatsEpoch(ctx, c, node)
		if err != nil {
			return providerVolumeUsageReceipt{}, err
		}
		disks, err := readProviderDevices(ctx, c, epoch.Address)
		if err != nil {
			return providerVolumeUsageReceipt{}, err
		}
		stats, err := readProviderMounts(ctx, api, epoch.Address, byNode[node], disks)
		if err != nil {
			return providerVolumeUsageReceipt{}, err
		}
		maps.Copy(used, stats)
		epochs[node] = epoch
		devices[node] = disks
	}
	afterClaims, afterMounts, err := observeProviderVolumeProjection(ctx, c)
	if err != nil || !slices.Equal(claims, afterClaims) || !slices.Equal(mounts, afterMounts) {
		return providerVolumeUsageReceipt{}, errors.New("provider claim bindings or consumer mounts changed during observation")
	}
	for _, node := range nodes {
		disks, err := readProviderDevices(ctx, c, epochs[node].Address)
		if err != nil || !maps.Equal(disks, devices[node]) {
			return providerVolumeUsageReceipt{}, errors.New("provider device identity changed during observation")
		}
		after, err := readProviderStatsEpoch(ctx, c, node)
		if err != nil || after != epochs[node] {
			return providerVolumeUsageReceipt{}, errors.New("provider claim worker identity changed during observation")
		}
	}
	return summarizeProviderVolumeUsage(claims, used)
}

func summarizeProviderVolumeUsage(claims []providerVolumeClaim, used map[string]uint64) (providerVolumeUsageReceipt, error) {
	receipt := providerVolumeUsageReceipt{Claims: len(claims)}
	for _, claim := range claims {
		amount, measured := used[claim.ClaimUID]
		if ^uint64(0)-receipt.RequestedBytes < claim.RequestedBytes || amount > claim.RequestedBytes {
			return providerVolumeUsageReceipt{}, errors.New("provider volume capacity exceeded the supported aggregate")
		}
		receipt.RequestedBytes += claim.RequestedBytes
		if measured {
			receipt.Measured++
			receipt.MeasuredUsedBytes += amount
		} else {
			receipt.Unmeasured++
			receipt.UnmeasuredRequestedBytes += claim.RequestedBytes
		}
	}
	if receipt.Measured != len(used) {
		return providerVolumeUsageReceipt{}, errors.New("filesystem statistics included an unverified provider claim")
	}
	return receipt, nil
}

func observeProviderVolumeProjection(ctx context.Context, c client) ([]providerVolumeClaim, []providerPodMount, error) {
	pvs, err := c.list(ctx, "persistentvolumes", "")
	if err != nil {
		return nil, nil, err
	}
	pvcs, err := c.list(ctx, "persistentvolumeclaims", "*")
	if err != nil {
		return nil, nil, err
	}
	claims, err := selectProviderVolumeClaims(pvs, pvcs)
	if err != nil {
		return nil, nil, err
	}
	namespaces := map[string]bool{}
	for _, claim := range claims {
		namespaces[claim.Namespace] = true
	}
	pods := []object{}
	for _, namespace := range slices.Sorted(maps.Keys(namespaces)) {
		current, err := c.list(ctx, "pods", namespace)
		if err != nil {
			return nil, nil, err
		}
		pods = append(pods, current...)
	}
	mounts, err := selectProviderPodMounts(claims, pods)
	return claims, mounts, err
}

func readProviderStatsEpoch(ctx context.Context, c client, name string) (providerStatsEpoch, error) {
	if !providerStatsWorker.MatchString(name) {
		return providerStatsEpoch{}, errors.New("provider filesystem statistics worker is unverified")
	}
	n, err := c.get(ctx, "node", "", name, false)
	if err != nil || text(n, "metadata", "name") != name || value(n, "metadata", "deletionTimestamp") != nil ||
		!ready(valueObject(n, "status")) || !providerStatsHost.MatchString(text(n, "spec", "providerID")) ||
		text(n, "metadata", "uid") == "" || text(n, "status", "nodeInfo", "bootID") == "" {
		return providerStatsEpoch{}, errors.New("provider filesystem statistics worker is unverified")
	}
	address, err := workerPrivateAddress(n)
	if err != nil || c.verifyHost(ctx, address, n) != nil {
		return providerStatsEpoch{}, errors.New("provider filesystem statistics worker hardware is unverified")
	}
	return providerStatsEpoch{text(n, "metadata", "uid"), text(n, "spec", "providerID"),
		text(n, "status", "nodeInfo", "bootID"), text(n, "status", "nodeInfo", "systemUUID"), address}, nil
}
