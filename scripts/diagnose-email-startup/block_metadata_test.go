package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func TestNativeBlockMetadataClassifications(t *testing.T) {
	created := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	raw := accessMeasurement(t, nativeBlockMetadata(t))
	reasons := []string{"RCPT_TO_FAILURE", "AUTH_FAILURE", "LOITERING", "PORT_SCANNING", "MANUAL", "OTHER"}
	for i, reason := range reasons {
		got, e := decodeBlockDetails(raw, netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 30+i)), created, accessNow)
		want := blockDetails{Reason: reason, Scope: "SINGLE_IP", Expiry: "NO_EXPIRY", Creation: "DURING_CURRENT_POD"}
		if e != nil || got != want {
			t.Fatalf("native %s classification %v/%v", reason, got, e)
		}
	}
	got, e := decodeBlockDetails(raw, netip.MustParseAddr("198.51.100.130"), created, accessNow)
	if e != nil || got.Reason != "MANUAL" || got.Scope != "NETWORK_PREFIX" {
		t.Fatalf("native prefix %v/%v", got, e)
	}
}

func TestBlockMetadataRetainsExpiryOverrideAndMultiplicity(t *testing.T) {
	candidate := netip.MustParseAddr("192.0.2.30")
	created := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"before-pod", "finite", "during-pod", "expired", "expiry-before-created", "allow-override", "nonmatching", "multiple", "single-ip-prefix"} {
		t.Run(scenario, func(t *testing.T) {
			entry := accessEntry("192.0.2.30", nil)
			allow := []any{}
			blocks := []any{entry}
			want := blockDetails{Reason: "MANUAL", Scope: "SINGLE_IP", Expiry: "NO_EXPIRY", Creation: "BEFORE_CURRENT_POD"}
			switch scenario {
			case "finite":
				entry["expiresAt"] = "2026-10-28T00:00:00Z"
				want.Expiry = "FINITE"
			case "during-pod":
				entry["createdAt"] = "2026-10-26T00:00:00Z"
				want.Creation = "DURING_CURRENT_POD"
			case "expired", "expiry-before-created":
				entry["expiresAt"] = "2026-10-26T00:00:00Z"
				if scenario == "expiry-before-created" {
					entry["createdAt"] = "2026-10-27T00:00:00Z"
				}
			case "allow-override":
				allow = []any{accessEntry("192.0.2.30", nil)}
			case "nonmatching":
				entry["address"] = "192.0.2.31"
			case "multiple":
				extra := accessEntry("192.0.2.28/30", nil)
				extra["id"] = "second"
				blocks = append(blocks, extra)
				want = blockDetails{"MULTIPLE_MATCHES", "MULTIPLE_MATCHES", "MULTIPLE_MATCHES", "MULTIPLE_MATCHES"}
			case "single-ip-prefix":
				entry["address"] = "192.0.2.30/32"
			}
			if scenario == "expired" || scenario == "expiry-before-created" || scenario == "allow-override" || scenario == "nonmatching" {
				want = blockDetails{"NOT_APPLICABLE", "NOT_APPLICABLE", "NOT_APPLICABLE", "NOT_APPLICABLE"}
			}
			raw := accessMeasurement(t, securityStateFor(t, allow, blocks))
			got, e := decodeBlockDetails(raw, candidate, created, accessNow)
			if e != nil || got != want {
				t.Fatalf("classification %v/%v, want %v", got, e, want)
			}
		})
	}
}

func TestBlockMetadataRefusesUnknownMissingAndInvalidRecords(t *testing.T) {
	for _, scenario := range []string{"missing-reason", "null-reason", "unknown-reason", "missing-created", "null-created", "invalid-created", "future-created", "extra-field"} {
		t.Run(scenario, func(t *testing.T) {
			var envelope map[string]any
			if json.Unmarshal(nativeBlockMetadata(t), &envelope) != nil {
				t.Fatal("invalid native fixture")
			}
			responses := envelope["methodResponses"].([]any)
			for _, value := range responses {
				call := value.([]any)
				if call[2] != "block" {
					continue
				}
				entry := call[1].(map[string]any)["list"].([]any)[0].(map[string]any)
				switch scenario {
				case "missing-reason":
					delete(entry, "reason")
				case "null-reason":
					entry["reason"] = nil
				case "unknown-reason":
					entry["reason"] = "PRIVATE_UNKNOWN_REASON"
				case "missing-created":
					delete(entry, "createdAt")
				case "null-created":
					entry["createdAt"] = nil
				case "invalid-created":
					entry["createdAt"] = "PRIVATE_INVALID_DATE"
				case "future-created":
					entry["createdAt"] = "2027-01-01T00:00:00Z"
				case "extra-field":
					entry["unrequested"] = "PRIVATE_UNUSED"
				}
			}
			b, _ := json.Marshal(envelope)
			_, e := decodeBlockDetails(accessMeasurement(t, b), netip.MustParseAddr("192.0.2.30"), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), accessNow)
			if e == nil {
				t.Fatal("incomplete or invalid block metadata accepted")
			}
		})
	}
}

func TestPodCreationRefusesMissingChangedOrInvalidTime(t *testing.T) {
	for _, value := range []any{nil, "", "bad", "1960-01-01T00:00:00Z"} {
		raw := changePod(t, accessPod(t), func(p map[string]any) { p["metadata"].(map[string]any)["creationTimestamp"] = value })
		if _, e := podCreationTime(raw); e == nil {
			t.Fatal("invalid Pod creation accepted")
		}
	}
	if _, e := decodeBlockDetails(accessMeasurement(t, nativeBlockMetadata(t)), netip.MustParseAddr("192.0.2.30"), accessNow.Add(time.Hour), accessNow); e == nil {
		t.Fatal("future Pod creation accepted")
	}
}
