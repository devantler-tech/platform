package main

import (
	"reflect"
	"testing"
)

// The stock manager requests 128Mi ordinary memory and 2048Mi
// hugepages; native LimitRanger adds the four-core CPU limit. With a
// 6% reservation, 8000m allocatable gives a 480m CPU request.
func nativeRestoreV2ManagerResourcesFixture() (nativeRestoreTarget, object, object, object, object, []object, []object) {
	target, storage, manager, pod := nativeRestoreV2ManagerIdentityFixture()
	_, nodes, _ := nativeRestoreTargetsFixture()
	node := nodes[1]
	valueObject(node, "status", "allocatable")["cpu"] = "8"
	container := value(pod, "spec", "containers").([]any)[0].(object)
	container["resources"] = object{"requests": object{"cpu": "480m", "memory": "128Mi", "hugepages-2Mi": "2Gi"}, "limits": object{"cpu": "4", "hugepages-2Mi": "2Gi"}}
	settings := []object{}
	for _, entry := range [][2]string{{"guaranteed-instance-manager-cpu", `{"v1":"6","v2":"6"}`}, {"data-engine-number-of-cpu-cores", `{"v2":"0"}`}, {"data-engine-memory-size", `{"v2":"2048"}`}, {"data-engine-hugepage-enabled", `{"v2":"true"}`}} {
		settings = append(settings, object{"apiVersion": "longhorn.io/v1beta2", "kind": "Setting", "metadata": object{"name": entry[0], "namespace": "longhorn-system", "uid": "setting-" + entry[0], "resourceVersion": "3"}, "value": entry[1]})
	}
	limits := []object{{"apiVersion": "v1", "kind": "LimitRange", "metadata": object{"name": "default-limitrange", "namespace": "longhorn-system", "uid": "limitrange-uid", "resourceVersion": "4"},
		"spec": object{"limits": []any{object{"type": "Container", "default": object{"cpu": "4"}, "defaultRequest": object{"cpu": "15m", "memory": "128Mi"}}}}}}
	return target, node, storage, manager, pod, settings, limits
}

func TestNativeRestoreV2ManagerResourcesMeasureProducerAndAdmission(t *testing.T) {
	target, node, storage, manager, pod, settings, limits := nativeRestoreV2ManagerResourcesFixture()
	got, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil)
	if err != nil || got.CPUMilli != 480 || got.MemoryBytes != 134217728 || got.HugepageBytes != 2147483648 || got.Identity.PodUID != "v2-pod-uid" {
		t.Fatal("native producer/admission resources were not measured:", got, err)
	}
	// Equivalent native Quantity spellings must not invalidate the profile.
	resources := valueObject(value(pod, "spec", "containers").([]any)[0].(object), "resources")
	valueObject(resources, "requests")["memory"] = "134217728"
	valueObject(resources, "requests")["hugepages-2Mi"] = "2048Mi"
	valueObject(resources, "limits")["cpu"] = "4000m"
	// Native allocation fields are optional and may report only some resources.
	// A zero retained request is safe because the declared request is counted.
	for _, status := range []object{valueObject(pod, "status"), value(pod, "status", "containerStatuses").([]any)[0].(object)} {
		status["resources"] = object{"requests": object{"memory": "0"}, "limits": object{"cpu": "4000m"}}
		status["allocatedResources"] = object{"cpu": "0", "hugepages-2Mi": "2048Mi"}
	}
	after, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil)
	if err != nil || !reflect.DeepEqual(got, after) {
		t.Fatal("equivalent admitted resource quantities changed the profile:", after, err)
	}
}

func TestNativeRestoreV2ManagerResourcesRoundTheNativeCPURequest(t *testing.T) {
	target, node, storage, manager, pod, settings, limits := nativeRestoreV2ManagerResourcesFixture()
	valueObject(node, "status", "allocatable")["cpu"] = "8025m"
	valueObject(value(pod, "spec", "containers").([]any)[0].(object), "resources", "requests")["cpu"] = "482m"
	got, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil)
	if err != nil || got.CPUMilli != 482 {
		t.Fatal("481.5m must round to 482m as the producer does:", got, err)
	}
}

