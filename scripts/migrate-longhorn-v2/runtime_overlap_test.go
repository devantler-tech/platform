package main

import (
	"strings"
	"testing"
)

func runtimeOverlapFixtureFault(index int, item object, fault string) {
	if index == 0 {
		allocatable := value(item, "status", "allocatable").(object)
		switch fault {
		case "overlap rounding":
			allocatable["cpu"] = "25m"
		case "overlap peak init", "overlap sidecar", "overlap Pod request", "overlap overhead", "overlap fractional bytes":
			allocatable["memory"] = "1Gi"
		case "overlap retained Pod enactment":
			allocatable["memory"] = "255Mi"
		case "overlap aggregate retains overhead":
			allocatable["memory"] = "257Mi"
		case "overlap invalid node memory":
			allocatable["memory"] = "NaN"
		}
	}
	if index == 1 {
		spec := value(item, "spec").(object)
		request := value(spec["containers"].([]any)[0].(object), "resources", "requests").(object)
		init := func(memory string, sidecar bool) object {
			container := object{"resources": object{"requests": object{"memory": memory}}}
			if sidecar {
				container["restartPolicy"] = "Always"
			}
			return container
		}
		switch fault {
		case "overlap CPU full":
			request["cpu"] = "7900m"
		case "overlap memory full":
			request["memory"] = "12Gi"
		case "overlap rounding":
			request["cpu"] = "24m"
		case "overlap override":
			request["cpu"] = "7000m"
		case "overlap peak init":
			request["memory"] = "256Mi"
			spec["initContainers"] = []any{init("512Mi", false), init("512Mi", false)}
		case "overlap sidecar":
			request["memory"] = "768Mi"
			spec["initContainers"] = []any{init("384Mi", true), init("128Mi", false)}
		case "overlap Pod request":
			spec["resources"] = object{"requests": object{"memory": "1Gi"}}
		case "overlap overhead":
			request["memory"] = "896Mi"
			spec["overhead"] = object{"memory": "1Mi"}
		case "overlap terminal":
			value(item, "status").(object)["phase"] = "Succeeded"
			request["memory"], request["cpu"] = "1Ti", "128"
		case "overlap invalid Pod memory":
			request["memory"] = "NaN"
		case "overlap invalid CPU":
			request["cpu"] = "999999999999999999999999999999999999999999999999"
		case "overlap numeric CPU":
			request["cpu"] = float64(500)
		case "overlap numeric memory":
			request["memory"] = float64(123)
		case "overlap malformed requests":
			value(spec["containers"].([]any)[0].(object), "resources").(object)["requests"] = []any{}
		case "overlap fractional bytes":
			request["memory"] = "939524095100m"
		case "overlap Pod allocated":
			value(item, "status").(object)["allocatedResources"] = object{"memory": "20Gi"}
		case "overlap Pod enacted":
			value(item, "status").(object)["resources"] = object{"requests": object{"memory": "20Gi"}}
		case "overlap Pod CPU allocated":
			value(item, "status").(object)["allocatedResources"] = object{"cpu": "7900m"}
		case "overlap container allocated":
			value(item, "status", "containerStatuses").([]any)[0].(object)["allocatedResources"] = object{"memory": "20Gi"}
		case "overlap container enacted":
			value(item, "status", "containerStatuses").([]any)[0].(object)["resources"] = object{"requests": object{"memory": "20Gi"}}
		case "overlap stable allocation":
			container := value(item, "status", "containerStatuses").([]any)[0].(object)
			container["allocatedResources"] = object{"cpu": "480m", "memory": "128Mi"}
			container["resources"] = object{"requests": object{"cpu": "480m", "memory": "128Mi"}}
		case "overlap smaller allocation":
			container := value(item, "status", "containerStatuses").([]any)[0].(object)
			container["allocatedResources"] = object{"cpu": "100m", "memory": "64Mi"}
			container["resources"] = object{"requests": object{"cpu": "100m", "memory": "64Mi"}}
		case "overlap retained CPU":
			value(item, "status", "containerStatuses").([]any)[0].(object)["allocatedResources"] = object{"cpu": "7900m"}
		case "overlap allocation unknown container":
			container := value(item, "status", "containerStatuses").([]any)[0].(object)
			container["name"] = "foreign"
			container["allocatedResources"] = object{"memory": "1Mi"}
		case "overlap malformed allocation":
			value(item, "status", "containerStatuses").([]any)[0].(object)["allocatedResources"] = object{"memory": float64(1024)}
		case "overlap stable Pod allocation":
			spec["resources"] = object{"requests": object{"cpu": "480m", "memory": "128Mi"}}
			status := value(item, "status").(object)
			status["allocatedResources"] = object{"cpu": "480m", "memory": "128Mi"}
			status["resources"] = object{"requests": object{"cpu": "480m", "memory": "128Mi"}}
		case "overlap aggregate Pod allocation":
			status := value(item, "status").(object)
			status["allocatedResources"] = object{"cpu": "480m", "memory": "128Mi"}
			status["resources"] = object{"requests": object{"cpu": "480m", "memory": "128Mi"}}
		case "overlap aggregate retains overhead":
			spec["overhead"] = object{"memory": "1Mi"}
			value(item, "status").(object)["allocatedResources"] = object{"memory": "129Mi"}
		case "overlap retained Pod enactment":
			request["memory"] = "100Mi"
			status := value(item, "status").(object)
			status["allocatedResources"] = object{"memory": "100Mi"}
			status["resources"] = object{"requests": object{"memory": "128Mi"}}
		}
	}
	if index == 2 {
		spec := value(item, "spec").(object)
		switch fault {
		case "overlap override":
			spec["instanceManagerCPURequest"] = float64(1500)
		case "overlap zero override":
			spec["instanceManagerCPURequest"] = float64(0)
		case "overlap malformed override":
			spec["instanceManagerCPURequest"] = "500"
		case "overlap fractional override":
			spec["instanceManagerCPURequest"] = float64(1.5)
		case "overlap negative override":
			spec["instanceManagerCPURequest"] = float64(-1)
		case "overlap missing node":
			value(item, "metadata").(object)["name"] = "unmatched-worker"
		}
	}
	if index == 10 && fault == "overlap already adopted" {
		item["value"] = runtimeRetirementImage
	}
	if index == 11 {
		switch fault {
		case "overlap missing percentage":
			item["value"] = `{"v2":"6"}`
		case "overlap duplicate percentage":
			item["value"] = `{"v1":"1","v1":"6","v2":"6"}`
		case "overlap zero percentage":
			item["value"] = `{"v1":"0","v2":"6"}`
		}
	}
	if index == 12 {
		rule := value(item, "spec", "limits").([]any)[0].(object)
		switch fault {
		case "overlap altered default":
			value(rule, "default").(object)["memory"] = "1Gi"
		case "overlap invalid default":
			value(rule, "defaultRequest").(object)["memory"] = "NaN"
		case "overlap CPU admission":
			value(rule, "default").(object)["cpu"] = "1m"
		}
	}
	if index == 13 {
		item["spec"] = object{"hard": object{"requests.cpu": "8"}}
	}
}

