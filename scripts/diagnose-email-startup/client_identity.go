package main

import (
	"encoding/json"
	"errors"
	"net/netip"
	"time"
)

// minimumDistinctClients is the smallest count a single shared address cannot produce.
const minimumDistinctClients = 2

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// automaticReasons are the server's own blocking decisions. A manual block or
// one of unknown cause is an administrator's choice, never client evidence.
var automaticReasons = map[string]bool{"rcptToFailure": true, "authFailure": true, "loitering": true, "portScanning": true}

// storedReasons are every reason the supported server stores.
var storedReasons = map[string]bool{"rcptToFailure": true, "authFailure": true, "loitering": true, "portScanning": true, "manual": true, "other": true}

type clientIdentity struct {
	Verdict string
	Public  int
}

// decodeClientIdentity reads which addresses the native server itself blocked
// while the current Pod, with its bound client adapter, was serving. Through the
// adapter, each external connection carries one client address, so automatic
// blocks on distinct public addresses show that real clients were judged
// separately. A manual block or one of unknown cause on a public address is
// not evidence either way. Any block in that window naming a private, loopback,
// node, Pod or Cilium address or a range, whatever its reason, fails: a shared
// intermediary is what this transport replaced.
//
// The window starts at Pod creation, not container start: the Pod spec fixes
// the adapter, and the Pod's initializer sets the server's listener trust, so a
// container restart in the same Pod keeps the same transport. Blocks created
// before the Pod are outside the window and remain unchanged. Expiry does not
// matter here: an expired block was still a decision about the address it
// names. The complete inventory is validated exactly as for the host
// classification first, so a partial answer is never counted.
func decodeClientIdentity(raw []byte, podCreated, now time.Time, platform ...netip.Addr) (clientIdentity, error) {
	bad := errors.New("incomplete client identity evidence")
	if podCreated.IsZero() || podCreated.After(now) || len(raw) > maxAccessOutput || len(platform) == 0 {
		return clientIdentity{}, bad
	}
	var observation accessObservation
	if strictJSON(raw, &observation) != nil {
		return clientIdentity{}, bad
	}
	for _, address := range platform {
		if !address.IsValid() {
			return clientIdentity{}, bad
		}
	}
	if _, e := effectiveHostBlock(observation.Security, platform[0], now); e != nil {
		return clientIdentity{}, bad
	}
	var envelope struct{ MethodResponses [][]json.RawMessage }
	if json.Unmarshal(observation.Security, &envelope) != nil {
		return clientIdentity{}, bad
	}
	public := map[netip.Addr]bool{}
	other := 0
	for _, call := range envelope.MethodResponses {
		var tag string
		if json.Unmarshal(call[2], &tag) != nil {
			return clientIdentity{}, bad
		}
		if tag != "block" {
			continue
		}
		var body struct {
			List []struct {
				Address           string
				Reason, CreatedAt *string
			}
		}
		if json.Unmarshal(call[1], &body) != nil {
			return clientIdentity{}, bad
		}
		for _, entry := range body.List {
			if entry.Reason == nil || !storedReasons[*entry.Reason] || entry.CreatedAt == nil {
				return clientIdentity{}, bad
			}
			created, e := time.Parse(time.RFC3339, *entry.CreatedAt)
			if e != nil || created.Unix() < 0 || created.After(now) {
				return clientIdentity{}, bad
			}
			if created.Before(podCreated) {
				continue
			}
			address, e := netip.ParseAddr(entry.Address)
			if e != nil {
				network, e := netip.ParsePrefix(entry.Address)
				if e != nil {
					return clientIdentity{}, bad
				}
				// A host-length prefix is one address; a wider range is never one client.
				if network.Bits() != network.Addr().BitLen() {
					other++
					continue
				}
				address = network.Addr()
			}
			address = address.Unmap()
			if !publicClient(address, platform) {
				other++
				continue
			}
			if automaticReasons[*entry.Reason] {
				public[address] = true
			}
		}
	}
	result := clientIdentity{Public: len(public)}
	switch {
	case other > 0:
		result.Verdict = "NON_CLIENT_ADDRESS_BLOCKED"
	case len(public) >= minimumDistinctClients:
		result.Verdict = "DISTINCT_PUBLIC_CLIENTS"
	default:
		result.Verdict = "INSUFFICIENT_EVIDENCE"
	}
	return result, nil
}

// publicClient admits only an address an external client can connect from.
func publicClient(address netip.Addr, platform []netip.Addr) bool {
	if !address.IsGlobalUnicast() || address.IsPrivate() || sharedAddressSpace.Contains(address) {
		return false
	}
	for _, known := range platform {
		if address == known.Unmap() {
			return false
		}
	}
	return true
}
