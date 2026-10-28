package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"slices"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

var errIngressEvidence = errors.New("inconclusive ingress evidence")

// decodeIngressFlows accepts complete jsonpb observations of new connections
// delivered to the bound application endpoint. It never returns partial proof.
func decodeIngressFlows(data []byte, destination netip.Addr, node string, since, until time.Time) ([]netip.Addr, error) {
	refuse := func() ([]netip.Addr, error) { return nil, errIngressEvidence }
	if len(data) == 0 || len(data) > maxAPIOutput || !destination.IsGlobalUnicast() || destination.Is4In6() ||
		node == "" || since.IsZero() || !until.After(since) || until.Sub(since) > 45*time.Second {
		return refuse()
	}
	fields := map[string][]string{
		"":            {"flow", "lost_events", "lostEvents", "node_status", "nodeStatus"},
		"flow":        {"time", "verdict", "traffic_direction", "trace_observation_point", "source", "destination", "IP", "l4", "node_name", "is_reply"},
		"flow.source": {"identity", "labels"}, "flow.destination": {"namespace", "pod_name"},
		"flow.IP": {"source", "source_xlated", "destination"}, "flow.l4": {"TCP"},
		"flow.l4.TCP": {"destination_port", "flags"}, "flow.l4.TCP.flags": {"SYN", "ACK"},
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var addresses []netip.Addr
	for count := 0; ; count++ {
		if count >= 128 {
			return refuse()
		}
		var document json.RawMessage
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return refuse()
		}
		if strictjson.Validate(document, fields) != nil {
			return refuse()
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(document, &envelope) != nil {
			return refuse()
		}
		for _, name := range []string{"lost_events", "lostEvents", "node_status", "nodeStatus"} {
			if _, exists := envelope[name]; exists {
				return refuse()
			}
		}
		raw, exists := envelope["flow"]
		if !exists {
			return refuse()
		}
		var flow struct {
			Time      time.Time `json:"time"`
			Verdict   string    `json:"verdict"`
			Direction string    `json:"traffic_direction"`
			Point     string    `json:"trace_observation_point"`
			Node      string    `json:"node_name"`
			Reply     *bool     `json:"is_reply"`
			Source    struct {
				Identity uint32   `json:"identity"`
				Labels   []string `json:"labels"`
			} `json:"source"`
			Destination struct {
				Namespace string `json:"namespace"`
				Pod       string `json:"pod_name"`
			} `json:"destination"`
			IP struct {
				Source      string `json:"source"`
				Translated  string `json:"source_xlated"`
				Destination string `json:"destination"`
			} `json:"IP"`
			L4 struct {
				TCP struct {
					Port  uint16                  `json:"destination_port"`
					Flags struct{ SYN, ACK bool } `json:"flags"`
				} `json:"TCP"`
			} `json:"l4"`
		}
		if json.Unmarshal(raw, &flow) != nil {
			return refuse()
		}
		if flow.Time.IsZero() || flow.Time.Before(since) || flow.Time.After(until) {
			return refuse()
		}
		// The pinned native parser classifies SNAT traces as EGRESS, even at
		// TO_ENDPOINT. The receiver observation point and node bind the wire
		// peer; direction alone does not distinguish pre/post translation.
		if flow.Verdict != "FORWARDED" || (flow.Direction != "INGRESS" && flow.Direction != "EGRESS") || flow.Point != "TO_ENDPOINT" ||
			flow.Node != node || (flow.Reply != nil && *flow.Reply) || flow.Source.Identity != 8 || !slices.Contains(flow.Source.Labels, "reserved:ingress") ||
			flow.Destination.Namespace != "private-mail" || flow.Destination.Pod != "private-mail-0" || flow.IP.Destination != destination.String() ||
			flow.L4.TCP.Port != 8080 || !flow.L4.TCP.Flags.SYN || flow.L4.TCP.Flags.ACK {
			return refuse()
		}
		original, err := netip.ParseAddr(flow.IP.Source)
		if err != nil || original.IsUnspecified() || original.Is4In6() || original.Zone() != "" {
			return refuse()
		}
		// Cilium's pinned flow schema retains the pre-SNAT address in source
		// when source_xlated is present. Only the translated peer reaches the app.
		source := flow.IP.Source
		if flow.IP.Translated != "" {
			source = flow.IP.Translated
		}
		address, err := netip.ParseAddr(source)
		if err != nil || !address.IsGlobalUnicast() || address.Is4In6() || address.Zone() != "" || address.Is4() != destination.Is4() || address == destination {
			return refuse()
		}
		if !slices.Contains(addresses, address) {
			addresses = append(addresses, address)
		}
		if len(addresses) > 32 {
			return refuse()
		}
	}
	if len(addresses) == 0 {
		return refuse()
	}
	return addresses, nil
}
