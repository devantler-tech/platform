// Package hubblepolicy verifies native Hubble flow evidence for policy checks.
package hubblepolicy

import (
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// Endpoint identifies a Pod in a Hubble policy decision.
type Endpoint struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod_name"`
}

// Denial describes the exact flow that must have been blocked by policy.
type Denial struct {
	Source, Destination           Endpoint
	SourceIP, DestinationIP, Node string
	Port                          uint16
	Since, Until                  time.Time
	RequireEgress                 bool
}

// DenialFailure describes an unmatched part of a native flow without including
// addresses, resource names, or the observer output.
type DenialFailure string

func (f DenialFailure) Error() string { return string(f) }

type flow struct {
	Verdict     string   `json:"verdict"`
	Reason      string   `json:"drop_reason_desc"`
	Direction   string   `json:"traffic_direction"`
	Source      Endpoint `json:"source"`
	Destination Endpoint `json:"destination"`
	IP          struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	} `json:"IP"`
	L4 struct {
		TCP struct {
			Port  uint16 `json:"destination_port"`
			Flags struct {
				SYN bool `json:"SYN"`
			} `json:"flags"`
		} `json:"TCP"`
	} `json:"l4"`
	Node string `json:"node_name"`
	Time string `json:"time"`
}

var flowFields = map[string][]string{
	"":                  {"flow", "lost_events", "lostEvents", "node_status", "nodeStatus"},
	"flow":              {"verdict", "drop_reason_desc", "traffic_direction", "source", "destination", "IP", "l4", "node_name", "time"},
	"flow.source":       {"namespace", "pod_name"},
	"flow.destination":  {"namespace", "pod_name"},
	"flow.IP":           {"source", "destination"},
	"flow.l4":           {"TCP"},
	"flow.l4.TCP":       {"destination_port", "flags"},
	"flow.l4.TCP.flags": {"SYN"},
}

// VerifyTCPDenial requires a correlated dropped TCP SYN and a complete stream.
func VerifyTCPDenial(input io.Reader, expected Denial) error {
	bounded := &io.LimitedReader{R: input, N: 4 << 20}
	decoder := json.NewDecoder(bounded)
	matched := false
	closest := -1
	failure := DenialFailure("no-observed-flow")
	for {
		var document json.RawMessage
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return errors.New("malformed observer response")
		}
		if err := strictjson.Validate(document, flowFields); err != nil {
			return err
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(document, &response); err != nil {
			return errors.New("malformed observer response")
		}
		for _, field := range []string{"lost_events", "lostEvents", "node_status", "nodeStatus"} {
			if _, ok := response[field]; ok {
				return errors.New("incomplete observer response")
			}
		}
		raw, ok := response["flow"]
		if !ok {
			return errors.New("unknown observer response")
		}
		var observed flow
		if err := json.Unmarshal(raw, &observed); err != nil {
			return errors.New("malformed flow")
		}
		rank, mismatch := compareDenial(observed, expected)
		if mismatch == "" {
			matched = true
		} else if rank > closest {
			closest, failure = rank, mismatch
		}
	}
	if bounded.N <= 0 {
		return errors.New("observer response exceeds bound")
	}
	if !matched {
		return failure
	}
	return nil
}

func compareDenial(observed flow, expected Denial) (int, DenialFailure) {
	when, err := time.Parse(time.RFC3339Nano, observed.Time)
	switch {
	case observed.IP.Source != expected.SourceIP || observed.IP.Destination != expected.DestinationIP ||
		observed.L4.TCP.Port != expected.Port:
		return 0, "connection-address"
	case err != nil || when.Before(expected.Since) || when.After(expected.Until):
		return 1, "connection-window"
	case observed.Source != expected.Source || observed.Destination != expected.Destination:
		return 2, "pod-identity"
	case observed.Node != expected.Node:
		return 3, "observer-node"
	case observed.Verdict != "DROPPED" || observed.Reason != "POLICY_DENIED":
		return 4, "policy-verdict"
	case observed.Direction != "EGRESS" && (expected.RequireEgress || observed.Direction != "INGRESS"):
		return 5, "traffic-direction"
	case !observed.L4.TCP.Flags.SYN:
		return 6, "tcp-syn"
	default:
		return 7, ""
	}
}
