package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var accessNow = time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)

// TestSupportedProbeMinorTracksDesiredKubernetes prevents a pin bump leaving the observer stale.
func TestSupportedProbeMinorTracksDesiredKubernetes(t *testing.T) {
	b, e := os.ReadFile("../../ksail.prod.yaml")
	if e != nil {
		t.Fatal(e)
	}
	var desired struct {
		Spec struct {
			Cluster struct {
				KubernetesVersion string `yaml:"kubernetesVersion"`
			}
		}
	}
	if yaml.Unmarshal(b, &desired) != nil || !supportedKubelet.MatchString(desired.Spec.Cluster.KubernetesVersion) {
		t.Fatal("desired Kubernetes minor requires a reviewed probe header update")
	}
}

// TestAdministratorShapeRetainsOnlyTheRequestedPassword rejects absent and extra fields.
func TestAdministratorShapeRetainsOnlyTheRequestedPassword(t *testing.T) {
	password := []byte(strings.Repeat("S", 64))
	raw := accessSecret(t, "private-mail-admin", "password", password)
	value, _, e := decodeSecret(raw, "private-mail-admin", "password")
	if e != nil || !bytes.Equal(value, password) {
		t.Fatal("existing two-key administrator Secret was rejected")
	}
	for _, scenario := range []string{"missing-credential", "extra-key", "missing-password"} {
		var object map[string]any
		if json.Unmarshal(raw, &object) != nil {
			t.Fatal("invalid fixture")
		}
		data := object["data"].(map[string]any)
		switch scenario {
		case "missing-credential":
			delete(data, "credential")
		case "extra-key":
			data["unexpected"] = base64.StdEncoding.EncodeToString([]byte("PRIVATE_UNUSED"))
		case "missing-password":
			delete(data, "password")
		}
		bad, _ := json.Marshal(object)
		if _, _, e := decodeSecret(bad, "private-mail-admin", "password"); e == nil {
			t.Fatalf("unsafe Secret shape accepted: %s", scenario)
		}
	}
}

// TestQueryAndGetIdentityAgreementRefusesSilentTruncation covers the server's implicit get limit.
func TestQueryAndGetIdentityAgreementRefusesSilentTruncation(t *testing.T) {
	valid := string(nativeSecurityState(t))
	for _, raw := range []string{
		strings.Replace(valid, `"total":1`, `"total":2`, 1),
		strings.Replace(valid, `"position":0`, `"position":1`, 1),
		strings.Replace(valid, `"canCalculateChanges":true`, `"canCalculateChanges":null`, 1),
		strings.Replace(valid, `"jlql9b11aaaa"`, `"missing"`, 1),
		strings.Replace(valid, `"expiresAt":null`, `"expiresAt":"1960-01-01T00:00:00Z"`, 1),
	} {
		if raw == valid {
			t.Fatal("negative control failed to change the captured response")
		}
		if _, e := effectiveHostBlock([]byte(raw), netip.MustParseAddr("192.0.2.10"), accessNow); e == nil {
			t.Fatal("incomplete or mismatched query/get accepted")
		}
	}
}

// TestReaderInputsRejectAlternateCredentialsAndOutputChannels keeps the fixed identity contract.
func TestReaderInputsRejectAlternateCredentialsAndOutputChannels(t *testing.T) {
	password := []byte(strings.Repeat("S", 64))
	for _, settings := range []string{
		`{}`, `{"domain":"example.test","administrator":"name\nheader"}`,
		`{"domain":"example.test","administrator":"admin","unexpected":"value"}`,
		`{"domain":"example.test","domain":"other.test","administrator":"admin"}`,
	} {
		if _, e := readerAuthorization([]byte(settings), password); e == nil {
			t.Fatal("invalid administrative input accepted")
		}
	}
	valid := []byte(`{"domain":"example.test","administrator":"admin"}`)
	for _, bad := range [][]byte{nil, []byte("short"), []byte(strings.Repeat("S", 64) + "\n")} {
		if _, e := readerAuthorization(valid, bad); e == nil {
			t.Fatal("invalid password accepted")
		}
	}
	raw := accessSecret(t, "private-mail-admin", "password", password)
	for _, bad := range [][]byte{
		[]byte(strings.Replace(string(raw), "private-mail-admin", "different-secret", 1)),
		[]byte(strings.Replace(string(raw), "private-mail", "different-namespace", 1)),
		[]byte(strings.Replace(string(raw), "00000000-0000-0000-0000-000000000004", "invalid", 1)),
		[]byte(strings.Replace(string(raw), `"123"`, `""`, 1)),
	} {
		if _, _, e := decodeSecret(bad, "private-mail-admin", "password"); e == nil {
			t.Fatal("unbound secret accepted")
		}
	}
}

