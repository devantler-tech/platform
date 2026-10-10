package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDrainRequiresCurrentZeroListenerProcess(t *testing.T) {
	body := []byte(`{"HR":{"spec":{"suspend":false,"values":{"minRunners":0,"maxRunners":0}},"metadata":{"annotations":{"kustomize.toolkit.fluxcd.io/reconcile":"disabled"}}},"ARS":{"metadata":{"name":"platform-linux","namespace":"arc-runners","uid":"ars","generation":2,"annotations":{"runner-scale-set-id":"9"}},"spec":{"minRunners":0,"maxRunners":0},"status":{"observedGeneration":2}},"Listeners":[{"metadata":{"name":"listener","namespace":"arc-systems","uid":"listener-2"},"spec":{"autoscalingRunnerSetName":"platform-linux","autoscalingRunnerSetNamespace":"arc-runners","ephemeralRunnerSetName":"platform-linux","runnerScaleSetId":9},"status":{}}],"ERS":{"metadata":{"name":"platform-linux","namespace":"arc-runners","uid":"ers","annotations":{"actions.github.com/autoscaling-runner-set-generation":"2"},"ownerReferences":[{"kind":"AutoscalingRunnerSet","uid":"ars","controller":true}]},"spec":{"replicas":0},"status":{"phase":"Running"}},"ListenerPods":[{"uid":"pod-2","ownerKind":"AutoscalingListener","ownerUID":"listener-2","phase":"Running","ready":"True","deleting":""}],"ChildrenAbsent":true,"NodesAbsent":true}`)
	if err := drainProof(body); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"listener-owner", "extra-old-pod", "busy", "nonzero", "omitted-maximum", "stale-generation", "stale-ers", "not-ready", "deleting", "ambiguous"} {
		var input map[string]any
		_ = json.Unmarshal(body, &input)
		ars := input["ARS"].(map[string]any)
		pods := input["ListenerPods"].([]any)
		switch change {
		case "listener-owner":
			pods[0].(map[string]any)["ownerUID"] = "listener-1"
		case "extra-old-pod":
			input["ListenerPods"] = append(pods, pods[0])
		case "busy":
			input["ChildrenAbsent"] = false
		case "nonzero":
			ars["spec"].(map[string]any)["maxRunners"] = 1
		case "omitted-maximum":
			delete(ars["spec"].(map[string]any), "maxRunners")
		case "stale-generation":
			ars["status"].(map[string]any)["observedGeneration"] = 1
		case "stale-ers":
			input["ERS"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)["actions.github.com/autoscaling-runner-set-generation"] = "1"
		case "not-ready":
			pods[0].(map[string]any)["ready"] = "False"
		case "deleting":
			pods[0].(map[string]any)["deleting"] = "now"
		}
		changed, _ := json.Marshal(input)
		if change == "ambiguous" {
			changed = []byte(strings.Replace(string(changed), `"NodesAbsent":true`, `"NodesAbsent":true,"nodesabsent":false`, 1))
		}
		if drainProof(changed) == nil {
			t.Fatal("accepted", change)
		}
	}
}

func TestPartialInstallationCannotInventAbsentListenerOrBusyChildren(t *testing.T) {
	body := []byte(`{"HR":{"spec":{"suspend":false,"values":{"minRunners":0,"maxRunners":0}},"metadata":{"annotations":{"kustomize.toolkit.fluxcd.io/reconcile":"disabled"}}},"ARS":null,"Listeners":[],"ERS":null,"ListenerPods":[],"ChildrenAbsent":true,"NodesAbsent":true}`)
	if err := drainProof(body); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{`"ChildrenAbsent":false`, `"ChildrenAbsent":null`} {
		if drainProof([]byte(strings.Replace(string(body), `"ChildrenAbsent":true`, change, 1))) == nil {
			t.Fatal("invented absence")
		}
	}
}