func TestRuntimeDeclaredOverlapMeasuresCapacityAndRefusesUnknownInputs(t *testing.T) {
	for _, test := range []struct{ fault, want string }{
		{"overlap", "FIT"}, {"overlap peak init", "FIT"}, {"overlap zero override", "FIT"}, {"overlap terminal", "FIT"},
		{"overlap CPU full", "HOLD"}, {"overlap memory full", "HOLD"}, {"overlap rounding", "HOLD"},
		{"overlap override", "HOLD"}, {"overlap sidecar", "HOLD"}, {"overlap Pod request", "HOLD"}, {"overlap overhead", "HOLD"},
		{"overlap fractional bytes", "HOLD"},
		{"overlap stable allocation", "FIT"}, {"overlap smaller allocation", "FIT"}, {"overlap stable Pod allocation", "FIT"},
		{"overlap aggregate Pod allocation", "FIT"},
		{"overlap malformed override", "UNVERIFIED"}, {"overlap fractional override", "UNVERIFIED"}, {"overlap negative override", "UNVERIFIED"},
		{"overlap missing percentage", "UNVERIFIED"}, {"overlap duplicate percentage", "UNVERIFIED"}, {"overlap zero percentage", "UNVERIFIED"},
		{"overlap missing node", "UNVERIFIED"}, {"overlap already adopted", "UNVERIFIED"}, {"overlap quota", "UNVERIFIED"},
		{"overlap invalid node memory", "UNVERIFIED"}, {"overlap invalid Pod memory", "UNVERIFIED"}, {"overlap invalid CPU", "UNVERIFIED"},
		{"overlap numeric CPU", "UNVERIFIED"}, {"overlap numeric memory", "UNVERIFIED"}, {"overlap malformed requests", "UNVERIFIED"},
		{"overlap altered default", "UNVERIFIED"}, {"overlap invalid default", "UNVERIFIED"}, {"overlap CPU admission", "UNVERIFIED"},
		{"overlap Pod allocated", "HOLD"}, {"overlap Pod enacted", "HOLD"}, {"overlap Pod CPU allocated", "HOLD"},
		{"overlap container allocated", "UNVERIFIED"}, {"overlap container enacted", "UNVERIFIED"},
		{"overlap retained CPU", "UNVERIFIED"}, {"overlap allocation unknown container", "UNVERIFIED"}, {"overlap malformed allocation", "UNVERIFIED"},
		{"overlap aggregate retains overhead", "HOLD"}, {"overlap retained Pod enactment", "HOLD"},
	} {
		t.Run(test.fault, func(t *testing.T) {
			c, _ := runtimeObservationFixture(t, test.fault)
			got, err := observeRuntimeInventory(t.Context(), c, strings.Repeat("a", 40))
			if err != nil || got.DeclaredOverlap != test.want || got.EpochSHA == "" ||
				(test.want != "UNVERIFIED" && got.OverlapNodes != 1) || (test.want == "UNVERIFIED" && got.OverlapNodes != 0) {
				t.Fatalf("resource evidence misclassified: got=%+v err=%v want=%s", got, err, test.want)
			}
		})
	}
}

func TestMemoryRequestRoundsTheEffectivePodAndRefusesUnknownAllocationStatus(t *testing.T) {
	pod := object{"spec": object{"containers": []any{
		object{"resources": object{"requests": object{"memory": "100m"}}},
		object{"resources": object{"requests": object{"memory": "100m"}}},
	}}}
	if got, err := podMemoryRequest(pod); err != nil || got != 1 {
		t.Fatalf("effective request must round after summing: got=%v err=%v", got, err)
	}
	for _, status := range []object{
		{"containerStatuses": []any{object{"name": "app", "resources": object{"requests": object{"memory": "1Gi"}}}}},
		{"initContainerStatuses": []any{object{"name": "init", "allocatedResources": object{"memory": "1Gi"}}}},
		{"conditions": []any{object{"type": "PodResizePending", "status": "True"}}},
		{"nodeAllocatableResourceClaimStatuses": []any{object{"resources": object{"memory": "1Gi"}}}},
	} {
		pod["status"] = status
		if _, err := podMemoryRequest(pod); err == nil {
			t.Fatal("unqualified retained allocation was treated as a spec request")
		}
	}
}
