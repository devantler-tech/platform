package main

import (
	"strings"
	"testing"
	"time"
)

func TestPolicyDenialMustBindEveryIdentityAndTheAttemptWindow(t *testing.T) {
	t.Parallel()
	expected := denialTarget{
		namespace: "arc-runners", pod: "probe-123", source: "192.0.2.2",
		destination: "192.0.2.3", port: 8181, node: "default/worker",
		since: time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC),
		until: time.Date(2026, 10, 5, 22, 0, 10, 0, time.UTC),
	}
	valid := `{"flow":{"verdict":"DROPPED","drop_reason_desc":"POLICY_DENIED","traffic_direction":"EGRESS","source":{"namespace":"arc-runners","pod_name":"probe-123"},"IP":{"source":"192.0.2.2","destination":"192.0.2.3"},"l4":{"TCP":{"destination_port":8181,"flags":{"SYN":true}}},"node_name":"default/worker","time":"2026-10-05T22:00:05Z"}}`
	cases := []struct {
		name, input string
		want        bool
	}{
		{"correlated policy drop", valid, true},
		{"timeout only", "", false},
		{"malformed observer", "{", false},
		{"other source", strings.Replace(valid, "192.0.2.2", "192.0.2.4", 1), false},
		{"same pod prefix", strings.Replace(valid, "probe-123", "probe-123-other", 1), false},
		{"other namespace", strings.Replace(valid, "arc-runners", "other", 1), false},
		{"other destination", strings.Replace(valid, "192.0.2.3", "192.0.2.4", 1), false},
		{"other port", strings.Replace(valid, "8181", "443", 1), false},
		{"other node", strings.Replace(valid, "default/worker", "default/other", 1), false},
		{"outside attempt", strings.Replace(valid, "22:00:05", "21:59:59", 1), false},
		{"unrecognized timestamp", strings.Replace(valid, "2026-10-05T22:00:05Z", "unknown", 1), false},
		{"other direction", strings.Replace(valid, "EGRESS", "INGRESS", 1), false},
		{"other reason", strings.Replace(valid, "POLICY_DENIED", "POLICY_DENY", 1), false},
		{"forwarded event", strings.Replace(valid, "DROPPED", "FORWARDED", 1), false},
		{"not a connection attempt", strings.Replace(valid, `"SYN":true`, `"SYN":false`, 1), false},
		{"lost events", valid + "\n" + `{"lost_events":{"num_events_lost":1}}`, false},
		{"observer node failure", valid + "\n" + `{"node_status":{"state_change":"NODE_ERROR"}}`, false},
		{"trailing malformed response", valid + "\n{", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyDenial(strings.NewReader(tc.input), expected)
			if (err == nil) != tc.want {
				t.Fatalf("verdict success = %v, want %v", err == nil, tc.want)
			}
		})
	}
}
