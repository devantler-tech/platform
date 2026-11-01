package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestProviderConvergenceStillRequiresTwoBoundIdentityReads(t *testing.T) {
	for _, scenario := range []string{"active", "mode-changed", "identity-changed", "destination-changed"} {
		t.Run(scenario, func(t *testing.T) {
			lb, b := providerFixture()
			if scenario != "mode-changed" {
				for _, item := range lb["services"].([]any) {
					item.(map[string]any)["proxyprotocol"] = true
				}
			}
			client, calls := providerHTTP(t, lb)
			transport := client.Transport
			client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v1/load_balancers/7" {
					switch scenario {
					case "mode-changed":
						lb["services"].([]any)[0].(map[string]any)["proxyprotocol"] = true
					case "identity-changed":
						lb["id"] = 8
					case "destination-changed":
						lb["services"].([]any)[0].(map[string]any)["destination_port"] = 80
					}
				}
				return transport.RoundTrip(r)
			})
			got, err := observeProvider(context.Background(), client, "disposable-test-token", b, false)
			pending := scenario == "active" || scenario == "mode-changed"
			if err == nil || errors.Is(err, errProviderMode) != pending || *calls != 2 {
				t.Fatal("mode convergence was not distinguished from a changed or unverified provider")
			}
			if pending && got != (providerIdentity{ID: 7, NetworkID: 9}) {
				t.Fatal("pending mode lost the independently re-read provider identity")
			}
		})
	}
}
