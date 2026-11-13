package main

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var identityPodCreated = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)

// identityPlatform stands for the bound node, Pod and Cilium addresses. They are
// documentation addresses, so only the explicit platform list can exclude them.
var identityPlatform = []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.20"), netip.MustParseAddr("192.0.2.30")}

func identityBlock(id, address, created string) map[string]any {
	return map[string]any{"id": id, "address": address, "expiresAt": nil, "reason": "portScanning", "createdAt": created}
}

// identityBlocks stores automatic port-scanning blocks.
func identityBlocks(created string, addresses ...string) []any {
	return identityBlocksFor("portScanning", created, addresses...)
}

// identityBlocksFor numbers each address within its reason, so repeated
// addresses and mixed reasons stay distinct records.
func identityBlocksFor(reason, created string, addresses ...string) []any {
	blocks := []any{}
	for i, address := range addresses {
		block := identityBlock(fmt.Sprintf("%s%d", reason, i), address, created)
		block["reason"] = reason
		blocks = append(blocks, block)
	}
	return blocks
}

// TestClientIdentityCountsOnlyDistinctPublicClientsBlockedDuringThePod
// catches a shared address passing as many clients, or a single client as two.
func TestClientIdentityCountsOnlyDistinctPublicClientsBlockedDuringThePod(t *testing.T) {
	during, before := "2026-10-26T00:00:00Z", "2026-10-24T00:00:00Z"
	for _, tc := range []struct {
		name    string
		blocks  []any
		verdict string
		public  int
	}{
		{"none", []any{}, "INSUFFICIENT_EVIDENCE", 0},
		{"one client", identityBlocks(during, "203.0.113.7"), "INSUFFICIENT_EVIDENCE", 1},
		{"one client twice", identityBlocks(during, "203.0.113.7", "203.0.113.7"), "INSUFFICIENT_EVIDENCE", 1},
		{"two clients", identityBlocks(during, "203.0.113.7", "198.51.100.9"), "DISTINCT_PUBLIC_CLIENTS", 2},
		{"IPv6 clients", identityBlocks(during, "2001:db8::7", "2001:db8::9"), "DISTINCT_PUBLIC_CLIENTS", 2},
		{"mixed families", identityBlocks(during, "203.0.113.7", "2001:db8::9"), "DISTINCT_PUBLIC_CLIENTS", 2},
		{"retained shared block", append(identityBlocks(during, "203.0.113.7", "198.51.100.9"), identityBlock("old", "10.244.3.17", before)), "DISTINCT_PUBLIC_CLIENTS", 2},
		{"public before the Pod", identityBlocks(before, "203.0.113.7", "198.51.100.9"), "INSUFFICIENT_EVIDENCE", 0},
		{"private", identityBlocks(during, "203.0.113.7", "198.51.100.9", "10.244.3.17"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"loopback", identityBlocks(during, "203.0.113.7", "198.51.100.9", "127.0.0.1"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"IPv6 loopback", identityBlocks(during, "203.0.113.7", "198.51.100.9", "::1"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"unique local", identityBlocks(during, "203.0.113.7", "198.51.100.9", "fd00::7"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"shared address space", identityBlocks(during, "203.0.113.7", "198.51.100.9", "100.64.0.7"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"node address", identityBlocks(during, "203.0.113.7", "198.51.100.9", "192.0.2.10"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"Pod address", identityBlocks(during, "203.0.113.7", "198.51.100.9", "192.0.2.20"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"Cilium address", identityBlocks(during, "203.0.113.7", "198.51.100.9", "192.0.2.30"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"range", identityBlocks(during, "203.0.113.7", "198.51.100.9", "203.0.113.0/24"), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"manual clients", identityBlocksFor("manual", during, "203.0.113.7", "198.51.100.9"), "INSUFFICIENT_EVIDENCE", 0},
		{"unknown-cause clients", identityBlocksFor("other", during, "203.0.113.7", "198.51.100.9"), "INSUFFICIENT_EVIDENCE", 0},
		{"one automatic, one manual", append(identityBlocks(during, "203.0.113.7"), identityBlocksFor("manual", during, "198.51.100.9")[0].(map[string]any)), "INSUFFICIENT_EVIDENCE", 1},
		{"manual private", append(identityBlocks(during, "203.0.113.7", "198.51.100.9"), identityBlocksFor("manual", during, "10.244.3.17")[0].(map[string]any)), "NON_CLIENT_ADDRESS_BLOCKED", 2},
		{"authentication failures", identityBlocksFor("authFailure", during, "203.0.113.7", "198.51.100.9"), "DISTINCT_PUBLIC_CLIENTS", 2},
		{"loitering", identityBlocksFor("loitering", during, "203.0.113.7", "198.51.100.9"), "DISTINCT_PUBLIC_CLIENTS", 2},
		{"recipient failures", identityBlocksFor("rcptToFailure", during, "203.0.113.7", "198.51.100.9"), "DISTINCT_PUBLIC_CLIENTS", 2},
		{"host-length prefix", identityBlocks(during, "203.0.113.7/32", "2001:db8::9/128"), "DISTINCT_PUBLIC_CLIENTS", 2},
		{"same client as address and prefix", identityBlocks(during, "203.0.113.7", "203.0.113.7/32"), "INSUFFICIENT_EVIDENCE", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := accessMeasurement(t, securityStateFor(t, []any{}, tc.blocks))
			got, e := decodeClientIdentity(raw, identityPodCreated, accessNow, identityPlatform...)
			if e != nil || got.Verdict != tc.verdict || got.Public != tc.public {
				t.Fatalf("got %+v/%v, want %s with %d", got, e, tc.verdict, tc.public)
			}
		})
	}
}

// TestClientIdentityKeepsExpiredAndAllowedDecisions shows that expiry and
// allow rules do not hide which address the server judged.
func TestClientIdentityKeepsExpiredAndAllowedDecisions(t *testing.T) {
	blocks := identityBlocks("2026-10-26T00:00:00Z", "203.0.113.7", "198.51.100.9", "10.244.3.17")
	blocks[0].(map[string]any)["expiresAt"] = "2026-10-26T01:00:00Z"
	blocks[2].(map[string]any)["expiresAt"] = "2026-10-26T01:00:00Z"
	allow := []any{map[string]any{"id": "a0", "address": "10.244.3.17", "expiresAt": nil}}
	got, e := decodeClientIdentity(accessMeasurement(t, securityStateFor(t, allow, blocks)), identityPodCreated, accessNow, identityPlatform...)
	if e != nil || got.Verdict != "NON_CLIENT_ADDRESS_BLOCKED" || got.Public != 2 {
		t.Fatalf("an expired or allow-overridden decision was hidden: %+v/%v", got, e)
	}
}

// TestClientIdentityRefusesIncompleteEvidence keeps a partial or invalid answer from being counted.
func TestClientIdentityRefusesIncompleteEvidence(t *testing.T) {
	during := "2026-10-26T00:00:00Z"
	valid := accessMeasurement(t, securityStateFor(t, []any{}, identityBlocks(during, "203.0.113.7", "198.51.100.9")))
	for _, tc := range []struct {
		name     string
		raw      []byte
		created  time.Time
		platform []netip.Addr
	}{
		{"missing creation", accessMeasurement(t, securityStateFor(t, []any{}, []any{map[string]any{"id": "b0", "address": "203.0.113.7", "expiresAt": nil, "reason": "manual", "createdAt": nil}})), identityPodCreated, identityPlatform},
		{"missing reason", accessMeasurement(t, securityStateFor(t, []any{}, []any{map[string]any{"id": "b0", "address": "203.0.113.7", "expiresAt": nil, "reason": nil, "createdAt": during}})), identityPodCreated, identityPlatform},
		{"unknown reason", accessMeasurement(t, securityStateFor(t, []any{}, identityBlocksFor("unreviewed", during, "203.0.113.7"))), identityPodCreated, identityPlatform},
		{"future creation", accessMeasurement(t, securityStateFor(t, []any{}, identityBlocks("2026-10-28T00:00:00Z", "203.0.113.7"))), identityPodCreated, identityPlatform},
		{"invalid creation", accessMeasurement(t, securityStateFor(t, []any{}, identityBlocks("yesterday", "203.0.113.7"))), identityPodCreated, identityPlatform},
		{"invalid address", accessMeasurement(t, securityStateFor(t, []any{}, identityBlocks(during, "203.0.113.300"))), identityPodCreated, identityPlatform},
		{"mapped address", accessMeasurement(t, securityStateFor(t, []any{}, identityBlocks(during, "::ffff:203.0.113.7"))), identityPodCreated, identityPlatform},
		{"truncated inventory", []byte(strings.Replace(string(valid), `"total":2`, `"total":3`, 1)), identityPodCreated, identityPlatform},
		{"partial observation", []byte(`{"live_loopback":200}`), identityPodCreated, identityPlatform},
		{"unknown Pod creation", valid, time.Time{}, identityPlatform},
		{"Pod created later", valid, accessNow.Add(time.Minute), identityPlatform},
		{"no platform addresses", valid, identityPodCreated, nil},
		{"invalid platform address", valid, identityPodCreated, []netip.Addr{{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, e := decodeClientIdentity(tc.raw, tc.created, accessNow, tc.platform...); e == nil {
				t.Fatalf("incomplete evidence counted: %+v", got)
			}
		})
	}
}

// TestAccessReportAcceptsPerClientBlocksOnlyThroughTheAdapter pins the public
// lines and the exit decision for both transports.
func TestAccessReportAcceptsPerClientBlocksOnlyThroughTheAdapter(t *testing.T) {
	healthy := accessResult{Paths: "ALL_LOCAL_TARGETS_HTTP_200", Access: "NO_ACTIVE_STORED_BLOCKS", CiliumAccess: "NO_ACTIVE_STORED_BLOCKS", Block: blockDetails{"NOT_APPLICABLE", "NOT_APPLICABLE", "NOT_APPLICABLE", "NOT_APPLICABLE"}, Identity: clientIdentity{Verdict: "NOT_APPLICABLE"}, Ready: true}
	line, code := accessReport(healthy)
	want := "MAIL_PROBE_PATHS=ALL_LOCAL_TARGETS_HTTP_200\nMAIL_ACCESS_STATE=NO_ACTIVE_STORED_BLOCKS\nMAIL_CILIUM_ACCESS_STATE=NO_ACTIVE_STORED_BLOCKS\nMAIL_CILIUM_BLOCK_REASON=NOT_APPLICABLE\nMAIL_CILIUM_BLOCK_SCOPE=NOT_APPLICABLE\nMAIL_CILIUM_BLOCK_EXPIRY=NOT_APPLICABLE\nMAIL_CILIUM_BLOCK_CREATION=NOT_APPLICABLE\nMAIL_CLIENT_IDENTITY=NOT_APPLICABLE\nMAIL_CLIENT_DISTINCT_PUBLIC_BLOCKS=0\nMAIL_KUBERNETES_READY=TRUE\n"
	if line != want || code != 0 {
		t.Fatalf("report changed: %d %q", code, line)
	}
	retained := healthy
	retained.Access, retained.CiliumAccess = "OTHER_ACTIVE_STORED_BLOCKS", "STORED_CILIUM_BLOCK_MATCH"
	retained.Block = blockDetails{"PORT_SCANNING", "SINGLE_IP", "NO_EXPIRY", "BEFORE_CURRENT_POD"}
	for _, tc := range []struct {
		name   string
		change func(*accessResult)
		code   int
	}{
		{"without adapter, any active block", func(r *accessResult) { *r = retained; r.Identity.Verdict = "NOT_APPLICABLE" }, 1},
		{"adapter with distinct clients", func(r *accessResult) { *r = retained; r.Identity = clientIdentity{"DISTINCT_PUBLIC_CLIENTS", 2} }, 0},
		{"adapter with too few clients", func(r *accessResult) { *r = retained; r.Identity = clientIdentity{"INSUFFICIENT_EVIDENCE", 1} }, 1},
		{"adapter with a non-client block", func(r *accessResult) { *r = retained; r.Identity = clientIdentity{"NON_CLIENT_ADDRESS_BLOCKED", 5} }, 1},
		{"adapter but unready", func(r *accessResult) {
			*r = retained
			r.Identity = clientIdentity{"DISTINCT_PUBLIC_CLIENTS", 2}
			r.Ready = false
		}, 1},
		{"adapter but a failed path", func(r *accessResult) {
			*r = retained
			r.Identity = clientIdentity{"DISTINCT_PUBLIC_CLIENTS", 2}
			r.Paths = "POD_ADDRESS_PATH_FAILED"
		}, 1},
		{"unclassified identity", func(r *accessResult) { *r = healthy; r.Identity = clientIdentity{} }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := healthy
			tc.change(&result)
			line, code := accessReport(result)
			if code != tc.code || !strings.Contains(line, "MAIL_CLIENT_IDENTITY="+result.Identity.Verdict+"\n") || !strings.Contains(line, fmt.Sprintf("MAIL_CLIENT_DISTINCT_PUBLIC_BLOCKS=%d\n", result.Identity.Public)) {
				t.Fatalf("exit %d, want %d: %q", code, tc.code, line)
			}
			for _, private := range []string{"203.0.113", "198.51.100", "192.0.2", "10.244"} {
				if strings.Contains(line, private) {
					t.Fatal("an address reached public output")
				}
			}
		})
	}
}
