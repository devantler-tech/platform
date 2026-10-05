package main

import (
	"encoding/json"
	"errors"
	"io"
	"time"
)

type denialTarget struct {
	namespace, pod, source, destination, node string
	destinationNamespace, destinationPod      string
	port                                      uint16
	since, until                              time.Time
}

type endpointIdentity struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod_name"`
}
type deniedFlow struct {
	Verdict     string           `json:"verdict"`
	Reason      string           `json:"drop_reason_desc"`
	Direction   string           `json:"traffic_direction"`
	Source      endpointIdentity `json:"source"`
	Destination endpointIdentity `json:"destination"`
	IP          struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	} `json:"IP"`
	L4 struct {
		TCP struct {
			DestinationPort uint16 `json:"destination_port"`
			Flags           struct {
				SYN bool `json:"SYN"`
			} `json:"flags"`
		} `json:"TCP"`
	} `json:"l4"`
	Node string `json:"node_name"`
	Time string `json:"time"`
}

func verifyDenial(input io.Reader, expected denialTarget) error {
	limited := &io.LimitedReader{R: input, N: 4 << 20}
	decoder := json.NewDecoder(limited)
	matched := false
	for {
		var response map[string]json.RawMessage
		if err := decoder.Decode(&response); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return errors.New("malformed observer response")
		}
		for _, field := range []string{"lost_events", "lostEvents", "node_status", "nodeStatus"} {
			if _, exists := response[field]; exists {
				return errors.New("incomplete observer evidence")
			}
		}
		raw, exists := response["flow"]
		if !exists {
			return errors.New("unknown observer response")
		}
		var flow deniedFlow
		if err := json.Unmarshal(raw, &flow); err != nil {
			return errors.New("malformed flow")
		}
		if matchesDenial(flow, expected) {
			matched = true
		}
	}
	if limited.N <= 0 {
		return errors.New("observer evidence exceeds the bound")
	}
	if !matched {
		return errors.New("no correlated policy denial")
	}
	return nil
}

func matchesDenial(flow deniedFlow, expected denialTarget) bool {
	observed, err := time.Parse(time.RFC3339Nano, flow.Time)
	if err != nil || observed.Before(expected.since) || observed.After(expected.until) {
		return false
	}
	if flow.Verdict != "DROPPED" || flow.Reason != "POLICY_DENIED" || flow.Direction != "EGRESS" ||
		flow.Source.Namespace != expected.namespace || flow.Source.Pod != expected.pod ||
		flow.IP.Source != expected.source || flow.IP.Destination != expected.destination ||
		flow.L4.TCP.DestinationPort != expected.port || !flow.L4.TCP.Flags.SYN || flow.Node != expected.node {
		return false
	}
	return (expected.destinationNamespace == "" || flow.Destination.Namespace == expected.destinationNamespace) &&
		(expected.destinationPod == "" || flow.Destination.Pod == expected.destinationPod)
}
