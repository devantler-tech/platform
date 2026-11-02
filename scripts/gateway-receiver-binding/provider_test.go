package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Fixtures use documentation addresses and a disposable bearer value only.
func providerFixture() (map[string]any, binding) {
	s, _ := json.Marshal(serviceFixture())
	b, _ := derive(s, gatewayFixture())
	lb := map[string]any{
		"id": 7, "name": "platform-gateway",
		"labels":      map[string]any{"hcloud-ccm/service-uid": b.ServiceUID},
		"public_net":  map[string]any{"enabled": true, "ipv4": map[string]any{"ip": b.PublicIPv4, "blocked": false}},
		"private_net": []any{map[string]any{"network": 9, "ip": "10.11.12.13"}},
		"services": []any{
			map[string]any{"listen_port": 80, "destination_port": 31080, "protocol": "tcp", "proxyprotocol": false},
			map[string]any{"listen_port": 443, "destination_port": 31443, "protocol": "tcp", "proxyprotocol": false},
		},
		"targets": []any{map[string]any{"type": "server", "server": map[string]any{"id": 11}, "use_private_ip": true}},
	}
	return lb, b
}

type providerTransport func(*http.Request) (*http.Response, error)

func (f providerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func providerResponse(v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}
}

func providerHTTP(t *testing.T, lb map[string]any) (*http.Client, *int) {
	t.Helper()
	calls := new(int)
	client := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "api.hetzner.cloud" || r.Header.Get("Authorization") != "Bearer disposable-test-token" || r.Body != nil {
			t.Fatal("provider observation changed method, origin or credential boundary")
		}
		switch r.URL.Path {
		case "/v1/load_balancers":
			if r.URL.Query().Get("label_selector") != "hcloud-ccm/service-uid=22222222-2222-4222-8222-222222222222" || r.URL.Query().Get("page") != "1" || r.URL.Query().Get("per_page") != "50" {
				t.Fatal("provider lookup was not scoped to the bound Service UID")
			}
			return providerResponse(map[string]any{"load_balancers": []any{lb}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 50, "last_page": 1, "next_page": nil, "total_entries": 1}}}), nil
		case "/v1/load_balancers/7":
			return providerResponse(map[string]any{"load_balancer": lb}), nil
		default:
			t.Fatal("provider observation requested an undeclared resource")
			return nil, nil
		}
	})}
	return client, calls
}

func TestProviderObservationRequiresEffectiveModeAndBoundIdentity(t *testing.T) {
	lb, b := providerFixture()
	client, calls := providerHTTP(t, lb)
	got, err := observeProvider(context.Background(), client, "disposable-test-token", b, false)
	if err != nil || got != (providerIdentity{ID: 7, NetworkID: 9}) || *calls != 2 {
		t.Fatalf("complete read-only provider observation refused: %v", err)
	}
	for _, item := range lb["services"].([]any) {
		item.(map[string]any)["proxyprotocol"] = true
	}
	got, err = observeProvider(context.Background(), client, "disposable-test-token", b, true)
	if err != nil || got.ID != 7 {
		t.Fatal("known active state cannot be inspected for recovery")
	}
}

