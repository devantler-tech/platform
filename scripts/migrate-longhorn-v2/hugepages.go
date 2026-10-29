package main

import (
	"context"
	"errors"
	"strings"
)

func hugepagesReady(n object) bool {
	status, _ := value(n, "status").(map[string]any)
	return ready(status) && text(n, "status", "capacity", "hugepages-2Mi") == "2Gi" &&
		text(n, "status", "allocatable", "hugepages-2Mi") == "2Gi" && workerPressureClear(n)
}

func workerPressureClear(n object) bool {
	conditions, _ := value(n, "status", "conditions").([]any)
	clear := map[string]bool{}
	for _, raw := range conditions {
		condition, _ := raw.(object)
		if text(condition, "status") == "False" {
			clear[text(condition, "type")] = true
		}
	}
	return clear["DiskPressure"] && clear["MemoryPressure"] && clear["PIDPressure"]
}

// Restarting kubelet preserves running containers and all data. It is required
// only when dynamically allocated huge pages have not reached Node capacity.
func refreshHugepages(ctx context.Context, c client, source string, original object) error {
	j := journal{Target: text(original, "metadata", "name"), NodeUID: text(original, "metadata", "uid"),
		ProviderID: text(original, "spec", "providerID"), MachineUUID: text(original, "status", "nodeInfo", "systemUUID")}
	boot := text(original, "status", "nodeInfo", "bootID")
	if j.NodeUID == "" || j.ProviderID == "" || j.MachineUUID == "" || boot == "" {
		return errors.New("worker identity is incomplete for a kubelet refresh")
	}
	get := func() (object, string, error) {
		n, err := c.get(ctx, "node", "", j.Target, false)
		if err != nil {
			return nil, "", err
		}
		address, err := verifyNode(n, j, true)
		status, _ := value(n, "status").(map[string]any)
		if err != nil || !ready(status) || value(n, "spec", "unschedulable") == true || text(n, "status", "nodeInfo", "bootID") != boot {
			return nil, "", errors.New("worker readiness or hardware changed during kubelet refresh")
		}
		if err = c.verifyHost(ctx, address, n); err != nil {
			return nil, "", err
		}
		return n, address, nil
	}
	n, address, err := get()
	if err != nil {
		return err
	}
	pages, err := c.run(ctx, nil, "talosctl", "--nodes="+address, "read", "/proc/sys/vm/nr_hugepages")
	if err != nil || strings.TrimSpace(string(pages)) != "1024" {
		return errors.New("the host has not allocated the reviewed huge pages")
	}
	modules, err := c.run(ctx, nil, "talosctl", "--nodes="+address, "read", "/proc/modules")
	if err != nil {
		return errors.New("worker kernel modules could not be read")
	}
	loaded := map[string]bool{}
	for line := range strings.SplitSeq(string(modules), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			loaded[fields[0]] = true
		}
	}
	if !loaded["nvme_tcp"] || !loaded["vfio_pci"] || !loaded["uio_pci_generic"] {
		return errors.New("required V2 kernel modules are not loaded")
	}
	if hugepagesReady(n) {
		return nil
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	if _, _, err = get(); err != nil {
		return err
	}
	// Never repeat an uncertain service request. Positive capacity and identity
	// readback, rather than its return code, establishes completion.
	_, _ = c.run(ctx, nil, "talosctl", "--nodes="+address, "service", "kubelet", "restart")
	return waitReady(ctx, func() error {
		n, _, err := get()
		if err != nil || !hugepagesReady(n) {
			return errors.New("kubelet has not reported the required huge-page capacity")
		}
		return nil
	})
}

func refreshClients(ctx context.Context, c client, source string, pools map[string]migrationPool) error {
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	if err = verifyCapacityRoster(s.nodes, pools, false); err != nil {
		return err
	}
	for _, n := range s.nodes {
		if strings.HasPrefix(text(n, "metadata", "name"), "prod-control-plane-") {
			continue
		}
		fresh, err := c.snapshot(ctx)
		if err != nil {
			return err
		}
		if err = verifyRedundancy(fresh); err != nil {
			return err
		}
		if err = verifyCapacityRoster(fresh.nodes, pools, false); err != nil {
			return err
		}
		if err = refreshHugepages(ctx, c, source, n); err != nil {
			return err
		}
	}
	return nil
}