// TestEmbeddedAccessProbeUsesRealHTTPAndOnlyTheTwoReadMethods exercises curl's actual config/HTTP encoding.
func TestEmbeddedAccessProbeUsesRealHTTPAndOnlyTheTwoReadMethods(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			password := strings.Repeat("S", 64)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/healthz/live", "/healthz/ready":
					if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("User-Agent") != "kube-probe/1.36" || r.Header.Get("Accept") != "*/*" || !r.Close {
						t.Error("health request differs from fixed probe")
					}
					w.WriteHeader(status)
				case "/jmap":
					user, gotPassword, ok := r.BasicAuth()
					if r.Method != "POST" || !ok || user != "administrator@example.test" || gotPassword != password {
						t.Error("fixed authenticated read was not received")
					}
					b, e := io.ReadAll(io.LimitReader(r.Body, 8193))
					if e != nil || len(b) > 8192 {
						t.Error("invalid request body")
						return
					}
					var request struct {
						Using       []string
						MethodCalls [][]json.RawMessage
					}
					if json.Unmarshal(b, &request) != nil || !reflect.DeepEqual(request.Using, []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"}) || len(request.MethodCalls) != 4 {
						t.Error("invalid bounded read request")
						return
					}
					for i, call := range request.MethodCalls {
						if len(call) != 3 {
							t.Error("invalid method tuple")
							return
						}
						var method string
						_ = json.Unmarshal(call[0], &method)
						expected := []string{"x:AllowedIp/query", "x:AllowedIp/get", "x:BlockedIp/query", "x:BlockedIp/get"}[i]
						if method != expected {
							t.Error("unapproved method")
							return
						}
						var arguments map[string]any
						if json.Unmarshal(call[1], &arguments) != nil {
							t.Error("invalid method arguments")
							return
						}
						if i%2 == 0 {
							if len(arguments) != 2 || arguments["limit"] != float64(257) || arguments["calculateTotal"] != true {
								t.Error("unbounded query")
							}
						} else {
							if len(arguments) != 2 || !reflect.DeepEqual(arguments["properties"], []any{"address", "expiresAt"}) || arguments["#ids"] == nil {
								t.Error("unapproved get")
							}
						}
					}
					_, _ = w.Write(nativeSecurityState(t))
				default:
					t.Error("unapproved URL")
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			script := strings.ReplaceAll(accessProbe, "http://127.0.0.1:8080", server.URL)
			script = strings.Replace(script, `task_pod_url="http://$task_pod_ip:8080"`, `task_pod_url="`+server.URL+`"`, 1)
			cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-euo", "pipefail", "-c", script)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
			cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString([]byte("administrator@example.test:"+password)) + "\n192.0.2.20\n")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			b, e := cmd.Output()
			if e != nil || stderr.Len() != 0 {
				t.Fatalf("embedded probe failed: %v", e)
			}
			got, e := decodeAccess(b, netip.MustParseAddr("192.0.2.10"), accessNow)
			expectedPaths := "ALL_LOCAL_TARGETS_HTTP_200"
			if status == 503 {
				expectedPaths = "LOOPBACK_PATH_FAILED"
			}
			if e != nil || got.Paths != expectedPaths || got.Access != "STORED_HOST_BLOCK_MATCH" || requests.Load() != 5 {
				t.Fatalf("actual HTTP collection %v/%v requests=%d", got, e, requests.Load())
			}
		})
	}
}

