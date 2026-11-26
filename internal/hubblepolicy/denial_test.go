package hubblepolicy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDatabaseDenialRequiresEgressFromTheRecoveredIdentity(t *testing.T) {
	start := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	peer := Denial{Source: Endpoint{Namespace: "longhorn-db-proof-umami-db-12345", Pod: "restore-1"},
		Destination: Endpoint{Namespace: "umami", Pod: "umami-db-1"},
		SourceIP:    "10.1.1.10", DestinationIP: "10.1.1.11", Node: "prod/prod-worker-3", Port: 5432,
		Since: start, Until: start.Add(5 * time.Second), RequireEgress: true}
	native := func(direction, source, destination string) string {
		return fmt.Sprintf(`{"flow":{"verdict":"DROPPED","drop_reason_desc":"POLICY_DENIED","traffic_direction":%q,
		"time":"2026-11-26T10:00:01Z","node_name":"prod/prod-worker-3",
		"source":{"namespace":%q,"pod_name":"restore-1"},
		"destination":{"namespace":"umami","pod_name":"umami-db-1"},
		"IP":{"source":"10.1.1.10","destination":%q},"l4":{"TCP":{"destination_port":5432,"flags":{"SYN":true}}}}}`,
			direction, source, destination)
	}
	correct := native("EGRESS", peer.Source.Namespace, peer.DestinationIP)
	for _, tc := range []struct {
		name, response string
		allow          bool
	}{
		{"exact source egress", correct, true},
		{"destination ingress only", native("INGRESS", peer.Source.Namespace, peer.DestinationIP), false},
		{"foreign namespace", native("EGRESS", "other", peer.DestinationIP), false},
		{"unbound address", native("EGRESS", peer.Source.Namespace, "10.1.1.12"), false},
		{"lost events", correct + `{"lost_events":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyTCPDenial(strings.NewReader(tc.response), peer); (err == nil) != tc.allow {
				t.Fatalf("unexpected isolated database flow verdict: %v", err)
			}
		})
	}
}
