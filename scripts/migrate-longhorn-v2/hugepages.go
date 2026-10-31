package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func hugepagesReady(n object) bool {
	status, _ := value(n, "status").(map[string]any)
	return ready(status) && text(n, "status", "capacity", "hugepages-2Mi") == "2Gi" &&
		text(n, "status", "allocatable", "hugepages-2Mi") == "2Gi" && workerPressureClear(n)
}

func workerPressureClear(n object) bool {
	conditions, _ := value(n, "status", "conditions").([]any)
	cleared := map[string]bool{}
	for _, raw := range conditions {
		condition, _ := raw.(object)
		if text(condition, "status") == "False" {
			cleared[text(condition, "type")] = true
		}
	}
	return cleared["DiskPressure"] && cleared["MemoryPressure"] && cleared["PIDPressure"]
}

// Restarting kubelet preserves running containers and all data. It is required
// only when dynamically allocated huge pages have not reached Node capacity.
func refreshHugepages(ctx context.Context, c client, source string, original object, pods []object) (err error) {
	j := journal{Target: text(original, "metadata", "name"), NodeUID: text(original, "metadata", "uid"),
		ProviderID: text(original, "spec", "providerID"), MachineUUID: text(original, "status", "nodeInfo", "systemUUID")}
	boot := text(original, "status", "nodeInfo", "bootID")
	fence := refreshFencePrefix + j.Target
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
		cordoned := value(n, "spec", "unschedulable") == true
		if err != nil || !ready(status) || cordoned && text(n, "metadata", "annotations", fenceAnnotation) != fence || text(n, "status", "nodeInfo", "bootID") != boot {
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
	// An interrupted run's cordon is adopted, and released unless this run's restart is unproven.
	held, restarted := text(n, "metadata", "annotations", fenceAnnotation) == fence, false
	defer func() {
		if held && !restarted {
			release, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			err = errors.Join(err, c.releaseRefreshCordon(release, j.Target))
		}
	}()
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
	// Counted pages with pressure left are not stale capacity: a restart would not help.
	if text(n, "status", "capacity", "hugepages-2Mi") == "2Gi" && text(n, "status", "allocatable", "hugepages-2Mi") == "2Gi" {
		return errors.New("worker " + text(n, "metadata", "name") + " reports resource pressure; a kubelet refresh would not clear it")
	}
	moves, err := planRefreshMoves(n, pods)
	if err != nil {
		return err
	}
	if len(moves) > 0 {
		if err = sourceCurrent(ctx, c.run, source); err != nil {
			return err
		}
		if err = c.moveForRefresh(ctx, n, moves, pods, &held); err != nil {
			return err
		}
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	if _, _, err = get(); err != nil {
		return err
	}
	// Never repeat an uncertain service request. Positive capacity and identity
	// readback, rather than its return code, establishes completion.
	restarted = true
	_, _ = c.run(ctx, nil, "talosctl", "--nodes="+address, "service", "kubelet", "restart")
	if err = waitReady(ctx, func() error {
		n, _, err := get()
		if err != nil || !hugepagesReady(n) {
			return errors.New("kubelet has not reported the required huge-page capacity")
		}
		return nil
	}); err != nil {
		return err
	}
	if held {
		return c.releaseRefreshCordon(ctx, j.Target)
	}
	return nil
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
		if err = refreshHugepages(ctx, c, source, n, fresh.pods); err != nil {
			return err
		}
	}
	return nil
}

// memoryBytes reads the Kubernetes memory quantities nodes and Pods report here.
// An unknown form is an error, so a request that cannot be measured holds the restart.
func memoryBytes(quantity string) (float64, error) {
	for _, unit := range []struct {
		suffix string
		factor float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"m", 1e-3}, {"", 1}} {
		if number, found := strings.CutSuffix(quantity, unit.suffix); found {
			amount, err := strconv.ParseFloat(number, 64)
			if err != nil || amount < 0 {
				break
			}
			return amount * unit.factor, nil
		}
	}
	return 0, fmt.Errorf("memory quantity %q cannot be measured", quantity)
}

func containerMemoryRequest(raw any) (float64, error) {
	container, _ := raw.(map[string]any)
	request := text(container, "resources", "requests", "memory")
	if request == "" {
		return 0, nil
	}
	return memoryBytes(request)
}

// podMemoryRequest is what the scheduler and kubelet count for one Pod: a Pod-level
// request when set, otherwise the running containers and native sidecars or the peak
// of any init container beside the sidecars started before it, plus runtime overhead.
func podMemoryRequest(pod object) (float64, error) {
	var total, sidecars, peakInit float64
	if request := text(pod, "spec", "resources", "requests", "memory"); request != "" {
		var err error
		if total, err = memoryBytes(request); err != nil {
			return 0, err
		}
	} else {
		containers, _ := value(pod, "spec", "containers").([]any)
		for _, raw := range containers {
			request, err := containerMemoryRequest(raw)
			if err != nil {
				return 0, err
			}
			total += request
		}
		initContainers, _ := value(pod, "spec", "initContainers").([]any)
		for _, raw := range initContainers {
			request, err := containerMemoryRequest(raw)
			if err != nil {
				return 0, err
			}
			container, _ := raw.(map[string]any)
			if text(container, "restartPolicy") == "Always" {
				sidecars += request
				peakInit = max(peakInit, sidecars)
				continue
			}
			peakInit = max(peakInit, sidecars+request)
		}
		total = max(total+sidecars, peakInit)
	}
	if overhead := text(pod, "spec", "overhead", "memory"); overhead != "" {
		extra, err := memoryBytes(overhead)
		if err != nil {
			return 0, err
		}
		total += extra
	}
	return total, nil
}

// refreshExcess is how much of the worker's Pod memory requests would not fit once a
// kubelet restart counts the huge pages; negative when they fit, by that much room.
func refreshExcess(n object, pods []object) (float64, error) {
	if len(pods) == 0 {
		return 0, errors.New("the worker's memory requests cannot be proven from an empty Pod inventory")
	}
	allocatable, err := memoryBytes(text(n, "status", "allocatable", "memory"))
	if err != nil {
		return 0, err
	}
	var requested float64
	name := text(n, "metadata", "name")
	for _, pod := range pods {
		phase := text(pod, "status", "phase")
		if text(pod, "spec", "nodeName") != name || phase == "Succeeded" || phase == "Failed" {
			continue
		}
		request, requestErr := podMemoryRequest(pod)
		if requestErr != nil {
			return 0, requestErr
		}
		requested += request
	}
	// Only the pages kubelet does not count yet still shrink allocatable.
	counted := 0.0
	if pages := text(n, "status", "allocatable", "hugepages-2Mi"); pages != "" {
		if counted, err = memoryBytes(pages); err != nil {
			return 0, err
		}
	}
	return requested - (allocatable - max(0, 2*(1<<30)-counted)), nil
}

// verifyRefreshFits holds a kubelet restart that would shrink allocatable memory
// below what the worker's Pods already request. A restarting kubelet admits its Pods
// again, and one that no longer fits is rejected outright, past any disruption budget.
func verifyRefreshFits(n object, pods []object) error {
	excess, err := refreshExcess(n, pods)
	if err != nil {
		return err
	}
	if excess > 0 {
		return errors.New("worker " + text(n, "metadata", "name") + " requests more memory than remains after huge pages")
	}
	return nil
}
