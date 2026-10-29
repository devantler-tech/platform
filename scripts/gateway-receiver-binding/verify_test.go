package main

import (
	"context"
	"encoding/json"
	"gopkg.in/yaml.v3"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadbackRejectsAChangedSourceOrBroaderPolicy(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	b := binding{CIDR: "10.11.12.13/32", ServiceUID: "22222222-2222-4222-8222-222222222222", GatewayUID: "11111111-1111-4111-8111-111111111111"}
	source, err := os.ReadFile("k8s/providers/hetzner/infrastructure/cilium/cilium-clusterwide-network-policy-gateway-receiver.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if yaml.Unmarshal([]byte(strings.ReplaceAll(string(source), "${gateway_receiver_source_cidr}", b.CIDR)), &policy) != nil {
		t.Fatal("fixture")
	}
	raw, _ := json.Marshal(policy)
	if !policyMatches(raw, b) {
		t.Fatal("declared policy refused")
	}
	for _, mutate := range []func(map[string]any){
		func(o map[string]any) { o["spec"].(map[string]any)["endpointSelector"] = map[string]any{} },
		func(o map[string]any) {
			o["spec"].(map[string]any)["ingress"] = []any{map[string]any{"fromEntities": []any{"all"}}}
		},
		func(o map[string]any) {
			o["spec"].(map[string]any)["ingress"].([]any)[0].(map[string]any)["fromCIDRSet"] = []any{map[string]any{"cidr": "10.0.0.0/8"}}
		},
		func(o map[string]any) {
			o["spec"].(map[string]any)["enableDefaultDeny"] = map[string]any{"ingress": false}
		},
	} {
		var changed map[string]any
		_ = json.Unmarshal(raw, &changed)
		mutate(changed)
		encoded, _ := json.Marshal(changed)
		if policyMatches(encoded, b) {
			t.Fatal("changed or broader policy accepted")
		}
	}
	secret := map[string]any{"metadata": map[string]string{"name": "variables-gateway-receiver", "namespace": "flux-system", "uid": "33333333-3333-4333-8333-333333333333"}, "data": map[string][]byte{"gateway_receiver_source_cidr": []byte(b.CIDR), "gateway_receiver_service_uid": []byte(b.ServiceUID), "gateway_receiver_gateway_uid": []byte(b.GatewayUID)}}
	encoded, _ := json.Marshal(secret)
	if !secretMatches(encoded, b) {
		t.Fatal("matching private readback refused")
	}
	secret["data"].(map[string][]byte)["gateway_receiver_source_cidr"] = []byte("10.0.0.0/8")
	encoded, _ = json.Marshal(secret)
	if secretMatches(encoded, b) {
		t.Fatal("broader source readback accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPublicGateRejectsPolicyDenialsAndServerFailures(t *testing.T) {
	for _, status := range []int{200, 302, 303, 403, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Scheme != "https" || (r.URL.Host != "platform.devantler.tech" && r.URL.Host != "vault.platform.devantler.tech") {
					t.Fatal("unapproved public probe")
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("private response must never be reflected")), Request: r}, nil
			})}
			err := probePublicWithClient(context.Background(), client)
			want := status == 200 || status == 302 || status == 303
			if (err == nil) != want {
				t.Fatal("public gateway failure became a passing source gate")
			}
			if want && calls != 2 {
				t.Fatal("not every declared public route was exercised")
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("response content escaped the closed failure code")
			}
		})
	}
}
func TestEncryptedOutputUsesOnlyTheApprovedProductionRecipient(t *testing.T) {
	b := binding{CIDR: "10.11.12.13/32", ServiceUID: "22222222-2222-4222-8222-222222222222", GatewayUID: "11111111-1111-4111-8111-111111111111"}
	fixture := map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": map[string]string{"name": "variables-gateway-receiver", "namespace": "flux-system"}, "stringData": map[string]string{"gateway_receiver_source_cidr": "ENC[AES256_GCM,data:synthetic]", "gateway_receiver_service_uid": "ENC[AES256_GCM,data:synthetic]", "gateway_receiver_gateway_uid": "ENC[AES256_GCM,data:synthetic]"}, "sops": map[string]any{"age": []any{map[string]string{"recipient": recipient}}, "mac": "ENC[AES256_GCM,data:synthetic]"}}
	raw, _ := yaml.Marshal(fixture)
	if !encrypted(raw, b) {
		t.Fatal("encrypted fixture structure refused")
	}
	fixture["stringData"].(map[string]string)["gateway_receiver_source_cidr"] = b.CIDR
	raw, _ = yaml.Marshal(fixture)
	if encrypted(raw, b) {
		t.Fatal("plaintext source could enter the repository")
	}
	fixture["stringData"].(map[string]string)["gateway_receiver_source_cidr"] = "ENC[AES256_GCM,data:synthetic]"
	fixture["sops"].(map[string]any)["age"] = []any{map[string]string{"recipient": "other-recipient"}}
	raw, _ = yaml.Marshal(fixture)
	if encrypted(raw, b) {
		t.Fatal("unapproved recipient accepted")
	}
}
