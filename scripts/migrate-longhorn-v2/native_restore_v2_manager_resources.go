package main

import (
	"math"
	"strconv"

	kresource "k8s.io/apimachinery/pkg/api/resource"
)

type nativeRestoreV2ManagerResources struct {
	Identity                             nativeRestoreV2ManagerIdentity
	Node, LimitRange                     benchmarkResource
	Settings                             [4]benchmarkResource
	SettingValues                        [4]string
	Versions                             [6]string
	PodVersion, ManagerVersion           string
	CPUMilli, MemoryBytes, HugepageBytes int64
	NodeAllocatableCPUMilli              int64
}

// Qualify only the fixed, non-dynamic V2 producer resource profile and the
// namespace admission rules that explain its native envelope. This measures
// neither free capacity nor host memory, cohort, argv, mounts or security.
// All inventories must be complete and re-observed by the eventual caller.
func qualifyNativeRestoreV2ManagerResources(target nativeRestoreTarget, node, storage, manager, pod object, settings, limitRanges, quotas []object) (nativeRestoreV2ManagerResources, error) {
	fail := func() (nativeRestoreV2ManagerResources, error) {
		return nativeRestoreV2ManagerResources{}, errNativeRestoreEvidence
	}
	identity, err := qualifyNativeRestoreV2ManagerIdentity(target, storage, manager, pod)
	if err != nil || !nativeRestoreV2ResourceIdentity(node, "v1", "Node", "") || text(node, "metadata", "name") != target.Name ||
		text(node, "metadata", "uid") != target.UID || text(node, "status", "nodeInfo", "bootID") != target.Preparation.BootID || len(settings) != 4 || len(limitRanges) != 1 || len(quotas) != 0 {
		return fail()
	}
	if override := value(storage, "spec", "instanceManagerCPURequest"); override != nil && override != float64(0) {
		return fail()
	}
	cpu, err := runtimeMilliCPU(text(node, "status", "allocatable", "cpu"))
	if err != nil || cpu <= 0 || cpu > 128000 {
		return fail()
	}
	result := nativeRestoreV2ManagerResources{Identity: identity, Node: benchmarkIdentity(node), NodeAllocatableCPUMilli: cpu, PodVersion: text(pod, "metadata", "resourceVersion"), ManagerVersion: text(manager, "metadata", "resourceVersion")}
	result.Versions[0] = text(node, "metadata", "resourceVersion")
	uids := map[string]bool{target.UID: true, target.Preparation.LonghornUID: true, target.Preparation.Journal.UID: true, identity.ManagerUID: true, identity.PodUID: true}
	names := [4]string{"guaranteed-instance-manager-cpu", "data-engine-number-of-cpu-cores", "data-engine-memory-size", "data-engine-hugepage-enabled"}
	values := [4]string{}
	for _, setting := range settings {
		if !nativeRestoreV2ResourceIdentity(setting, "longhorn.io/v1beta2", "Setting", "longhorn-system") || uids[text(setting, "metadata", "uid")] {
			return fail()
		}
		index := -1
		for i, name := range names {
			if text(setting, "metadata", "name") == name {
				index = i
			}
		}
		if index < 0 || result.Settings[index].UID != "" {
			return fail()
		}
		raw := text(setting, "value")
		configured, err := nativeRestoreV2SettingValue(raw)
		if err != nil || configured == "" {
			return fail()
		}
		values[index], result.SettingValues[index] = configured, raw
		result.Settings[index], result.Versions[index+1] = benchmarkIdentity(setting), text(setting, "metadata", "resourceVersion")
		uids[text(setting, "metadata", "uid")] = true
	}
	percentage, err := strconv.ParseFloat(values[0], 64)
	if err != nil || math.IsNaN(percentage) || math.IsInf(percentage, 0) || percentage <= 0 || percentage > 100 || values[1] != "0" || values[2] != "2048" || values[3] != "true" {
		return fail()
	}
	result.CPUMilli, result.MemoryBytes, result.HugepageBytes = int64(math.Round(float64(cpu)*percentage/100)), 128*1024*1024, 2048*1024*1024
	if result.CPUMilli <= 0 || result.CPUMilli > 2000 {
		return fail()
	}
	limit := limitRanges[0]
	if !nativeRestoreV2ResourceIdentity(limit, "v1", "LimitRange", "longhorn-system") || text(limit, "metadata", "name") != "default-limitrange" || uids[text(limit, "metadata", "uid")] {
		return fail()
	}
	rules, ok := value(limit, "spec", "limits").([]any)
	if !ok || len(rules) != 1 {
		return fail()
	}
	rule, ok := rules[0].(object)
	if !ok || !runtimeOnlyKeys(rule, "type", "default", "defaultRequest") || text(rule, "type") != "Container" ||
		!nativeRestoreV2ResourceEqual(rule["default"], object{"cpu": "2"}) || !nativeRestoreV2ResourceEqual(rule["defaultRequest"], object{"cpu": "15m", "memory": "128Mi"}) {
		return fail()
	}
	result.LimitRange, result.Versions[5] = benchmarkIdentity(limit), text(limit, "metadata", "resourceVersion")
	container := value(pod, "spec", "containers").([]any)[0].(object)
	resources, ok := container["resources"].(object)
	requests := object{"cpu": strconv.FormatInt(result.CPUMilli, 10) + "m", "memory": "128Mi", "hugepages-2Mi": "2Gi"}
	limits := object{"cpu": "2", "hugepages-2Mi": "2Gi"}
	if !ok || !runtimeOnlyKeys(resources, "requests", "limits") || !nativeRestoreV2ResourceEqual(resources["requests"], requests) || !nativeRestoreV2ResourceEqual(resources["limits"], limits) ||
		value(pod, "spec", "resources") != nil || value(pod, "spec", "overhead") != nil || podAllocationUnqualified(pod) {
		return fail()
	}
	for _, observed := range []object{valueObject(pod, "status"), value(pod, "status", "containerStatuses").([]any)[0].(object)} {
		if !nativeRestoreV2ResourceBound(observed["allocatedResources"], requests) {
			return fail()
		}
		if raw := observed["resources"]; raw != nil {
			native, ok := raw.(object)
			if !ok || !runtimeOnlyKeys(native, "requests", "limits") || !nativeRestoreV2ResourceBound(native["requests"], requests) || !nativeRestoreV2ObservedLimits(native["limits"], limits) {
				return fail()
			}
		}
	}
	return result, nil
}

