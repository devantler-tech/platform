package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const ingressAgentSample = `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"cilium-abcde","namespace":"kube-system","uid":"11111111-1111-1111-1111-111111111111","resourceVersion":"10","labels":{"k8s-app":"cilium"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"cilium","uid":"22222222-2222-2222-2222-222222222222","controller":true}]},"spec":{"nodeName":"worker-1","serviceAccountName":"cilium","hostNetwork":true,"containers":[{"name":"cilium-agent","image":"quay.io/cilium/cilium:v1.20.4@sha256:20fe92e4c86641233a450098c158d2d29085f3419931ab6bf681d2485dedf16e"}]},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","imageID":"quay.io/cilium/cilium@sha256:20fe92e4c86641233a450098c158d2d29085f3419931ab6bf681d2485dedf16e","containerID":"containerd://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-10-28T00:00:00Z"}}}]}}]}`

// An unrelated, replaced or unready privileged container must never become an
// execution target merely because it carries the controller's label.
func TestIngressAgentRequiresOwnedPinnedRunningContainer(t *testing.T) {
	typed := strings.Replace(ingressAgentSample, `"metadata":{`, `"apiVersion":"v1","kind":"Pod","metadata":{`, 1)
	generic := strings.Replace(typed, `"kind":"PodList"`, `"kind":"List"`, 1)
	var duplicate map[string]json.RawMessage
	if json.Unmarshal([]byte(ingressAgentSample), &duplicate) != nil {
		t.Fatal("invalid synthetic agent")
	}
	var items []json.RawMessage
	if json.Unmarshal(duplicate["items"], &items) != nil {
		t.Fatal("invalid synthetic list")
	}
	var err error
	duplicate["items"], err = json.Marshal(append(items, items[0]))
	if err != nil {
		t.Fatal(err)
	}
	duplicated, err := json.Marshal(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"owned running agent", ingressAgentSample, true},
		{"typed generic list", generic, true},
		{"generic list without resource identity", strings.Replace(ingressAgentSample, `"kind":"PodList"`, `"kind":"List"`, 1), false},
		{"generic list wrong resource", strings.Replace(generic, `"kind":"Pod"`, `"kind":"Node"`, 1), false},
		{"pod list wrong resource", strings.Replace(typed, `"kind":"Pod"`, `"kind":"Node"`, 1), false},
		{"different node", strings.Replace(ingressAgentSample, `"nodeName":"worker-1"`, `"nodeName":"worker-2"`, 1), false},
		{"different owner", strings.Replace(ingressAgentSample, "22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333", 1), false},
		{"wrong namespace", strings.Replace(ingressAgentSample, `"namespace":"kube-system"`, `"namespace":"other"`, 1), false},
		{"wrong service account", strings.Replace(ingressAgentSample, `"serviceAccountName":"cilium"`, `"serviceAccountName":"other"`, 1), false},
		{"not host network", strings.Replace(ingressAgentSample, `"hostNetwork":true`, `"hostNetwork":false`, 1), false},
		{"unexpected image", strings.Replace(ingressAgentSample, "v1.20.4@sha256:", "v1.20.5@sha256:", 1), false},
		{"unready", strings.Replace(ingressAgentSample, `"ready":true`, `"ready":false`, 1), false},
		{"not running", strings.Replace(ingressAgentSample, `"running":`, `"terminated":`, 1), false},
		{"negative restart", strings.Replace(ingressAgentSample, `"restartCount":0`, `"restartCount":-1`, 1), false},
		{"ambiguous", string(duplicated), false},
		{"case alias", strings.Replace(ingressAgentSample, `"nodeName":`, `"NodeName":`, 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent, err := bindIngressAgent([]byte(test.raw), "worker-1", "22222222-2222-2222-2222-222222222222")
			if (err == nil) != test.valid {
				t.Fatal("unbound execution target accepted or owned agent refused")
			}
			if test.valid && (agent.Name != "cilium-abcde" || agent.UID != "11111111-1111-1111-1111-111111111111") {
				t.Fatal("wrong execution target")
			}
			if !test.valid && agent.Name != "" {
				t.Fatal("failed binding released an execution target")
			}
		})
	}
}
