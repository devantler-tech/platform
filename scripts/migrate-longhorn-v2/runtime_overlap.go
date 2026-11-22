package main

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	kresource "k8s.io/apimachinery/pkg/api/resource"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// This is a sufficient declared CPU/RAM request budget for one additional V1
// manager on every storage node. It neither reserves capacity nor measures
// resident memory, admission, serving health, rollout or retirement authority.
func runtimeDeclaredOverlap(records []object, image string) (int, string) {
	unknown := func() (int, string) { return 0, "UNVERIFIED" }
	if image != "stock" {
		return unknown()
	}
	kube, storage := map[string]object{}, map[string]object{}
	var pods, limits []object
	percentage := 0
	for _, record := range records {
		identity := text(record, "identity")
		switch {
		case strings.HasPrefix(identity, "v1/Node//"):
			kube[strings.TrimPrefix(identity, "v1/Node//")] = record
		case strings.HasPrefix(identity, "longhorn.io/v1beta2/Node/longhorn-system/"):
			storage[strings.TrimPrefix(identity, "longhorn.io/v1beta2/Node/longhorn-system/")] = record
		case strings.HasPrefix(identity, "v1/Pod/"):
			pods = append(pods, record)
		case strings.HasPrefix(identity, "v1/LimitRange/longhorn-system/"):
			limits = append(limits, record)
		case strings.HasPrefix(identity, "v1/ResourceQuota/longhorn-system/"):
			// Namespace quota needs its own complete usage/admission budget.
			return unknown()
		case identity == "longhorn.io/v1beta2/Setting/longhorn-system/guaranteed-instance-manager-cpu":
			raw := []byte(text(record, "value"))
			var configured map[string]string
			if strictjson.Validate(raw, map[string][]string{"": {"v1", "v2"}}) != nil || json.Unmarshal(raw, &configured) != nil {
				return unknown()
			}
			var err error
			percentage, err = strconv.Atoi(configured["v1"])
			if err != nil || percentage <= 0 || percentage > 100 {
				return unknown()
			}
		}
	}
	if len(storage) == 0 || len(pods) == 0 || len(limits) != 1 || percentage == 0 {
		return unknown()
	}
	rules, ok := value(limits[0], "spec", "limits").([]any)
	if !ok || len(rules) != 1 {
		return unknown()
	}
	rule, ok := rules[0].(object)
	if !ok || text(rule, "type") != "Container" || rule["min"] != nil || rule["max"] != nil || rule["maxLimitRequestRatio"] != nil || value(rule, "default", "memory") != nil {
		return unknown()
	}
	extraQuantity, err := memoryRequestQuantity(text(rule, "defaultRequest", "memory"))
	extraMemory := float64(extraQuantity.Value())
	cpuLimit, cpuErr := runtimeMilliCPU(text(rule, "default", "cpu"))
	if err != nil || cpuErr != nil || !runtimeFiniteMemory(extraMemory) || extraMemory <= 0 {
		return unknown()
	}
	fits := true
	for name, node := range storage {
		if _, ok := node["spec"].(object); !ok {
			return unknown()
		}
		n := kube[name]
		cpu, err := runtimeMilliCPU(text(n, "allocatable", "cpu"))
		memoryQuantity, memoryErr := memoryRequestQuantity(text(n, "allocatable", "memory"))
		memory := float64(memoryQuantity.Value())
		if err != nil || memoryErr != nil || cpu <= 0 || cpu > 128000 || !runtimeFiniteMemory(memory) || memory <= 0 {
			return unknown()
		}
		extraCPU := int64(math.Round(float64(cpu) * float64(percentage) / 100))
		if override := value(node, "spec", "instanceManagerCPURequest"); override != nil {
			number, valid := override.(float64)
			if !valid || number < 0 || number > 1<<40 || math.Trunc(number) != number {
				return unknown()
			}
			if number != 0 {
				extraCPU = int64(number)
			}
		}
		if extraCPU <= 0 || extraCPU > cpuLimit {
			return unknown()
		}
		requestedCPU, requestedMemory := int64(0), float64(0)
		for _, pod := range pods {
			if text(pod, "spec", "node") != name || text(pod, "phase") == "Succeeded" || text(pod, "phase") == "Failed" {
				continue
			}
			if pod["allocationUnqualified"] != false {
				return unknown()
			}
			spec, valid := value(pod, "spec").(object)
			if !valid {
				return unknown()
			}
			accounted := object{"spec": spec, "status": value(pod, "allocation")}
			podCPU, cpuErr := runtimePodCPURequestUpper(accounted)
			podMemory, memoryErr := podMemoryRequest(accounted)
			if cpuErr != nil || memoryErr != nil || !runtimeFiniteMemory(podMemory) || podCPU > 1<<40-requestedCPU {
				return unknown()
			}
			requestedCPU += podCPU
			requestedMemory += podMemory
		}
		fits = fits && requestedCPU+extraCPU <= cpu && requestedMemory+extraMemory <= memory
	}
	if !fits {
		return len(storage), "HOLD"
	}
	return len(storage), "FIT"
}