// Zero is not an enforced CPU ceiling. Any reported applied limit must match
// the declared native limit; a smaller retained value is also unapplied state.
func nativeRestoreV2ObservedLimits(raw any, expected object) bool {
	if !nativeRestoreV2ResourceBound(raw, expected) {
		return false
	}
	actual, _ := raw.(object)
	for name, quantity := range actual {
		if !nativeRestoreV2ResourceEqual(object{name: quantity}, object{name: expected[name]}) {
			return false
		}
	}
	return true
}

func nativeRestoreV2ResourceIdentity(o object, api, kind, namespace string) bool {
	return text(o, "apiVersion") == api && text(o, "kind") == kind && text(o, "metadata", "namespace") == namespace && text(o, "metadata", "name") != "" &&
		nativeRestoreUID(text(o, "metadata", "uid")) && text(o, "metadata", "resourceVersion") != "" && value(o, "metadata", "deletionTimestamp") == nil
}

func nativeRestoreV2ResourceEqual(raw any, expected object) bool {
	actual, ok := raw.(object)
	return ok && len(actual) == len(expected) && nativeRestoreV2ResourceBound(actual, expected) && nativeRestoreV2ResourceBound(expected, actual)
}

func nativeRestoreV2ResourceBound(raw any, expected object) bool {
	if raw == nil {
		return true
	}
	actual, ok := raw.(object)
	if !ok {
		return false
	}
	for name, raw := range actual {
		quantity, ok := raw.(string)
		bound, exists := expected[name].(string)
		if !ok || !exists || quantity == "" || bound == "" {
			return false
		}
		n, err := kresource.ParseQuantity(quantity)
		maximum, boundErr := kresource.ParseQuantity(bound)
		if err != nil || boundErr != nil || n.Sign() < 0 || n.Cmp(maximum) > 0 {
			return false
		}
	}
	return true
}