func TestProviderObservationRefusesMissingOrConflictingFacts(t *testing.T) {
	for _, bad := range []string{"uid", "name", "id", "public", "public-disabled", "public-blocked", "private", "network", "extra-network", "missing-mode", "null-mode", "enabled", "partial-mode", "destination", "protocol", "extra-service", "duplicate-service", "target-public", "target-ip", "empty-targets"} {
		t.Run(bad, func(t *testing.T) {
			lb, b := providerFixture()
			svc := lb["services"].([]any)[1].(map[string]any)
			switch bad {
			case "uid":
				lb["labels"].(map[string]any)["hcloud-ccm/service-uid"] = "other"
			case "name":
				lb["name"] = "other"
			case "id":
				lb["id"] = 0
			case "public":
				lb["public_net"].(map[string]any)["ipv4"].(map[string]any)["ip"] = "192.0.2.2"
			case "public-disabled":
				lb["public_net"].(map[string]any)["enabled"] = false
			case "public-blocked":
				lb["public_net"].(map[string]any)["ipv4"].(map[string]any)["blocked"] = true
			case "private":
				lb["private_net"].([]any)[0].(map[string]any)["ip"] = "10.11.12.14"
			case "network":
				lb["private_net"].([]any)[0].(map[string]any)["network"] = 0
			case "extra-network":
				lb["private_net"] = append(lb["private_net"].([]any), map[string]any{"network": 10, "ip": "10.11.12.14"})
			case "missing-mode":
				delete(svc, "proxyprotocol")
			case "null-mode":
				svc["proxyprotocol"] = nil
			case "enabled":
				for _, item := range lb["services"].([]any) {
					item.(map[string]any)["proxyprotocol"] = true
				}
			case "partial-mode":
				svc["proxyprotocol"] = true
			case "destination":
				svc["destination_port"] = 443
			case "protocol":
				svc["protocol"] = "https"
			case "extra-service":
				lb["services"] = append(lb["services"].([]any), map[string]any{"listen_port": 25})
			case "duplicate-service":
				svc["listen_port"] = 80
			case "target-public":
				lb["targets"].([]any)[0].(map[string]any)["use_private_ip"] = false
			case "target-ip":
				lb["targets"].([]any)[0].(map[string]any)["type"] = "ip"
			case "empty-targets":
				lb["targets"] = []any{}
			}
			client, _ := providerHTTP(t, lb)
			if _, err := observeProvider(context.Background(), client, "disposable-test-token", b, false); err == nil {
				t.Fatal("unverified provider state accepted")
			}
		})
	}
}

func TestProviderObservationRefusesIncompleteInventoryAndChangedDetail(t *testing.T) {
	for _, bad := range []string{"missing-meta", "no-match", "multiple", "more-pages", "missing-next", "unknown-count", "wrong-page", "duplicate-json", "truncated-json", "detail-id", "detail-network", "detail-mode", "detail-uid"} {
		t.Run(bad, func(t *testing.T) {
			lb, b := providerFixture()
			client, _ := providerHTTP(t, lb)
			base := client.Transport
			client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
				resp, err := base.RoundTrip(r)
				if err != nil {
					return resp, err
				}
				defer func() { _ = resp.Body.Close() }()
				var doc map[string]any
				if json.NewDecoder(resp.Body).Decode(&doc) != nil {
					t.Fatal("fixture response")
				}
				if r.URL.Path == "/v1/load_balancers" {
					p := doc["meta"].(map[string]any)["pagination"].(map[string]any)
					switch bad {
					case "missing-meta":
						delete(doc, "meta")
					case "no-match":
						doc["load_balancers"] = []any{}
					case "multiple":
						doc["load_balancers"] = []any{lb, lb}
						p["total_entries"] = 2
					case "more-pages":
						p["next_page"] = 2
						p["last_page"] = 2
					case "missing-next":
						delete(p, "next_page")
					case "unknown-count":
						delete(p, "total_entries")
					case "wrong-page":
						p["page"] = 2
					case "duplicate-json":
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"load_balancers":[],"load_balancers":[]}`))}, nil
					case "truncated-json":
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"load_balancers": [`))}, nil
					}
				} else {
					detail := doc["load_balancer"].(map[string]any)
					switch bad {
					case "detail-id":
						detail["id"] = 8
					case "detail-network":
						detail["private_net"].([]any)[0].(map[string]any)["network"] = 10
					case "detail-mode":
						detail["services"].([]any)[1].(map[string]any)["proxyprotocol"] = true
					case "detail-uid":
						detail["labels"].(map[string]any)["hcloud-ccm/service-uid"] = "other"
					}
				}
				return providerResponse(doc), nil
			})
			if _, err := observeProvider(context.Background(), client, "disposable-test-token", b, false); err == nil {
				t.Fatal("incomplete or changed provider evidence accepted")
			}
		})
	}
}