func TestNativeRestoreV2ManagerResourcesAcceptCurrentProductionAdmission(t *testing.T) {
	target, node, storage, manager, pod, settings, limits := nativeRestoreV2ManagerResourcesFixture()
	settings[0]["value"] = `{"v1":"6","v2":"25"}`
	resources := valueObject(value(pod, "spec", "containers").([]any)[0].(object), "resources")
	valueObject(resources, "requests")["cpu"] = "2"
	valueObject(resources, "limits")["cpu"] = "4"
	valueObject(value(limits[0], "spec", "limits").([]any)[0].(object), "default")["cpu"] = "4"
	got, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil)
	if err != nil || got.CPUMilli != 2000 {
		t.Fatal("the declared 25% CPU reservation and four-core production limit were refused:", got, err)
	}
}

func TestNativeRestoreV2ManagerResourcesBindProcessObjectRevisions(t *testing.T) {
	target, node, storage, manager, pod, settings, limits := nativeRestoreV2ManagerResourcesFixture()
	before, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	valueObject(pod, "metadata")["resourceVersion"] = "10"
	after, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil)
	if err != nil || reflect.DeepEqual(before, after) {
		t.Fatal("a changed Pod revision was invisible in the resource receipt:", after, err)
	}
	valueObject(manager, "metadata")["resourceVersion"] = "11"
	rebound, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil)
	if err != nil || reflect.DeepEqual(after, rebound) {
		t.Fatal("a changed manager revision was invisible in the resource receipt:", rebound, err)
	}
}

func TestNativeRestoreV2ManagerResourcesRejectUnappliedLimits(t *testing.T) {
	for _, location := range []string{"pod", "container"} {
		for _, cpu := range []string{"0", "500m"} {
			t.Run(location+"/"+cpu, func(t *testing.T) {
				target, node, storage, manager, pod, settings, limits := nativeRestoreV2ManagerResourcesFixture()
				status := valueObject(pod, "status")
				if location == "container" {
					status = value(pod, "status", "containerStatuses").([]any)[0].(object)
				}
				status["resources"] = object{"limits": object{"cpu": cpu}}
				if got, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil); err == nil || got != (nativeRestoreV2ManagerResources{}) {
					t.Fatal("an unrestricted or unapplied CPU limit was accepted:", got, err)
				}
			})
		}
	}
}

func TestNativeRestoreV2ManagerResourcesRejectMalformedOtherEngine(t *testing.T) {
	target, node, storage, manager, pod, settings, limits := nativeRestoreV2ManagerResourcesFixture()
	settings[0]["value"] = `{"v1":null,"v2":"6"}`
	if got, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, nil); err == nil || got != (nativeRestoreV2ManagerResources{}) {
		t.Fatal("a malformed engine map was accepted:", got, err)
	}
}

