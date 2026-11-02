package main

import (
	"encoding/json"
	"errors"
	"net/netip"
	"time"
)

type blockDetails struct{ Reason, Scope, Expiry, Creation string }

func podCreationTime(raw []byte) (time.Time, error) {
	var p struct {
		Metadata struct{ CreationTimestamp string }
	}
	if uniqueJSON(raw) != nil || json.Unmarshal(raw, &p) != nil {
		return time.Time{}, errors.New("invalid Pod creation")
	}
	created, e := time.Parse(time.RFC3339, p.Metadata.CreationTimestamp)
	if e != nil || created.Unix() < 0 {
		return time.Time{}, errors.New("invalid Pod creation")
	}
	return created, nil
}

// decodeBlockDetails describes stored metadata, not the actor that created it.
// Its complete inventory validation is identical to the existing address classification.
func decodeBlockDetails(raw []byte, candidate netip.Addr, podCreated, now time.Time) (blockDetails, error) {
	bad := errors.New("incomplete block metadata")
	if podCreated.IsZero() || podCreated.After(now) || len(raw) > maxAccessOutput {
		return blockDetails{}, bad
	}
	var observation accessObservation
	if strictJSON(raw, &observation) != nil {
		return blockDetails{}, bad
	}
	access, e := effectiveHostBlock(observation.Security, candidate, now)
	if e != nil {
		return blockDetails{}, bad
	}
	var envelope struct{ MethodResponses [][]json.RawMessage }
	if json.Unmarshal(observation.Security, &envelope) != nil {
		return blockDetails{}, bad
	}
	names := map[string]string{"rcptToFailure": "RCPT_TO_FAILURE", "authFailure": "AUTH_FAILURE", "loitering": "LOITERING", "portScanning": "PORT_SCANNING", "manual": "MANUAL", "other": "OTHER"}
	matches := 0
	result := blockDetails{}
	for _, call := range envelope.MethodResponses {
		var tag string
		if json.Unmarshal(call[2], &tag) != nil {
			return blockDetails{}, bad
		}
		if tag != "block" {
			continue
		}
		var body struct {
			List []struct {
				Address                      string
				Reason, CreatedAt, ExpiresAt *string
			}
		}
		if json.Unmarshal(call[1], &body) != nil {
			return blockDetails{}, bad
		}
		for _, entry := range body.List {
			if entry.Reason == nil || entry.CreatedAt == nil || names[*entry.Reason] == "" {
				return blockDetails{}, bad
			}
			created, e := time.Parse(time.RFC3339, *entry.CreatedAt)
			if e != nil || created.Unix() < 0 || created.After(now) {
				return blockDetails{}, bad
			}
			expiry := "NO_EXPIRY"
			if entry.ExpiresAt != nil {
				end, e := time.Parse(time.RFC3339, *entry.ExpiresAt)
				if e != nil || end.Unix() < 0 {
					return blockDetails{}, bad
				}
				if !end.After(now) {
					continue
				}
				expiry = "FINITE"
			}
			var network netip.Prefix
			if ip, e := netip.ParseAddr(entry.Address); e == nil {
				network = netip.PrefixFrom(ip, ip.BitLen())
			} else {
				network, e = netip.ParsePrefix(entry.Address)
				if e != nil {
					return blockDetails{}, bad
				}
			}
			if !network.Contains(candidate) {
				continue
			}
			matches++
			scope := "SINGLE_IP"
			if network.Bits() != network.Addr().BitLen() {
				scope = "NETWORK_PREFIX"
			}
			creation := "DURING_CURRENT_POD"
			if created.Before(podCreated) {
				creation = "BEFORE_CURRENT_POD"
			}
			result = blockDetails{Reason: names[*entry.Reason], Scope: scope, Expiry: expiry, Creation: creation}
		}
	}
	if access != "STORED_HOST_BLOCK_MATCH" {
		return blockDetails{Reason: "NOT_APPLICABLE", Scope: "NOT_APPLICABLE", Expiry: "NOT_APPLICABLE", Creation: "NOT_APPLICABLE"}, nil
	}
	if matches == 0 {
		return blockDetails{}, bad
	}
	if matches > 1 {
		return blockDetails{Reason: "MULTIPLE_MATCHES", Scope: "MULTIPLE_MATCHES", Expiry: "MULTIPLE_MATCHES", Creation: "MULTIPLE_MATCHES"}, nil
	}
	return result, nil
}