func TestProviderObservationRequiresCanonicalJSONFields(t *testing.T) {
	for _, where := range []string{"list", "detail"} {
		for _, duplicate := range []bool{false, true} {
			t.Run(where+"/"+map[bool]string{false: "alias", true: "collision"}[duplicate], func(t *testing.T) {
				lb, b := providerFixture()
				client, _ := providerHTTP(t, lb)
				base := client.Transport
				client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := base.RoundTrip(r)
					if err != nil {
						return resp, err
					}
					if (where == "list") != (r.URL.Path == "/v1/load_balancers") {
						return resp, nil
					}
					data, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					field := `"ProxyProtocol":false`
					if duplicate {
						field = `"proxyprotocol":true,"ProxyProtocol":false`
					}
					data = []byte(strings.Replace(string(data), `"proxyprotocol":false`, field, 1))
					resp.Body = io.NopCloser(strings.NewReader(string(data)))
					return resp, nil
				})
				if _, err := observeProvider(context.Background(), client, "disposable-test-token", b, false); err == nil {
					t.Fatal("case-insensitive decoder accepted a missing or overridden authoritative field")
				}
			})
		}
	}
}

func TestProviderObservationAcceptsEmptyServerChildrenAndArbitraryLabelCase(t *testing.T) {
	lb, b := providerFixture()
	lb["targets"].([]any)[0].(map[string]any)["targets"] = []any{}
	lb["labels"].(map[string]any)["BusinessOwner"] = "synthetic"
	client, _ := providerHTTP(t, lb)
	if _, err := observeProvider(context.Background(), client, "disposable-test-token", b, false); err != nil {
		t.Fatal("valid provider server leaf or arbitrary label refused")
	}
}

func TestProviderObservationRefusesUnicodeFoldAliases(t *testing.T) {
	for _, where := range []string{"list", "detail"} {
		for _, duplicate := range []bool{false, true} {
			t.Run(where+"/"+map[bool]string{false: "alias", true: "collision"}[duplicate], func(t *testing.T) {
				lb, b := providerFixture()
				client, _ := providerHTTP(t, lb)
				base := client.Transport
				client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := base.RoundTrip(r)
					if err != nil {
						return resp, err
					}
					if (where == "list") != (r.URL.Path == "/v1/load_balancers") {
						return resp, nil
					}
					data, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					field := `"\u017fervices":`
					if duplicate {
						field = `"services":[{"listen_port":80,"destination_port":31080,"protocol":"tcp","proxyprotocol":true},{"listen_port":443,"destination_port":31443,"protocol":"tcp","proxyprotocol":true}],` + field
					}
					data = []byte(strings.Replace(string(data), `"services":`, field, 1))
					resp.Body = io.NopCloser(strings.NewReader(string(data)))
					return resp, nil
				})
				if _, err := observeProvider(context.Background(), client, "disposable-test-token", b, false); err == nil {
					t.Fatal("Unicode-folded alias substituted an authoritative schema field")
				}
			})
		}
	}
}

func TestProviderObservationDoesNotFollowRedirectOrDiscloseProviderBody(t *testing.T) {
	forwarded := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded++; w.WriteHeader(200) }))
	defer other.Close()
	_, b := providerFixture()
	for _, status := range []int{301, 302, 307, 401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer disposable-test-token" {
					t.Error("request boundary")
				}
				w.Header().Set("Location", other.URL)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "private-provider-detail disposable-test-token")
			}))
			defer server.Close()
			local, _ := url.Parse(server.URL)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.hetzner.cloud" {
					t.Fatal("redirect followed")
				}
				copy := r.Clone(r.Context())
				copy.URL.Scheme, copy.URL.Host = local.Scheme, local.Host
				return transport.RoundTrip(copy)
			})}
			_, err := observeProvider(context.Background(), client, "disposable-test-token", b, false)
			if err == nil || strings.Contains(err.Error(), "private-provider-detail") || strings.Contains(err.Error(), "disposable-test-token") || forwarded != 0 {
				t.Fatal("HTTP refusal leaked data or followed a redirect")
			}
		})
	}
}
