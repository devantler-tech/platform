package main

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

const ingressFlowSample = `{"flow":{"time":"2026-10-28T16:00:02Z","verdict":"FORWARDED","traffic_direction":"INGRESS","trace_observation_point":"TO_ENDPOINT","source":{"identity":8,"labels":["reserved:ingress"]},"destination":{"namespace":"private-mail","pod_name":"private-mail-0"},"IP":{"source":"10.244.1.1","destination":"10.244.2.2"},"l4":{"TCP":{"destination_port":8080,"flags":{"SYN":true}}},"node_name":"prod/worker-1"}}`

// Changing the target, wire observation point or source identity must never
// release an unrelated address for comparison with the application's rules.
func TestIngressFlowRequiresBoundWireEvidence(t *testing.T) {
	start := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	destination := netip.MustParseAddr("10.244.2.2")
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"wire source", ingressFlowSample, true},
		{"SNAT trace classified as egress", strings.Replace(ingressFlowSample, `"traffic_direction":"INGRESS"`, `"traffic_direction":"EGRESS"`, 1), true},
		{"unknown direction", strings.Replace(ingressFlowSample, `"traffic_direction":"INGRESS"`, `"traffic_direction":"TRAFFIC_DIRECTION_UNKNOWN"`, 1), false},
		{"duplicate observation", ingressFlowSample + "\n" + ingressFlowSample, true},
		{"wrong destination", strings.Replace(ingressFlowSample, `"destination":"10.244.2.2"`, `"destination":"10.244.2.3"`, 1), false},
		{"wrong pod", strings.Replace(ingressFlowSample, `"pod_name":"private-mail-0"`, `"pod_name":"other-0"`, 1), false},
		{"wrong namespace", strings.Replace(ingressFlowSample, `"namespace":"private-mail"`, `"namespace":"other"`, 1), false},
		{"wrong port", strings.Replace(ingressFlowSample, `"destination_port":8080`, `"destination_port":8200`, 1), false},
		{"wrong node", strings.Replace(ingressFlowSample, `"prod/worker-1"`, `"prod/worker-2"`, 1), false},
		{"wrong identity", strings.Replace(ingressFlowSample, `"identity":8`, `"identity":6`, 1), false},
		{"wrong labels", strings.Replace(ingressFlowSample, `"reserved:ingress"`, `"reserved:host"`, 1), false},
		{"pre-translation observation", strings.Replace(ingressFlowSample, `"TO_ENDPOINT"`, `"FROM_PROXY"`, 1), false},
		{"not a new connection", strings.Replace(ingressFlowSample, `"SYN":true`, `"SYN":false`, 1), false},
		{"reply handshake", strings.Replace(ingressFlowSample, `"SYN":true`, `"SYN":true,"ACK":true`, 1), false},
		{"dropped", strings.Replace(ingressFlowSample, `"FORWARDED"`, `"DROPPED"`, 1), false},
		{"old", strings.Replace(ingressFlowSample, "16:00:02Z", "15:59:02Z", 1), false},
		{"future", strings.Replace(ingressFlowSample, "16:00:02Z", "16:01:02Z", 1), false},
		{"duplicate field", strings.Replace(ingressFlowSample, `"identity":8`, `"identity":6,"identity":8`, 1), false},
		{"case alias", strings.Replace(ingressFlowSample, `"identity":8`, `"Identity":8`, 1), false},
		{"truncated", ingressFlowSample[:len(ingressFlowSample)-1], false},
		{"trailing malformed", ingressFlowSample + "\n{", false},
		{"empty flow after a match", ingressFlowSample + "\n" + `{"flow":{}}`, false},
		{"null flow after a match", ingressFlowSample + "\n" + `{"flow":null}`, false},
		{"lost events after a match", ingressFlowSample + "\n" + `{"lost_events":{"num_events_lost":1}}`, false},
		{"unavailable node after a match", ingressFlowSample + "\n" + `{"node_status":{"state_change":"NODE_UNAVAILABLE"}}`, false},
		{"empty", "", false},
		{"oversized", strings.Repeat(" ", maxAPIOutput+1) + ingressFlowSample, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			addresses, err := decodeIngressFlows([]byte(test.raw), destination, "prod/worker-1", start, start.Add(10*time.Second))
			if (err == nil) != test.valid {
				t.Fatal("unbound or incomplete flow evidence was accepted, or real wire evidence refused")
			}
			if test.valid && (len(addresses) != 1 || addresses[0] != netip.MustParseAddr("10.244.1.1")) {
				t.Fatal("wire source was not returned exactly once")
			}
			if !test.valid && len(addresses) != 0 {
				t.Fatal("failed evidence released an address")
			}
		})
	}
}

// The pinned native schema defines source_xlated as the post-SNAT address.
// Comparing its pre-translation source with application rules would name the wrong peer.
func TestIngressFlowUsesPostTranslationSource(t *testing.T) {
	start := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	raw := strings.Replace(ingressFlowSample, `"source":"10.244.1.1"`, `"source":"10.244.1.1","source_xlated":"10.0.1.1"`, 1)
	addresses, err := decodeIngressFlows([]byte(raw), netip.MustParseAddr("10.244.2.2"), "prod/worker-1", start, start.Add(10*time.Second))
	if err != nil || len(addresses) != 1 || addresses[0] != netip.MustParseAddr("10.0.1.1") {
		t.Fatal("post-translation wire address was replaced by the original source")
	}
	invalid := strings.Replace(raw, `"source_xlated":"10.0.1.1"`, `"source_xlated":"not-an-address"`, 1)
	if addresses, err := decodeIngressFlows([]byte(invalid), netip.MustParseAddr("10.244.2.2"), "prod/worker-1", start, start.Add(10*time.Second)); err == nil || len(addresses) != 0 {
		t.Fatal("an invalid translated address fell back to a different peer")
	}
}
