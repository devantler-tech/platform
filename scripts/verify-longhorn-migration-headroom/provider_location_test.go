package main

import (
	"context"
	"testing"
)

// Model the current HTTP wire format independently of server's JSON tags.
// Hetzner removed datacenter; its replacement is the top-level location.
func TestAccountCensusUsesCurrentProviderLocation(t *testing.T) {
	body := providerResponse(t, func(response map[string]any) {
		for _, value := range response["servers"].([]any) {
			entry := value.(map[string]any)
			delete(entry, "datacenter")
			entry["location"] = map[string]any{"name": "fsn1"}
		}
	})
	_, nodes := roster(t, false)
	read := func(context.Context) ([]byte, error) { return nodeResponse(t, nodes), nil }
	got, err := observeAccountCensus(t.Context(), providerClient(t, body, 200), "private-test-token", read)
	want := accountCensus{Servers: 8, Nodes: 8, Classification: "none", State: "none"}
	if err != nil || got != want {
		t.Fatalf("current provider location did not reconcile: got=%+v err=%v", got, err)
	}
}

func TestAccountCensusRefusesUnverifiedCurrentLocation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		location any
	}{
		{"absent", nil},
		{"empty", map[string]any{"name": ""}},
		{"wrong region", map[string]any{"name": "hel1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := providerResponse(t, func(response map[string]any) {
				for _, value := range response["servers"].([]any) {
					entry := value.(map[string]any)
					delete(entry, "location")
					if tc.location != nil {
						entry["location"] = tc.location
					}
					// A legacy value must not substitute for missing or inconsistent
					// evidence from the field the provider currently serves.
					entry["datacenter"] = map[string]any{"location": map[string]any{"name": "fsn1"}}
				}
			})
			_, nodes := roster(t, false)
			read := func(context.Context) ([]byte, error) { return nodeResponse(t, nodes), nil }
			got, err := observeAccountCensus(t.Context(), providerClient(t, body, 200), "private-test-token", read)
			if err == nil || got != (accountCensus{}) {
				t.Fatalf("unverified current location produced a receipt: got=%+v err=%v", got, err)
			}
		})
	}
}
