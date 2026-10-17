package main

import "github.com/devantler-tech/platform/internal/strictjson"

// Validate each bounded native document before typed decoding can overwrite
// duplicate members or accept case aliases. Unrelated native fields retain
// their spelling and values; only fields used by this proof have canonical names.
func requireTransportJSON(data []byte, health bool) error {
	fields := map[string][]string{
		"":                  {"flow", "lost_events", "lostEvents", "node_status", "nodeStatus"},
		"flow":              {"verdict", "drop_reason_desc", "traffic_direction", "source", "destination", "IP", "l4", "node_name", "time"},
		"flow.source":       {"namespace", "pod_name"},
		"flow.destination":  {"namespace", "pod_name"},
		"flow.IP":           {"source", "destination"},
		"flow.l4":           {"TCP"},
		"flow.l4.TCP":       {"destination_port", "flags"},
		"flow.l4.TCP.flags": {"SYN"},
	}
	if health {
		fields = map[string][]string{"": {"initialized", "sealed", "version", "cluster_id"}}
	}
	return strictjson.Validate(data, fields)
}
