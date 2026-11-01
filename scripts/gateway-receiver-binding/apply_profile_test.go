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

func TestProfileApplicationPreservesItsObservedBoundary(t *testing.T) {
	for _, tc := range []struct {
		name           string
		proxy, fails   bool
		writes, probes int
	}{
		{"disabled", false, false, 0, 0},
		{"needs-fence", false, false, 1, 20},
		{"lost-write-response", false, false, 1, 20},
		{"recover-active", false, false, 0, 0},
		{"gateway-lag", false, false, 0, 0},
		{"active", true, false, 0, 20},
		{"active-probe-failure", true, true, 0, 1},
		{"provider-replaced", false, true, 0, 0},
		{"receiver-replaced", false, true, 0, 0},
		{"missing-binding", false, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(filepath.Join("..", ".."))
			_, b := providerFixture()
			phase, pauses, settles, writes, probes, providerReads, receiverReads := 0, 0, 0, 0, 0, 0, 0
			c := receiverClient{context: "admin@prod"}
			c.command = func(_ context.Context, _ []byte, args ...string) ([]byte, error) {
				kind := ""
				for i, arg := range args {
					if arg == "delete" {
						t.Fatal("selected transport deleted a fence binding")
					}
					if arg == "patch" {
						writes++
						if tc.name == "lost-write-response" {
							return nil, errors.New("response-lost")
						}
						return []byte("{}"), nil
					}
					if arg == "get" {
						kind = args[i+1]
					}
				}
				active := tc.proxy || (tc.name == "recover-active" && phase == 0)
				switch kind {
				case "service":
					s := serviceFixture()
					if active {
						s["metadata"].(map[string]any)["annotations"].(map[string]any)["load-balancer.hetzner.cloud/uses-proxyprotocol"] = "true"
						for _, item := range s["status"].(map[string]any)["loadBalancer"].(map[string]any)["ingress"].([]any) {
							item.(map[string]any)["ipMode"] = "Proxy"
						}
					}
					return json.Marshal(s)
				case "gateway":
					g := strings.Replace(string(gatewayFixture()), `"gatewayClassName":"cilium"`, `"gatewayClassName":"cilium","listeners":[{"name":"https","port":443,"protocol":"HTTPS"},{"name":"http","port":80,"protocol":"HTTP","allowedRoutes":{"namespaces":{"from":"Same"}}}]`, 1)
					if tc.name == "gateway-lag" && phase == 0 {
						g = strings.ReplaceAll(g, `"observedGeneration":2`, `"observedGeneration":1`)
					}
					return []byte(g), nil
				case "secret":
					return json.Marshal(map[string]any{"metadata": map[string]string{"name": "variables-gateway-receiver", "namespace": "flux-system", "uid": "33333333-3333-4333-8333-333333333333"}, "data": map[string][]byte{"gateway_receiver_source_cidr": []byte(b.CIDR), "gateway_receiver_service_uid": []byte(b.ServiceUID), "gateway_receiver_gateway_uid": []byte(b.GatewayUID)}})
				case "ciliumenvoyconfig":
					receiverReads++
					config, _ := disabledFixture()
					setFencePeer(config, "10.11.12.13", true)
					if (tc.name == "needs-fence" || tc.name == "lost-write-response") && writes == 0 {
						setFencePeer(config, "", false)
					}
					if active {
						config["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)["listenerFilters"] = activeReceiverFilters()
					}
					if tc.name == "receiver-replaced" && receiverReads > 1 {
						config["metadata"].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
					}
					return json.Marshal(config)
				}
				for _, r := range append(append([]admissionResource{}, preparedPolicies...), preparedBindings...) {
					if r.Resource == kind {
						if tc.name == "missing-binding" && strings.HasSuffix(r.Kind, "Binding") {
							return nil, errors.New("missing")
						}
						return registryFixture(t, r, b), nil
					}
				}
				return nil, errors.New("unexpected")
			}
			c.pause = func(ctx context.Context, duration time.Duration) error {
				pauses++
				if duration == fenceSettle {
					settles++
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if tc.name == "missing-binding" || pauses > 5 {
					return context.DeadlineExceeded
				}
				if tc.name == "recover-active" || tc.name == "gateway-lag" {
					phase++
				}
				return nil
			}
			wrote, err := c.applyProfile(context.Background(), transportProfile{Fence: true, Proxy: tc.proxy}, func(_ context.Context, actual binding, want bool) (providerIdentity, error) {
				providerReads++
				if want != tc.proxy || !sameReceiver(actual, b) {
					t.Fatal("application did not retain the reviewed provider profile")
				}
				id := providerIdentity{ID: 7, NetworkID: 9}
				if tc.name == "provider-replaced" && providerReads > 1 {
					id.ID = 8
				}
				active := tc.proxy || (tc.name == "recover-active" && phase == 0)
				if active != want {
					return id, errProviderMode
				}
				return id, nil
			}, func(_ context.Context, ip string) error {
				probes++
				if ip != "192.0.2.1" {
					t.Fatal("public sampling changed its bound provider target")
				}
				if tc.name == "active-probe-failure" {
					return errors.New("public-route")
				}
				return nil
			})
			if (err != nil) != tc.fails || writes != tc.writes || probes != tc.probes || wrote != (tc.writes > 0) {
				t.Fatalf("profile application did not preserve the bounded observation: writes=%d probes=%d wrote=%t error=%v", writes, probes, wrote, err)
			}
			if tc.writes > 0 && settles != 1 {
				t.Fatal("a confirmed fence installation skipped the settling period")
			}
			if (tc.name == "recover-active" || tc.name == "gateway-lag") && phase != 1 {
				t.Fatal("known transition was not observed before application")
			}
		})
	}
}
