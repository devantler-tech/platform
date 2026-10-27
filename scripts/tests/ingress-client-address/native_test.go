package clientaddress_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type nativePins struct{ Cilium, Image string }

type headerPatch struct {
	Op, Path string
	Value    struct {
		Type                  string
		RequestHeaderModifier struct {
			Set    []struct{ Name, Value string }
			Remove []string
		} `yaml:"requestHeaderModifier"`
	}
}

func fixtureConfiguration(t *testing.T, listenerPort, backendPort int) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../k8s/bases/apps/private-mail-client-identity/patches/trusted-client-header.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var patches []headerPatch
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&patches); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 1 || patches[0].Op != "add" || patches[0].Path != "/spec/rules/0/filters/-" || patches[0].Value.Type != "RequestHeaderModifier" {
		t.Fatal("unsupported client identity patch")
	}
	policy := patches[0].Value.RequestHeaderModifier
	var added []any
	for _, h := range policy.Set {
		// Cilium 1.20.3 translates HTTPRoute set with this exact Envoy action.
		added = append(added, map[string]any{"header": map[string]any{"key": h.Name, "value": h.Value}, "append_action": "OVERWRITE_IF_EXISTS_OR_ADD"})
	}
	socket := func(port int) map[string]any {
		return map[string]any{"socket_address": map[string]any{"address": "127.0.0.1", "port_value": port}}
	}
	config := map[string]any{"static_resources": map[string]any{
		"listeners": []any{map[string]any{
			"name": "client-identity", "address": socket(listenerPort),
			"listener_filters": []any{map[string]any{"name": "envoy.filters.listener.proxy_protocol", "typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.listener.proxy_protocol.v3.ProxyProtocol", "allow_requests_without_proxy_protocol": false}}},
			"filter_chains": []any{map[string]any{"filters": []any{map[string]any{
				"name": "envoy.filters.network.http_connection_manager", "typed_config": map[string]any{
					"@type":       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
					"stat_prefix": "identity", "use_remote_address": true, "skip_xff_append": false, "xff_num_trusted_hops": 0,
					"route_config": map[string]any{"name": "email", "virtual_hosts": []any{map[string]any{"name": "email", "domains": []string{"*"}, "routes": []any{map[string]any{
						"match": map[string]any{"prefix": "/"}, "route": map[string]any{"cluster": "backend"},
						"request_headers_to_add": added, "request_headers_to_remove": policy.Remove,
					}}}}},
					"http_filters": []any{map[string]any{"name": "envoy.filters.http.router", "typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}},
				},
			}}}},
		}},
		"clusters": []any{map[string]any{"name": "backend", "connect_timeout": "1s", "type": "STATIC", "load_assignment": map[string]any{"cluster_name": "backend", "endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{"endpoint": map[string]any{"address": socket(backendPort)}}}}}}}},
	}}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestClientIdentityFixtureUsesTheAuditedControllerRelease(t *testing.T) {
	data, err := os.ReadFile("../../../k8s/bases/infrastructure/controllers/cilium/helm-release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var release struct {
		Spec struct {
			Chart struct {
				Spec struct{ Version string }
			}
			ValuesFrom []any `yaml:"valuesFrom"`
			Values     struct {
				Envoy      struct{ Image any }
				GatewayAPI struct {
					XffNumTrustedHops *uint32 `yaml:"xffNumTrustedHops"`
				} `yaml:"gatewayAPI"`
			}
		}
	}
	if err := yaml.Unmarshal(data, &release); err != nil || release.Spec.Chart.Spec.Version != "1.20.3" {
		t.Fatal("reaudit controller header translation and Envoy pin before updating this proof")
	}
	if len(release.Spec.ValuesFrom) != 0 || release.Spec.Values.Envoy.Image != nil || (release.Spec.Values.GatewayAPI.XffNumTrustedHops != nil && *release.Spec.Values.GatewayAPI.XffNumTrustedHops != 0) {
		t.Fatal("reaudit effective image and HTTP trust settings before changing the native proof")
	}
	var configuration map[string]any
	if err := json.Unmarshal(fixtureConfiguration(t, 11080, 18080), &configuration); err != nil {
		t.Fatal(err)
	}
}

func TestNativeGatewayReplacesSpoofedClientIdentity(t *testing.T) {
	if os.Getenv("EMAIL_CLIENT_IDENTITY_NATIVE_TEST") != "1" {
		t.Skip("opt-in native Linux container proof")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("native proof requires a Linux host, not only a Linux Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(args ...string) []byte {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("native Envoy fixture failed: %v: %.4000s", err, out)
		}
		return out
	}
	var pins nativePins
	data, err := os.ReadFile("pins.json")
	if err != nil || json.Unmarshal(data, &pins) != nil || pins.Cilium != "1.20.3" || pins.Image != "quay.io/cilium/cilium-envoy@sha256:d6c8c0c91954d1885026d82a31b341f37dcd34a4b127fdaf1587e43ddd224cef" {
		t.Fatal("native proof must use the audited Cilium Envoy pin")
	}
	if got := strings.TrimSpace(string(run("info", "--format", "{{.OSType}}"))); got != "linux" {
		t.Fatal("native fixture requires Linux host networking")
	}
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.Header)
	}))
	defer backend.Close()
	backendPort, err := strconv.Atoi(strings.Split(backend.Listener.Addr().String(), ":")[1])
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "envoy.json")
	if err := os.WriteFile(config, fixtureConfiguration(t, port, backendPort), 0o644); err != nil {
		t.Fatal(err)
	}
	run("pull", pins.Image)
	name := fmt.Sprintf("email-client-identity-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if out, err := exec.CommandContext(cleanup, "docker", "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("owned native fixture cleanup failed: %v: %.1000s", err, out)
		}
	})
	run("run", "--detach", "--name", name, "--network", "host", "--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--cpus", "1", "--memory", "256m", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16m", "--mount", "type=bind,source="+config+",target=/fixture.json,readonly", "--entrypoint", "/usr/bin/cilium-envoy", pins.Image, "-c", "/fixture.json", "--concurrency", "1", "--disable-hot-restart", "--log-level", "error")
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native Envoy did not start: %.2000s", run("logs", name))
		}
		time.Sleep(100 * time.Millisecond)
	}
	request := func(prefix string) (*http.Response, error) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = fmt.Fprintf(conn, "%sGET /admin/ HTTP/1.1\r\nHost: mail.example.test\r\nConnection: close\r\nForwarded: for=203.0.113.20\r\nForwarded: for=203.0.113.21\r\nX-Forwarded-For: 203.0.113.22, 203.0.113.23\r\nX-Real-IP: 203.0.113.24\r\nX-Envoy-External-Address: 203.0.113.25\r\n\r\n", prefix)
		if err != nil {
			return nil, err
		}
		return http.ReadResponse(bufio.NewReader(conn), nil)
	}
	for _, tc := range []struct{ protocol, source, destination string }{
		{"TCP4", "198.51.100.9", "192.0.2.9"},
		{"TCP6", "2001:db8::9", "2001:db8::10"},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			response, err := request(fmt.Sprintf("PROXY %s %s %s 4242 443\r\n", tc.protocol, tc.source, tc.destination))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			var headers http.Header
			if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&headers) != nil {
				t.Fatal("trusted transport did not reach the synthetic backend")
			}
			values := headers.Values("Forwarded")
			if len(values) != 1 || values[0] != `for="`+tc.source+`"` || headers.Get("X-Forwarded-For") != "" || headers.Get("X-Real-IP") != "" {
				t.Fatal("untrusted forwarding metadata changed the canonical client identity")
			}
		})
	}
	before := hits.Load()
	response, err := request("")
	if err == nil {
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			t.Fatal("a direct request without the transport envelope was accepted")
		}
	}
	if hits.Load() != before {
		t.Fatal("a direct request reached the backend")
	}
}
