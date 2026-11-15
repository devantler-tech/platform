package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	kresource "k8s.io/apimachinery/pkg/api/resource"
)

const (
	talosReadLimit    = 30 * time.Second
	talosRestartLimit = 2 * time.Minute
)

func (c client) talosCommand(ctx context.Context, limit time.Duration, address string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return c.run(ctx, nil, "talosctl", append([]string{"--nodes=" + address}, args...)...)
}

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
	get := func(ctx context.Context) (object, string, error) {
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
	n, address, err := get(ctx)
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
	pages, err := c.talosCommand(ctx, talosReadLimit, address, "read", "/proc/sys/vm/nr_hugepages")
	if err != nil || strings.TrimSpace(string(pages)) != "1024" {
		return errors.New("the host has not allocated the reviewed huge pages")
	}
	// /proc/modules omits built-in modules, and nvme_tcp is built into the Talos kernel.
	// /sys/module holds both kinds, so it answers whether each one is usable.
	for _, module := range []string{"nvme_tcp", "vfio_pci", "uio_pci_generic"} {
		if _, err := c.talosCommand(ctx, talosReadLimit, address, "ls", "/sys/module/"+module); err != nil {
			return errors.New("required V2 kernel module " + module + " is not available")
		}
	}
	if hugepagesReady(n) {
		return nil
	}
	// A restart clears no pressure, and readiness could never be proven under it, so
	// hold before any Pod moves.
	if !workerPressureClear(n) {
		return errors.New("worker " + text(n, "metadata", "name") + " reports resource pressure; a kubelet refresh would not clear it")
	}
	moves, err := planRefreshMoves(n, pods)
	if err != nil {
		return err
	}
	if len(moves) > 0 {
		if err = c.moveForRefresh(ctx, source, n, moves, pods, &held); err != nil {
			return err
		}
	}
	if n, _, err = get(ctx); err != nil {
		return err
	}
	if !workerPressureClear(n) {
		return errors.New("worker " + text(n, "metadata", "name") + " reports resource pressure; a kubelet refresh would not clear it")
	}
	// Pods can land after the first measurement, with or without a cordon.
	before, err := c.list(ctx, "pods", "*")
	if err != nil {
		return err
	}
	if err = verifyRefreshFits(n, before); err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	// Never repeat an uncertain service request. Positive capacity and identity
	// readback, rather than its return code, establishes completion.
	restarted = true
	_, _ = c.talosCommand(ctx, talosRestartLimit, address, "service", "kubelet", "restart")
	if err = waitReady(ctx, func(ctx context.Context) error {
		n, _, err := get(ctx)
		if err != nil || !hugepagesReady(n) {
			return errors.New("kubelet has not reported the required huge-page capacity")
		}
		return nil
	}); err != nil {
		return err
	}
	if err = c.watchRestartedPods(ctx, j.Target, before); err != nil {
		return err
	}
	if held {
		return c.releaseRefreshCordon(ctx, j.Target)
	}
	return nil
}

// refreshSettle is how long Pods are watched after kubelet reports its new capacity.
// Node status and Pod admission are separate kubelet paths, so capacity alone proves
// nothing about Pods.
var refreshSettle = 2 * time.Minute

func (c client) watchRestartedPods(ctx context.Context, name string, before []object) error {
	settled := time.Now().Add(refreshSettle)
	for {
		after, err := c.list(ctx, "pods", "*")
		if err != nil {
			return err
		}
		if err = verifyRestartKeptPods(name, before, after); err != nil {
			return err
		}
		if !time.Now().Before(settled) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readinessPoll):
		}
	}
}

// verifyRestartKeptPods fails when a Pod running before the restart was rejected or is
// gone. A controller can delete a rejected Pod, so a missing one is not proof of health;
// ordinary churn can also trip this, and the next dispatch passes once capacity is proven.
func verifyRestartKeptPods(name string, before, after []object) error {
	if len(after) == 0 {
		return errors.New("the Pods kept by a kubelet restart cannot be proven from an empty inventory")
	}
	failed := map[string]bool{}
	for _, pod := range before {
		if text(pod, "status", "phase") == "Failed" {
			failed[text(pod, "metadata", "uid")] = true
		}
		if text(pod, "spec", "nodeName") == name && text(pod, "status", "phase") == "Running" && !podListed(after, text(pod, "metadata", "uid")) {
			return errors.New("a Pod running on " + name + " before its restart is gone")
		}
	}
	for _, pod := range after {
		reason := text(pod, "status", "reason")
		if text(pod, "spec", "nodeName") != name || text(pod, "status", "phase") != "Failed" || failed[text(pod, "metadata", "uid")] {
			continue
		}
		if strings.HasPrefix(reason, "OutOf") || reason == "UnexpectedAdmissionError" {
			return errors.New("kubelet rejected a Pod on " + name + " after its restart")
		}
	}
	return nil
}