func runtimeFiniteMemory(n float64) bool { return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) }

func runtimeMilliCPU(raw string) (int64, error) {
	q, err := kresource.ParseQuantity(raw)
	if err != nil || q.Sign() < 0 || q.Cmp(*kresource.NewMilliQuantity(1<<40, kresource.DecimalSI)) > 0 {
		return 0, errors.New("CPU request cannot be measured")
	}
	return q.MilliValue(), nil
}

func runtimePodCPURequestUpper(pod object) (int64, error) {
	if podAllocationUnqualified(pod) {
		return 0, errors.New("native allocation is unqualified")
	}
	spec, _ := value(pod, "spec").(object)
	result, err := runtimePodCPUUpper(spec)
	if err != nil {
		return 0, err
	}
	var overhead int64
	if raw := text(spec, "overhead", "cpu"); raw != "" {
		overhead, err = runtimeMilliCPU(raw)
		if err != nil {
			return 0, err
		}
	}
	for _, requests := range []any{value(pod, "status", "allocatedResources"), value(pod, "status", "resources", "requests")} {
		list, _ := requests.(object)
		if raw := text(list, "cpu"); raw != "" {
			allocated, err := runtimeMilliCPU(raw)
			if err != nil || allocated > 1<<40-overhead {
				return 0, errors.New("CPU allocation cannot be measured")
			}
			result = max(result, allocated+overhead)
		}
	}
	return result, nil
}

// Summing all init CPU requests is conservative even with native sidecars.
// Memory uses the existing exact peak-init/sidecar accounting above.
func runtimePodCPUUpper(spec object) (int64, error) {
	if !runtimeRequestShape(spec["resources"]) || !runtimeResourceListShape(spec["overhead"]) {
		return 0, errors.New("pod resource requests are malformed")
	}
	var total int64
	for _, field := range []string{"containers", "initContainers"} {
		containers, ok := spec[field].([]any)
		if !ok && spec[field] != nil || field == "containers" && len(containers) == 0 {
			return 0, errors.New("container resource inventory is incomplete")
		}
		for _, raw := range containers {
			container, ok := raw.(object)
			if !ok || !runtimeRequestShape(container["resources"]) {
				return 0, errors.New("container resource inventory is malformed")
			}
			if request := text(container, "resources", "requests", "cpu"); request != "" {
				n, err := runtimeMilliCPU(request)
				if err != nil || n > 1<<40-total {
					return 0, errors.New("container CPU request cannot be measured")
				}
				total += n
			}
		}
	}
	for _, path := range [][]string{{"resources", "requests", "cpu"}, {"overhead", "cpu"}} {
		if request := text(spec, path...); request != "" {
			n, err := runtimeMilliCPU(request)
			if err != nil || n > 1<<40-total {
				return 0, errors.New("pod CPU request cannot be measured")
			}
			if path[0] == "resources" {
				total = max(total, n)
			} else {
				total += n
			}
		}
	}
	return total, nil
}

func runtimeRequestShape(raw any) bool {
	if raw == nil {
		return true
	}
	resources, ok := raw.(object)
	return ok && runtimeResourceListShape(resources["requests"])
}

func runtimeResourceListShape(raw any) bool {
	if raw == nil {
		return true
	}
	resources, ok := raw.(object)
	if !ok {
		return false
	}
	for _, name := range []string{"cpu", "memory"} {
		if raw, found := resources[name]; found {
			if quantity, ok := raw.(string); !ok || quantity == "" {
				return false
			}
		}
	}
	return true
}
