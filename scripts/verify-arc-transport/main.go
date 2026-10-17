// Protected CI's bounded evidence parser. It prints no health body, address or flow.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"
)

type target struct {
	pod, source, destination, node string
	port                           uint16
	since, until                   time.Time
}

type endpoint struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod_name"`
}
type flow struct {
	Verdict     string   `json:"verdict"`
	Reason      string   `json:"drop_reason_desc"`
	Direction   string   `json:"traffic_direction"`
	Source      endpoint `json:"source"`
	Destination endpoint `json:"destination"`
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

func verifyHealth(input io.Reader) error {
	bounded := &io.LimitedReader{R: input, N: 1 << 20}
	decoder := json.NewDecoder(bounded)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return errors.New("invalid health response")
	}
	if err := requireTransportJSON(raw, true); err != nil {
		return err
	}
	var health struct {
		Initialized *bool  `json:"initialized"`
		Sealed      *bool  `json:"sealed"`
		Version     string `json:"version"`
		Cluster     string `json:"cluster_id"`
	}
	if err := json.Unmarshal(raw, &health); err != nil {
		return errors.New("invalid health response")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || bounded.N <= 0 {
		return errors.New("incomplete health response")
	}
	if health.Initialized == nil || !*health.Initialized || health.Sealed == nil || *health.Sealed || health.Version != "2.6.3" || health.Cluster == "" {
		return errors.New("canary is not initialized and unsealed at the pinned version")
	}
	return nil
}

func verifyDenial(input io.Reader, expected target) error {
	bounded := &io.LimitedReader{R: input, N: 4 << 20}
	decoder := json.NewDecoder(bounded)
	matched := false
	for {
		var document json.RawMessage
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return errors.New("malformed observer response")
		}
		if err := requireTransportJSON(document, false); err != nil {
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
		when, err := time.Parse(time.RFC3339Nano, observed.Time)
		if err != nil || when.Before(expected.since) || when.After(expected.until) {
			continue
		}
		// Source default-deny or destination ingress denial both prove interception.
		// UNKNOWN, missing and other directions never qualify.
		if observed.Verdict == "DROPPED" && observed.Reason == "POLICY_DENIED" &&
			(observed.Direction == "EGRESS" || observed.Direction == "INGRESS") &&
			observed.Source.Namespace == "arc-runners" && observed.Source.Pod == expected.pod &&
			observed.Destination.Namespace == "openbao" && observed.Destination.Pod == "openbao-2" &&
			observed.IP.Source == expected.source && observed.IP.Destination == expected.destination &&
			observed.L4.TCP.Port == expected.port && observed.L4.TCP.Flags.SYN && observed.Node == expected.node {
			matched = true
		}
	}
	if bounded.N <= 0 {
		return errors.New("observer response exceeds bound")
	}
	if !matched {
		return errors.New("no correlated policy denial")
	}
	return nil
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "health" {
		return verifyHealth(os.Stdin)
	}
	if len(args) == 0 || args[0] != "denial" {
		return errors.New("invalid command")
	}
	flags := flag.NewFlagSet("denial", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var expected target
	var port uint
	var since, until string
	flags.StringVar(&expected.pod, "pod", "", "")
	flags.StringVar(&expected.source, "source", "", "")
	flags.StringVar(&expected.destination, "destination", "", "")
	flags.StringVar(&expected.node, "node", "", "")
	flags.UintVar(&port, "port", 0, "")
	flags.StringVar(&since, "since", "", "")
	flags.StringVar(&until, "until", "", "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	if expected.pod == "" || expected.node == "" || (port != 8200 && port != 8204) {
		return errors.New("invalid target")
	}
	for _, address := range []string{expected.source, expected.destination} {
		if _, err := netip.ParseAddr(address); err != nil {
			return errors.New("invalid address")
		}
	}
	var err error
	expected.since, err = time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return errors.New("invalid start")
	}
	expected.until, err = time.Parse(time.RFC3339Nano, until)
	if err != nil || expected.until.Before(expected.since) || expected.until.Sub(expected.since) > 45*time.Second {
		return errors.New("invalid window")
	}
	expected.port = uint16(port)
	return verifyDenial(os.Stdin, expected)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ARC transport evidence: FAIL:", err)
		os.Exit(1)
	}
}
