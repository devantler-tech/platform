package main

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The existing hosted transport gate provides the checksum-verified native
// client. Parsing the exact invocation must succeed without connecting; a
// malformed protobuf filter must fail through the same executable.
func TestIngressNativeFilter(t *testing.T) {
	binary := os.Getenv("HUBBLE_TEST_BIN")
	if binary == "" {
		t.Skip("native Hubble parser runs in the hosted transport gate")
	}
	filter, err := ingressFlowFilter(netip.MustParseAddr("192.0.2.20"))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{string(filter), `{"destination_ip":"192.0.2.20"}`} {
		command := exec.Command(binary, "observe", "--server", "unix:///var/run/cilium/hubble.sock", "--timeout", "5s", "--since", "2026-10-28T16:00:00Z", "--until", "2026-10-28T16:00:10Z", "--allowlist", input, "--output", "jsonpb", "--print-raw-filters")
		out, err := command.CombinedOutput()
		if (err == nil) != (input == string(filter)) || (err == nil && !strings.Contains(string(out), "allowlist")) {
			t.Fatalf("native parser refused the exact bounded invocation or accepted a malformed filter: %v %s", err, out)
		}
	}
}
