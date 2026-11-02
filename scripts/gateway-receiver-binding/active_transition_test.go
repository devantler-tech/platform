package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActiveConvergenceRequiresTheCompleteFencedProfile(t *testing.T) {
	for _, scenario := range []string{"converges", "unfenced", "unknown-proxy", "unsafe-http", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			t.Chdir(filepath.Join("..", ".."))
			_, b := providerFixture()
			phase := 0
			c := receiverClient{context: "admin@prod", command: func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
				kind := ""
				for i, arg := range args {
					if arg == "get" {
						kind = args[i+1]
					}
					if arg == "patch" || arg == "delete" {
						t.Fatal("active convergence changed controller configuration or its fence")
					}
				}
				switch kind {
				case "service":
					s := serviceFixture()
					if phase > 0 {
						s["metadata"].(map[string]any)["annotations"].(map[string]any)["load-balancer.hetzner.cloud/uses-proxyprotocol"] = "true"
						for _, item := range s["status"].(map[string]any)["loadBalancer"].(map[string]any)["ingress"].([]any) {
							item.(map[string]any)["ipMode"] = "Proxy"
						}
					}
					return json.Marshal(s)
				case "gateway":
					from := "Same"
					if scenario == "unsafe-http" {
						from = "All"
					}
					return []byte(strings.Replace(string(gatewayFixture()), `"gatewayClassName":"cilium"`, `"gatewayClassName":"cilium","listeners":[{"name":"https","port":443,"protocol":"HTTPS"},{"name":"http","port":80,"protocol":"HTTP","allowedRoutes":{"namespaces":{"from":"`+from+`"}}}]`, 1)), nil
				case "secret":
					return json.Marshal(map[string]any{"metadata": map[string]string{"name": "variables-gateway-receiver", "namespace": "flux-system", "uid": "33333333-3333-4333-8333-333333333333"}, "data": map[string][]byte{"gateway_receiver_source_cidr": []byte(b.CIDR), "gateway_receiver_service_uid": []byte(b.ServiceUID), "gateway_receiver_gateway_uid": []byte(b.GatewayUID)}})
				case "ciliumenvoyconfig":
					config, _ := disabledFixture()
					setFencePeer(config, "10.11.12.13", true)
					listener := config["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
					if phase > 0 {
						listener["listenerFilters"] = activeReceiverFilters()
					}
					if scenario == "unfenced" {
						setFencePeer(config, "", false)
					}
					if scenario == "unknown-proxy" {
						listener["listenerFilters"] = []any{map[string]any{"name": "envoy.filters.listener.proxy_protocol", "typedConfig": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.listener.proxy_protocol.v3.ProxyProtocol", "allowRequestsWithoutProxyProtocol": true}}}
					}
					return json.Marshal(config)
				}
				for _, r := range append(append([]admissionResource{}, preparedPolicies...), preparedBindings...) {
					if r.Resource == kind {
						return registryFixture(t, r, b), nil
					}
				}
				return nil, errors.New("unexpected")
			}, pause: func(context.Context, time.Duration) error {
				phase++
				if phase > 1 || scenario == "deadline" {
					return context.DeadlineExceeded
				}
				return nil
			}}
			_, err := c.awaitTransport(context.Background(), true, func(_ context.Context, _ binding, want bool) (providerIdentity, error) {
				if !want {
					t.Fatal("active source requested disabled provider proof")
				}
				id := providerIdentity{ID: 7, NetworkID: 9}
				if phase == 0 {
					return id, errProviderMode
				}
				return id, nil
			})
			if (err == nil) != (scenario == "converges") {
				t.Fatalf("active verification accepted an incomplete or unknown transport profile: %v", err)
			}
		})
	}
}