// Each fault would otherwise let a changed native resource or admission
// contract masquerade as the approved profile before capacity accounting.
func TestNativeRestoreV2ManagerResourcesRejectUnqualifiedEvidence(t *testing.T) {
	for _, fault := range []string{"missing setting", "duplicate setting", "numeric setting", "duplicate json", "unknown engine", "dynamic cores", "memory setting", "hugepages disabled", "zero percent", "nonfinite percent", "setting identity", "setting deleting", "uid alias", "node uid", "node boot", "node deleting", "node cpu", "node override", "fractional override", "cpu request", "cpu limit", "memory request", "memory limit", "hugepage request", "hugepage limit", "extra resource", "pod resources", "overhead", "quota", "extra limitrange", "limitrange maximum", "limitrange memory limit", "limitrange request", "resize", "retained cpu", "retained memory", "retained hugepages", "unknown allocation", "allocated resources shape"} {
		t.Run(fault, func(t *testing.T) {
			target, node, storage, manager, pod, settings, limits := nativeRestoreV2ManagerResourcesFixture()
			resources := valueObject(value(pod, "spec", "containers").([]any)[0].(object), "resources")
			var quotas []object
			switch fault {
			case "missing setting":
				settings = settings[1:]
			case "duplicate setting":
				settings[1] = settings[0]
			case "numeric setting":
				settings[1]["value"] = `{"v2":0}`
			case "duplicate json":
				settings[1]["value"] = `{"v2":"1","v2":"0"}`
			case "unknown engine":
				settings[1]["value"] = `{"v2":"0","other":"0"}`
			case "dynamic cores":
				settings[1]["value"] = `{"v2":"2"}`
			case "memory setting":
				settings[2]["value"] = `{"v2":"1024"}`
			case "hugepages disabled":
				settings[3]["value"] = `{"v2":"false"}`
			case "zero percent":
				settings[0]["value"] = `{"v2":"0"}`
			case "nonfinite percent":
				settings[0]["value"] = `{"v2":"NaN"}`
			case "setting identity":
				valueObject(settings[0], "metadata")["namespace"] = "other"
			case "setting deleting":
				valueObject(settings[0], "metadata")["deletionTimestamp"] = "2026-11-16T12:00:00Z"
			case "uid alias":
				valueObject(settings[0], "metadata")["uid"] = target.UID
			case "node uid":
				valueObject(node, "metadata")["uid"] = "replacement"
			case "node boot":
				valueObject(node, "status", "nodeInfo")["bootID"] = "replacement"
			case "node deleting":
				valueObject(node, "metadata")["deletionTimestamp"] = "2026-11-16T12:00:00Z"
			case "node cpu":
				valueObject(node, "status", "allocatable")["cpu"] = "unknown"
			case "node override":
				valueObject(storage, "spec")["instanceManagerCPURequest"] = float64(480)
			case "fractional override":
				valueObject(storage, "spec")["instanceManagerCPURequest"] = 0.5
			case "cpu request":
				valueObject(resources, "requests")["cpu"] = "2"
			case "cpu limit":
				valueObject(resources, "limits")["cpu"] = "1"
			case "memory request":
				valueObject(resources, "requests")["memory"] = "64Mi"
			case "memory limit":
				valueObject(resources, "limits")["memory"] = "128Mi"
			case "hugepage request":
				valueObject(resources, "requests")["hugepages-2Mi"] = "1Gi"
			case "hugepage limit":
				valueObject(resources, "limits")["hugepages-2Mi"] = "1Gi"
			case "extra resource":
				valueObject(resources, "requests")["example.com/device"] = "1"
			case "pod resources":
				valueObject(pod, "spec")["resources"] = object{"requests": object{"memory": "128Mi"}}
			case "overhead":
				valueObject(pod, "spec")["overhead"] = object{"memory": "1Mi"}
			case "quota":
				quotas = []object{{}}
			case "extra limitrange":
				limits = append(limits, limits[0])
			case "limitrange maximum":
				value(limits[0], "spec", "limits").([]any)[0].(object)["max"] = object{"memory": "1Gi"}
			case "limitrange memory limit":
				valueObject(value(limits[0], "spec", "limits").([]any)[0].(object), "default")["memory"] = "128Mi"
			case "limitrange request":
				valueObject(value(limits[0], "spec", "limits").([]any)[0].(object), "defaultRequest")["memory"] = "64Mi"
			case "resize":
				valueObject(pod, "status")["resize"] = "InProgress"
			case "retained cpu":
				valueObject(pod, "status")["allocatedResources"] = object{"cpu": "481m"}
			case "retained memory":
				valueObject(pod, "status")["allocatedResources"] = object{"memory": "129Mi"}
			case "retained hugepages":
				value(pod, "status", "containerStatuses").([]any)[0].(object)["allocatedResources"] = object{"hugepages-2Mi": "3Gi"}
			case "unknown allocation":
				valueObject(pod, "status")["allocatedResources"] = object{"example.com/device": "1"}
			case "allocated resources shape":
				valueObject(pod, "status")["resources"] = "unknown"
			}
			got, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, settings, limits, quotas)
			if err == nil || got != (nativeRestoreV2ManagerResources{}) {
				t.Fatal("unqualified V2 resource evidence was accepted:", fault, got, err)
			}
		})
	}
}
