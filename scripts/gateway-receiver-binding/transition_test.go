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

func TestDisabledRecoveryWaitsOnlyForBoundTransportConvergence(t *testing.T) {
	for _, scenario := range []string{"disabled", "receiver-converges", "provider-converges", "intent-converges", "status-converges", "gateway-converges", "provider-unknown", "receiver-unknown", "provider-replaced", "receiver-replaced", "binding-replaced", "source-replaced", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			t.Chdir(filepath.Join("..", ".."))
			_, b := providerFixture()
			phase, pauses := 0, 0
			c := receiverClient{context: "admin@prod", command: func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
				kind := ""
				for i, arg := range args {
					if arg == "get" {
						kind = args[i+1]
					}
					if arg == "patch" || arg == "delete" {
						t.Fatal("transport convergence changed the listener or its fence")
					}
				}
				switch kind {
				case "service":
					s := serviceFixture()
					if phase > 0 {
						s["metadata"].(map[string]any)["resourceVersion"] = "56"
					}
					if phase == 0 && scenario == "intent-converges" {
						s["metadata"].(map[string]any)["annotations"].(map[string]any)["load-balancer.hetzner.cloud/uses-proxyprotocol"] = "true"
					}
					if phase == 0 && (scenario == "intent-converges" || scenario == "status-converges") {
						for _, item := range s["status"].(map[string]any)["loadBalancer"].(map[string]any)["ingress"].([]any) {
							item.(map[string]any)["ipMode"] = "Proxy"
						}
					}
					if phase > 0 && scenario == "source-replaced" {
						s["metadata"].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
					}
					return json.Marshal(s)
				case "gateway":
					if phase == 0 && scenario == "gateway-converges" {
						return []byte(strings.ReplaceAll(string(gatewayFixture()), `"observedGeneration":2`, `"observedGeneration":1`)), nil
					}
					return gatewayFixture(), nil
				case "secret":
					return json.Marshal(map[string]any{"metadata": map[string]string{"name": "variables-gateway-receiver", "namespace": "flux-system", "uid": "33333333-3333-4333-8333-333333333333"}, "data": map[string][]byte{"gateway_receiver_source_cidr": []byte(b.CIDR), "gateway_receiver_service_uid": []byte(b.ServiceUID), "gateway_receiver_gateway_uid": []byte(b.GatewayUID)}})
				case "ciliumenvoyconfig":
					config, _ := disabledFixture()
					setFencePeer(config, "10.11.12.13", true)
					listener := config["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)
					if phase == 0 && scenario != "disabled" && scenario != "provider-converges" && scenario != "status-converges" && scenario != "intent-converges" {
						listener["listenerFilters"] = activeReceiverFilters()
					}
					if scenario == "receiver-unknown" {
						listener["listenerFilters"] = []any{map[string]any{"name": "envoy.filters.listener.proxy_protocol", "typedConfig": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.listener.proxy_protocol.v3.ProxyProtocol", "allowRequestsWithoutProxyProtocol": true}}}
					}
					if phase > 0 && scenario == "receiver-replaced" {
						config["metadata"].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
					}
					return json.Marshal(config)
				}
				for _, resource := range append(append([]admissionResource{}, preparedPolicies...), preparedBindings...) {
					if resource.Resource == kind {
						raw := registryFixture(t, resource, b)
						if phase > 0 && scenario == "binding-replaced" && strings.HasSuffix(resource.Kind, "Binding") {
							return []byte(strings.Replace(string(raw), `"resourceVersion":"10"`, `"resourceVersion":"11"`, 1)), nil
						}
						return raw, nil
					}
				}
				t.Fatal("unexpected recovery read")
				return nil, errors.New("unexpected")
			}, pause: func(context.Context, time.Duration) error {
				pauses++
				if pauses > 2 || scenario == "deadline" {
					return context.DeadlineExceeded
				}
				phase++
				return nil
			}}
			provider := func(_ context.Context, actual binding, want bool) (providerIdentity, error) {
				if want || actual.HTTPNodePort != 31080 {
					t.Fatal("recovery did not request its bound disabled provider")
				}
				if scenario == "provider-unknown" {
					return providerIdentity{}, errors.New("provider-document")
				}
				id := providerIdentity{ID: 7, NetworkID: 9}
				if phase > 0 && scenario == "provider-replaced" {
					id.ID = 8
				}
				if phase == 0 && scenario == "provider-converges" {
					return id, errProviderMode
				}
				return id, nil
			}
			got, err := c.awaitDisabledTransport(context.Background(), provider)
			ok := scenario == "disabled" || scenario == "receiver-converges" || scenario == "provider-converges" || scenario == "intent-converges" || scenario == "status-converges" || scenario == "gateway-converges"
			if (err == nil) != ok {
				t.Fatalf("recovery misclassified known convergence or unknown state: %v", err)
			}
			if phase > 0 {
				b.ServiceVersion = "56"
			}
			if ok && (got.Binding != b || got.Provider != (providerIdentity{ID: 7, NetworkID: 9}) || got.ReceiverUID != "22222222-2222-4222-8222-222222222222") {
				t.Fatal("recovery lost its original bound identities")
			}
			if ok && scenario != "disabled" && pauses != 1 {
				t.Fatal("recovery did not wait for the known pending state exactly once")
			}
		})
	}
}
