package gatewayreceiver_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registration "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

// Emit the listener produced by the actual reviewed Kubernetes admission
// expressions. Every address, route and certificate below is synthetic.
func nativeEnvoyConfiguration(t *testing.T) []byte {
	t.Helper()
	load := func(name string, target any) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s/providers/hetzner/infrastructure/cilium/gateway-receiver", name))
		if err != nil {
			t.Fatal(err)
		}
		raw = []byte(strings.NewReplacer("${gateway_receiver_source_cidr}", "127.0.0.1/32", "${gateway_receiver_gateway_uid}", "11111111-1111-4111-8111-111111111111").Replace(string(raw)))
		if yaml.UnmarshalStrict(raw, target) != nil {
			t.Fatal("native policy decode")
		}
	}
	var mutation registration.MutatingAdmissionPolicy
	var validation registration.ValidatingAdmissionPolicy
	load("mutating-admission-policy-gateway-receiver.yaml", &mutation)
	load("validating-admission-policy-gateway-receiver.yaml", &validation)
	obj := fixture()
	l := listener(obj)
	l["name"] = "listener"
	l["address"] = map[string]any{"socketAddress": map[string]any{"address": "127.0.0.1", "portValue": int64(18443)}}
	l["listenerFilters"] = []any{
		map[string]any{"name": "envoy.filters.listener.proxy_protocol", "typedConfig": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.listener.proxy_protocol.v3.ProxyProtocol"}},
		map[string]any{"name": "envoy.filters.listener.tls_inspector", "typedConfig": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector"}},
	}
	filters := func(tls bool) []any {
		route := map[string]any{"match": map[string]any{"prefix": "/"}}
		if tls {
			route["directResponse"] = map[string]any{"status": 200, "body": map[string]any{"inlineString": "native-peer-proof"}}
		} else {
			route["redirect"] = map[string]any{"schemeRedirect": "https", "responseCode": "PERMANENT_REDIRECT"}
		}
		return []any{map[string]any{"name": "envoy.filters.network.http_connection_manager", "typedConfig": map[string]any{
			"@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager", "statPrefix": "email_peer_fixture",
			"useRemoteAddress": true, "xffNumTrustedHops": int64(0), "skipXffAppend": false,
			"routeConfig": map[string]any{"name": "fixture", "virtualHosts": []any{map[string]any{"name": "fixture", "domains": []any{"*"}, "routes": []any{route}, "responseHeadersToAdd": []any{map[string]any{"header": map[string]any{"key": "x-observed-client", "value": "%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT%"}, "appendAction": "OVERWRITE_IF_EXISTS_OR_ADD"}}}}},
			"httpFilters": []any{map[string]any{"name": "envoy.filters.http.router", "typedConfig": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}},
		}}}
	}
	chains := l["filterChains"].([]any)
	for index, raw := range chains {
		chain := raw.(map[string]any)
		chain["filters"] = filters(index != 0)
		if index != 0 {
			chain["transportSocket"] = map[string]any{"name": "envoy.transport_sockets.tls", "typedConfig": map[string]any{
				"@type":            "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext",
				"commonTlsContext": map[string]any{"tlsCertificates": []any{map[string]any{"certificateChain": map[string]any{"filename": "/tmp/email-proxy-native/certificate.pem"}, "privateKey": map[string]any{"filename": "/tmp/email-proxy-native/private-key.pem"}}}},
			}}
		}
	}
	chains[1].(map[string]any)["filterChainMatch"].(map[string]any)["serverNames"] = []any{"mail.example.test"}
	encoded, err := json.Marshal(obj.Object)
	if err != nil || json.Unmarshal(encoded, &obj.Object) != nil {
		t.Fatal("native fixture JSON normalization")
	}
	admitted := mutate(t, mutation, obj)
	if !validates(t, validation, admitted) {
		t.Fatal("native admission refused emitted listener")
	}
	result := listener(admitted)
	delete(result, "@type")
	config := map[string]any{"staticResources": map[string]any{"listeners": []any{result}}}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal("native fixture output")
	}
	return raw
}
