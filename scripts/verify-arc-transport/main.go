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

	"github.com/devantler-tech/platform/internal/hubblepolicy"
)

type target struct {
	pod, source, destination, node string
	port                           uint16
	since, until                   time.Time
}

func verifyHealth(input io.Reader) error {
	bounded := &io.LimitedReader{R: input, N: 1 << 20}
	decoder := json.NewDecoder(bounded)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return errors.New("invalid health response")
	}
	if err := requireHealthJSON(raw); err != nil {
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
	if health.Initialized == nil || !*health.Initialized || health.Sealed == nil || *health.Sealed || health.Version != "2.7.3" || health.Cluster == "" {
		return errors.New("canary is not initialized and unsealed at the pinned version")
	}
	return nil
}

func verifyDenial(input io.Reader, expected target) error {
	return hubblepolicy.VerifyTCPDenial(input, hubblepolicy.Denial{
		Source:      hubblepolicy.Endpoint{Namespace: "arc-runners", Pod: expected.pod},
		Destination: hubblepolicy.Endpoint{Namespace: "openbao", Pod: "openbao-2"},
		SourceIP:    expected.source, DestinationIP: expected.destination, Node: expected.node,
		Port: expected.port, Since: expected.since, Until: expected.until,
	})
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