// nativeSecurityState is captured from the checksum-verified 0.16.27 synthetic store.
func nativeSecurityState(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/native-security-state.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func securityStateFor(t *testing.T, allow, block []any) []byte {
	t.Helper()
	query := func(entries []any) map[string]any {
		ids := []string{}
		for _, entry := range entries {
			ids = append(ids, entry.(map[string]any)["id"].(string))
		}
		return map[string]any{"accountId": "b", "queryState": "n", "canCalculateChanges": true, "position": 0, "ids": ids, "total": len(ids)}
	}
	b, e := json.Marshal(map[string]any{"sessionState": "synthetic", "methodResponses": []any{
		[]any{"x:AllowedIp/query", query(allow), "allowIds"},
		[]any{"x:AllowedIp/get", map[string]any{"accountId": "b", "list": allow, "notFound": []any{}}, "allow"},
		[]any{"x:BlockedIp/query", query(block), "blockIds"},
		[]any{"x:BlockedIp/get", map[string]any{"accountId": "b", "list": block, "notFound": []any{}}, "block"},
	}})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func accessEntry(address string, expiry any) map[string]any {
	return map[string]any{"id": "synthetic", "address": address, "expiresAt": expiry}
}

// TestNativeAddressBlockAndExpirySemantics matches the supported server's IP/CIDR precedence.
func TestNativeAddressBlockAndExpirySemantics(t *testing.T) {
	host := netip.MustParseAddr("192.0.2.10")
	if got, e := effectiveHostBlock(nativeSecurityState(t), host, accessNow); e != nil || got != "STORED_HOST_BLOCK_MATCH" {
		t.Fatalf("native blocked fixture: %s/%v", got, e)
	}
	for _, tc := range []struct {
		name         string
		allow, block []any
		want         string
	}{
		{"empty", []any{}, []any{}, "NO_ACTIVE_STORED_BLOCKS"},
		{"exact", []any{}, []any{accessEntry("192.0.2.10", nil)}, "STORED_HOST_BLOCK_MATCH"},
		{"network", []any{}, []any{accessEntry("192.0.2.0/24", nil)}, "STORED_HOST_BLOCK_MATCH"},
		{"IPv6", []any{}, []any{accessEntry("2001:db8::/32", nil)}, "OTHER_ACTIVE_STORED_BLOCKS"},
		{"other", []any{}, []any{accessEntry("198.51.100.2", nil)}, "OTHER_ACTIVE_STORED_BLOCKS"},
		{"expired", []any{}, []any{accessEntry("192.0.2.10", "2026-10-26T23:59:59Z")}, "NO_ACTIVE_STORED_BLOCKS"},
		{"boundary", []any{}, []any{accessEntry("192.0.2.10", "2026-10-27T00:00:00Z")}, "NO_ACTIVE_STORED_BLOCKS"},
		{"allow overrides", []any{accessEntry("192.0.2.0/24", nil)}, []any{accessEntry("192.0.2.10", nil)}, "STORED_HOST_ALLOW_OVERRIDE"},
		{"expired allow", []any{accessEntry("192.0.2.10", "2026-10-26T00:00:00Z")}, []any{accessEntry("192.0.2.10", nil)}, "STORED_HOST_BLOCK_MATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, e := effectiveHostBlock(securityStateFor(t, tc.allow, tc.block), host, accessNow)
			if e != nil || got != tc.want {
				t.Fatalf("got %s/%v want %s", got, e, tc.want)
			}
		})
	}
}

// TestSecurityStateRefusesPartialOrAmbiguousResponses retains unknown on all API/schema failures.
func TestSecurityStateRefusesPartialOrAmbiguousResponses(t *testing.T) {
	valid := string(securityStateFor(t, []any{}, []any{}))
	cases := []string{"", "null", "{}", valid + "{}", strings.Replace(valid, `"methodResponses":`, `"methodResponses":[],"methodResponses":`, 1),
		strings.Replace(valid, `"sessionState":"synthetic"`, `"sessionState":null`, 1),
		strings.Replace(valid, `"notFound":[]`, `"notFound":["missing"]`, 1),
		strings.Replace(valid, `"list":[]`, `"list":null`, 1),
		strings.Replace(valid, `x:BlockedIp/get`, `error`, 1),
		strings.Replace(valid, `"block"`, `"allow"`, 1),
		strings.Replace(valid, `"accountId":"b"`, `"accountId":""`, 1),
		strings.Repeat("x", maxAccessOutput+1)}
	for _, entry := range []map[string]any{
		{"id": "synthetic", "address": "nonsense", "expiresAt": nil},
		{"id": "synthetic", "address": "192.0.2.10", "expiresAt": "invalid"},
		{"id": "synthetic", "address": "192.0.2.10"},
		{"id": "synthetic", "address": "192.0.2.10", "expiresAt": nil, "unrequested": "PRIVATE_SENTINEL"},
	} {
		cases = append(cases, string(securityStateFor(t, []any{}, []any{entry})))
	}
	for i, b := range cases {
		if _, e := effectiveHostBlock([]byte(b), netip.MustParseAddr("192.0.2.10"), accessNow); e == nil {
			t.Fatalf("unsafe response %d accepted", i)
		}
	}
	many := make([]any, 257)
	for i := range many {
		many[i] = accessEntry("192.0.2.10", nil)
	}
	if _, e := effectiveHostBlock(securityStateFor(t, []any{}, many), netip.MustParseAddr("192.0.2.10"), accessNow); e == nil {
		t.Fatal("oversized result accepted")
	}
}

// accessPod supplies the real projection shape with documentation-only IPs.
func accessPod(t *testing.T) []byte {
	return changePod(t, fixturePod(t), func(p map[string]any) {
		status := p["status"].(map[string]any)
		status["podIP"] = "192.0.2.20"
		status["hostIP"] = "192.0.2.10"
		spec := p["spec"].(map[string]any)
		spec["volumes"] = append(spec["volumes"].([]any), map[string]any{
			"name": "private-inputs", "projected": map[string]any{"sources": []any{
				map[string]any{"secret": map[string]any{"name": "private-mail-settings", "items": []any{map[string]any{"key": "settings.json", "path": "settings.json"}}}},
				map[string]any{"secret": map[string]any{"name": "private-mail-admin", "items": []any{map[string]any{"key": "password", "path": "administrator-password"}}}},
			}}})
	})
}
func accessSecret(t *testing.T, name, key string, data []byte) []byte {
	t.Helper()
	values := map[string][]byte{key: data}
	if name == "private-mail-admin" {
		values["credential"] = append([]byte("admin:"), data...)
	}
	b, e := json.Marshal(map[string]any{"metadata": map[string]any{"name": name, "namespace": "private-mail", "uid": "00000000-0000-0000-0000-000000000004", "resourceVersion": "123"}, "data": values})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func accessMeasurement(t *testing.T, security []byte) []byte {
	t.Helper()
	b, e := json.Marshal(map[string]any{"live_loopback": 200, "ready_loopback": 200, "live_pod": 200, "ready_pod": 200, "security": json.RawMessage(security)})
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// TestAccessCollectionBindsInputsAndRuntimeAndNeverPlacesCredentialsInArgv exercises the complete collector.
func TestAccessCollectionBindsInputsAndRuntimeAndNeverPlacesCredentialsInArgv(t *testing.T) {
	for _, scenario := range []string{"healthy", "blocked", "cilium-blocked", "cilium-cidr-blocked", "cilium-allow-override", "cilium-expired", "pod-replaced", "restarted", "addresses-changed", "map-changed", "secret-changed", "read-failed", "exec-failed", "partial-response", "unapproved-projection", "runtime-env", "kubelet-minor-drift", "node-replaced", "cilium-missing", "cilium-foreign-node", "cilium-replaced", "cilium-address-changed", "cilium-version-changed", "cilium-ambiguous", "cilium-malformed", "cilium-host-mismatch", "cilium-pod-address", "cilium-host-address", "cilium-non-unicast"} {
		t.Run(scenario, func(t *testing.T) {
			pod := accessPod(t)
			after := append([]byte(nil), pod...)
			cm := fixtureMap(t)
			afterCM := append([]byte(nil), cm...)
			settings := []byte(`{"domain":"example.test","administrator":"administrator"}`)
			password := []byte(strings.Repeat("S", 64))
			settingSecret := accessSecret(t, "private-mail-settings", "settings.json", settings)
			adminSecret := accessSecret(t, "private-mail-admin", "password", password)
			afterAdmin := append([]byte(nil), adminSecret...)
			measured := accessMeasurement(t, securityStateFor(t, []any{}, []any{}))
			switch scenario {
			case "blocked":
				measured = accessMeasurement(t, nativeSecurityState(t))
			case "cilium-blocked":
				measured = accessMeasurement(t, securityStateFor(t, []any{}, []any{accessEntry("192.0.2.30", nil)}))
			case "cilium-cidr-blocked":
				measured = accessMeasurement(t, securityStateFor(t, []any{}, []any{accessEntry("192.0.2.28/30", nil)}))
			case "cilium-allow-override":
				measured = accessMeasurement(t, securityStateFor(t, []any{accessEntry("192.0.2.30", nil)}, []any{accessEntry("192.0.2.28/30", nil)}))
			case "cilium-expired":
				measured = accessMeasurement(t, securityStateFor(t, []any{}, []any{accessEntry("192.0.2.30", "2026-10-26T00:00:00Z")}))
			case "pod-replaced":
				after = []byte(strings.Replace(string(after), "000000000001", "000000000009", 1))
			case "restarted":
				after = []byte(strings.Replace(string(after), `"restartCount":0`, `"restartCount":1`, 1))
			case "addresses-changed":
				after = []byte(strings.Replace(string(after), "192.0.2.20", "192.0.2.21", 1))
			case "map-changed":
				afterCM = []byte(strings.Replace(string(cm), `"7"`, `"8"`, 1))
			case "secret-changed":
				afterAdmin = []byte(strings.Replace(string(adminSecret), `"123"`, `"124"`, 1))
			case "partial-response":
				measured = []byte(`{"live_loopback":200}`)
			case "unapproved-projection":
				pod = []byte(strings.Replace(string(pod), "private-mail-admin", "different-secret", 1))
			case "runtime-env":
				pod = changePod(t, pod, func(p map[string]any) {
					p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"] = []any{map[string]any{"name": "BASH_ENV", "value": "/tmp/unreviewed"}}
				})
			}
			calls, execs, ciliumReads := 0, 0, 0
			fake := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if len(args) < 7 || !reflect.DeepEqual(args[:3], []string{"--context", "admin@prod", "--request-timeout=10s"}) || args[3] != "get" {
					t.Fatalf("unexpected request %q", args)
				}
				if scenario == "read-failed" {
					return nil, errors.New("PRIVATE_ERROR")
				}
				switch args[4] {
				case "ciliumnode":
					ciliumReads++
					if args[5] != "synthetic-node" {
						t.Fatal("unbound CiliumNode read")
					}
					if scenario == "cilium-missing" {
						return nil, errors.New("not found")
					}
					uid, owner, address, version, host := "00000000-0000-0000-0000-000000000007", "00000000-0000-0000-0000-000000000005", "192.0.2.30", "77", "192.0.2.10"
					if scenario == "cilium-foreign-node" {
						owner = "00000000-0000-0000-0000-000000000009"
					}
					if scenario == "cilium-replaced" && execs > 0 {
						uid = "00000000-0000-0000-0000-000000000008"
					}
					if scenario == "cilium-address-changed" && execs > 0 {
						address = "192.0.2.31"
					}
					if scenario == "cilium-version-changed" && execs > 0 {
						version = "78"
					}
					if scenario == "cilium-host-mismatch" {
						host = "192.0.2.11"
					}
					if scenario == "cilium-pod-address" {
						address = "192.0.2.20"
					}
					if scenario == "cilium-host-address" {
						address = "192.0.2.10"
					}
					if scenario == "cilium-non-unicast" {
						address = "127.0.0.1"
					}
					addresses := []any{map[string]any{"type": "InternalIP", "ip": host}, map[string]any{"type": "CiliumInternalIP", "ip": address}}
					if scenario == "cilium-ambiguous" {
						addresses = append(addresses, map[string]any{"type": "CiliumInternalIP", "ip": "192.0.2.31"})
					}
					if scenario == "cilium-malformed" {
						return []byte(`{"metadata":null}`), nil
					}
					b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "synthetic-node", "uid": uid, "resourceVersion": version, "ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Node", "name": "synthetic-node", "uid": owner}}}, "spec": map[string]any{"addresses": addresses}})
					return b, nil
				case "node":
					if args[5] != "synthetic-node" {
						t.Fatal("unbound node read")
					}
					uid, version := "00000000-0000-0000-0000-000000000005", "v1.36.4"
					if scenario == "kubelet-minor-drift" {
						version = "v1.37.0"
					}
					if scenario == "node-replaced" && execs > 0 {
						uid = "00000000-0000-0000-0000-000000000006"
					}
					b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": "synthetic-node", "uid": uid}, "status": map[string]any{"nodeInfo": map[string]any{"kubeletVersion": version}}})
					return b, nil
				case "pod":
					if execs > 0 {
						return after, nil
					}
					return pod, nil
				case "configmap":
					if execs > 0 {
						return afterCM, nil
					}
					return cm, nil
				case "secret":
					switch args[5] {
					case "private-mail-settings":
						return settingSecret, nil
					case "private-mail-admin":
						if execs > 0 {
							return afterAdmin, nil
						}
						return adminSecret, nil
					}
				}
				t.Fatalf("unexpected resource %q", args)
				return nil, nil
			}
			send := func(_ context.Context, input []byte, args ...string) ([]byte, error) {
				execs++
				want := base64.StdEncoding.EncodeToString([]byte("administrator@example.test:"+string(password))) + "\n192.0.2.20\n"
				if string(input) != want {
					t.Fatal("credential/target input framing changed")
				}
				if strings.Contains(strings.Join(args, " "), string(password)) || strings.Contains(strings.Join(args, " "), strings.SplitN(want, "\n", 2)[0]) {
					t.Fatal("credential reached argv")
				}
				if len(args) != 18 || args[3] != "exec" || args[4] != "-i" || args[len(args)-1] != accessProbe {
					t.Fatal("unreviewed exec command shape")
				}
				if scenario == "exec-failed" {
					return []byte("PRIVATE_RESPONSE"), errors.New("PRIVATE_ERROR")
				}
				return measured, nil
			}
			got, e := collectAccess(context.Background(), fake, send, func(context.Context) error { t.Fatal("unexpected wait"); return nil }, func() time.Time { return accessNow })
			if scenario == "healthy" || scenario == "blocked" || scenario == "cilium-blocked" || scenario == "cilium-cidr-blocked" || scenario == "cilium-allow-override" || scenario == "cilium-expired" {
				if e != nil || got.Paths != "ALL_LOCAL_TARGETS_HTTP_200" || !got.Ready {
					t.Fatalf("valid collection %v/%v", got, e)
				}
				if (got.Access == "STORED_HOST_BLOCK_MATCH") != (scenario == "blocked") {
					t.Fatalf("block result %s", got.Access)
				}
				if ciliumReads != 2 {
					t.Fatalf("Cilium source candidate was not bound before and after: %d reads", ciliumReads)
				}
				want := "NO_ACTIVE_STORED_BLOCKS"
				switch scenario {
				case "blocked":
					want = "OTHER_ACTIVE_STORED_BLOCKS"
				case "cilium-blocked", "cilium-cidr-blocked":
					want = "STORED_CILIUM_BLOCK_MATCH"
				case "cilium-allow-override":
					want = "STORED_CILIUM_ALLOW_OVERRIDE"
				}
				if got.CiliumAccess != want {
					t.Fatalf("Cilium classification %q, want %q", got.CiliumAccess, want)
				}
			} else if e == nil {
				t.Fatalf("unsafe %s accepted: %v", scenario, got)
			}
			if execs > 1 || calls > 12 {
				t.Fatalf("unbounded reads/execs %d/%d", calls, execs)
			}
		})
	}
}

// TestAccessDispatchAndErrorsStayPrivate rejects invalid requests before native access.
func TestAccessDispatchAndErrorsStayPrivate(t *testing.T) {
	for _, key := range []string{"GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_RUN_ATTEMPT", "MAIL_ACCESS_CONFIRM", "valid"} {
		t.Run(key, func(t *testing.T) {
			env := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "MAIL_ACCESS_CONFIRM": "read-email-probe-state"}
			if key != "valid" {
				env[key] = "unapproved"
			}
			reads, execs := 0, 0
			var out bytes.Buffer
			code := runAccessEntry(context.Background(), func(k string) string { return env[k] }, func(context.Context, ...string) ([]byte, error) {
				reads++
				return []byte("PRIVATE_BODY"), errors.New("PRIVATE_ERROR")
			}, func(context.Context, []byte, ...string) ([]byte, error) { execs++; return nil, nil }, nil, &out)
			wantReads := 0
			if key == "valid" {
				wantReads = 1
			}
			if code != 3 || out.String() != "MAIL_PROBE_STATE=INCONCLUSIVE\n" || reads != wantReads || execs != 0 {
				t.Fatalf("unsafe dispatch %d %q %d/%d", code, out.String(), reads, execs)
			}
		})
	}
}