// releaseProvenRefreshCordons runs ahead of the roster and volume checks. Once kubelet reports the
// smaller allocatable the cordon protects nothing, so an unrelated hold must not keep it.
func (c client) releaseProvenRefreshCordons(ctx context.Context, nodes []object) error {
	for _, n := range nodes {
		name := text(n, "metadata", "name")
		if value(n, "spec", "unschedulable") == true && text(n, "metadata", "annotations", fenceAnnotation) == refreshFencePrefix+name && hugepagesReady(n) {
			if err := c.releaseRefreshCordon(ctx, name); err != nil {
				return err
			}
		}
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
	if err = c.releaseProvenRefreshCordons(ctx, s.nodes); err != nil {
		return err
	}
	if err = verifyCapacityRoster(s.nodes, pools, false); err != nil {
		return err
	}
	// A worker this procedure left cordoned goes first, so a hold on another cannot strand it.
	sort.SliceStable(s.nodes, func(i, j int) bool {
		return text(s.nodes[i], "metadata", "annotations", fenceAnnotation) != "" && text(s.nodes[j], "metadata", "annotations", fenceAnnotation) == ""
	})
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

func memoryRequestQuantity(raw string) (kresource.Quantity, error) {
	q, err := kresource.ParseQuantity(raw)
	if err != nil || q.Sign() < 0 || q.Cmp(*kresource.NewQuantity(1<<52, kresource.BinarySI)) > 0 {
		return kresource.Quantity{}, errors.New("memory request cannot be measured exactly")
	}
	return q, nil
}

func containerMemoryRequest(raw any) (kresource.Quantity, error) {
	container, _ := raw.(map[string]any)
	request := text(container, "resources", "requests", "memory")
	if request == "" {
		return kresource.Quantity{}, nil
	}
	return memoryRequestQuantity(request)
}

// Keep exact quantities through the Pod-level or peak init/sidecar calculation,
// then round the effective Pod request to whole bytes as the scheduler does.
// Count retained Pod aggregate allocations as well as declared requests.
// Container status must remain covered by its corresponding declared request.
func podMemoryRequest(pod object) (float64, error) {
	if podAllocationUnqualified(pod) {
		return 0, errors.New("native resource allocation status is unqualified")
	}
	result, err := podDeclaredMemoryRequest(pod)
	if err != nil {
		return 0, err
	}
	for _, requests := range []any{value(pod, "status", "allocatedResources"), value(pod, "status", "resources", "requests")} {
		list, _ := requests.(object)
		if raw := text(list, "memory"); raw != "" {
			allocated, err := memoryRequestQuantity(raw)
			if err != nil {
				return 0, err
			}
			if overhead := text(pod, "spec", "overhead", "memory"); overhead != "" {
				extra, err := memoryRequestQuantity(overhead)
				if err != nil {
					return 0, err
				}
				allocated.Add(extra)
			}
			if allocated.Cmp(*kresource.NewQuantity(1<<52, kresource.BinarySI)) > 0 {
				return 0, errors.New("effective allocation exceeds exact accounting")
			}
			result = max(result, float64(allocated.Value()))
		}
	}
	return result, nil
}

func podDeclaredMemoryRequest(pod object) (float64, error) {
	var total, sidecars, peakInit kresource.Quantity
	maximum := func(a, b kresource.Quantity) kresource.Quantity {
		if a.Cmp(b) < 0 {
			return b.DeepCopy()
		}
		return a.DeepCopy()
	}
	if request := text(pod, "spec", "resources", "requests", "memory"); request != "" {
		var err error
		if total, err = memoryRequestQuantity(request); err != nil {
			return 0, err
		}
	} else {
		containers, _ := value(pod, "spec", "containers").([]any)
		for _, raw := range containers {
			request, err := containerMemoryRequest(raw)
			if err != nil {
				return 0, err
			}
			total.Add(request)
		}
		initContainers, _ := value(pod, "spec", "initContainers").([]any)
		for _, raw := range initContainers {
			request, err := containerMemoryRequest(raw)
			if err != nil {
				return 0, err
			}
			container, _ := raw.(map[string]any)
			if text(container, "restartPolicy") == "Always" {
				sidecars.Add(request)
				peakInit = maximum(peakInit, sidecars)
				continue
			}
			candidate := sidecars.DeepCopy()
			candidate.Add(request)
			peakInit = maximum(peakInit, candidate)
		}
		total.Add(sidecars)
		total = maximum(total, peakInit)
	}
	if overhead := text(pod, "spec", "overhead", "memory"); overhead != "" {
		extra, err := memoryRequestQuantity(overhead)
		if err != nil {
			return 0, err
		}
		total.Add(extra)
	}
	if total.Cmp(*kresource.NewQuantity(1<<52, kresource.BinarySI)) > 0 {
		return 0, errors.New("effective memory request exceeds exact accounting")
	}
	return float64(total.Value()), nil
}

func podAllocationUnqualified(pod object) bool {
	status, _ := value(pod, "status").(object)
	spec, _ := value(pod, "spec").(object)
	cpuCap := kresource.NewMilliQuantity(1<<40, kresource.DecimalSI)
	memoryCap := kresource.NewQuantity(1<<52, kresource.BinarySI)
	bound := object{"resources": object{"requests": object{"cpu": cpuCap.String(), "memory": memoryCap.String()}}}
	if !allocationWithinBound(bound, status) || text(status, "resize") != "" {
		return true
	}
	if claims := status["nodeAllocatableResourceClaimStatuses"]; claims != nil {
		list, ok := claims.([]any)
		if !ok || len(list) != 0 {
			return true
		}
	}
	conditions, _ := status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(object)
		if (text(condition, "type") == "PodResizePending" || text(condition, "type") == "PodResizeInProgress") && text(condition, "status") == "True" {
			return true
		}
	}
	for group, field := range map[string]string{"containerStatuses": "containers", "initContainerStatuses": "initContainers", "ephemeralContainerStatuses": "ephemeralContainers"} {
		entries, ok := status[group].([]any)
		if !ok && status[group] != nil {
			return true
		}
		if len(entries) == 0 {
			continue
		}
		containers, _ := spec[field].([]any)
		declared, seen := map[string]object{}, map[string]bool{}
		for _, raw := range containers {
			container, _ := raw.(object)
			name := text(container, "name")
			if declared[name] != nil {
				return true
			}
			declared[name] = container
		}
		for _, raw := range entries {
			entry, ok := raw.(object)
			if !ok {
				return true
			}
			if entry["allocatedResources"] != nil || entry["resources"] != nil {
				name := text(entry, "name")
				if name == "" || seen[name] || declared[name] == nil || !allocationWithinBound(declared[name], entry) {
					return true
				}
				seen[name] = true
			}
		}
	}
	return false
}

// Compare native CPU/RAM observations with a supplied request bound. Container
// budgets require declared coverage; Pod aggregates use numeric bounds here
// and are counted separately in the effective memory and CPU budgets.
func allocationWithinBound(spec, status object) bool {
	if !runtimeRequestShape(status["resources"]) {
		return false
	}
	declared := value(spec, "resources", "requests")
	for _, observed := range []any{status["allocatedResources"], value(status, "resources", "requests")} {
		if observed == nil {
			continue
		}
		if !runtimeResourceListShape(declared) || !runtimeResourceListShape(observed) {
			return false
		}
		requests, _ := observed.(object)
		for _, name := range []string{"cpu", "memory"} {
			if requests[name] == nil {
				continue
			}
			actual, err := kresource.ParseQuantity(text(requests, name))
			bound := kresource.Quantity{}
			if raw := text(object{"requests": declared}, "requests", name); raw != "" {
				var boundErr error
				bound, boundErr = kresource.ParseQuantity(raw)
				if boundErr != nil {
					return false
				}
			}
			if err != nil || actual.Sign() < 0 || bound.Sign() < 0 || actual.Cmp(bound) > 0 {
				return false
			}
		}
	}
	return true
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
