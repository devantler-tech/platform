package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

type failedClose struct{ io.Reader }

func (failedClose) Close() error { return errors.New("body close failed") }

func TestProviderCloseFailureDiscardsInventory(t *testing.T) {
	body := providerResponse(t, nil)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: failedClose{strings.NewReader(body)}}, nil
	})}
	if s, err := collectServers(context.Background(), client, "private-test-token"); err == nil || s != nil {
		t.Fatal("failed provider read completion accepted")
	}
}

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func providerResponse(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	s, _ := roster(t, false)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var values []any
	if err = json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	response := map[string]any{"servers": values, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 50, "last_page": 1, "next_page": nil, "total_entries": 8}}}
	if mutate != nil {
		mutate(response)
	}
	raw, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func providerClient(t *testing.T, body string, status int) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.String() != "https://api.hetzner.cloud/v1/servers?per_page=50&page=1" || r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Fatalf("unexpected provider request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
}

func TestCompleteBoundedProviderRead(t *testing.T) {
	body := providerResponse(t, nil)
	s, err := collectServers(context.Background(), providerClient(t, body, 200), "private-test-token")
	if err != nil || len(s) != 8 {
		t.Fatalf("complete provider response rejected: count=%d err=%v", len(s), err)
	}
}

func TestProviderEnvelopeHoldExplainsFailedGuardsWithoutPayload(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		guard  string
	}{
		{"missing inventory", func(v map[string]any) { v["servers"] = nil }, "servers_present=false"},
		{"capacity", func(v map[string]any) {
			values := v["servers"].([]any)
			v["servers"] = append(values, values[0], values[0])
			v["meta"].(map[string]any)["pagination"].(map[string]any)["total_entries"] = 10
		}, "servers=10 servers_present=true within_capacity=false"},
		{"wrong page", func(v map[string]any) { pagination(v)["page"] = 2 }, "page_valid=false"},
		{"missing page", func(v map[string]any) { delete(pagination(v), "page") }, "page_valid=false"},
		{"wrong page size", func(v map[string]any) { pagination(v)["per_page"] = 25 }, "per_page_valid=false"},
		{"missing page size", func(v map[string]any) { delete(pagination(v), "per_page") }, "per_page_valid=false"},
		{"additional pages", func(v map[string]any) { pagination(v)["last_page"] = 2 }, "last_page_valid=false"},
		{"unknown last page", func(v map[string]any) { pagination(v)["last_page"] = nil }, "last_page_valid=false"},
		{"continuation", func(v map[string]any) { pagination(v)["next_page"] = 2 }, "next_page_terminal=false"},
		{"missing continuation", func(v map[string]any) { delete(pagination(v), "next_page") }, "next_page_terminal=false"},
		{"unknown total", func(v map[string]any) { pagination(v)["total_entries"] = nil }, "total_present=false total_matches=false"},
		{"count mismatch", func(v map[string]any) { pagination(v)["total_entries"] = 9 }, "total_present=true total_matches=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := providerResponse(t, func(v map[string]any) {
				v["servers"].([]any)[0].(map[string]any)["name"] = "private-provider-name-canary"
				tc.mutate(v)
			})
			servers, err := collectServers(context.Background(), providerClient(t, body, 200), "private-test-token")
			if err == nil || servers != nil {
				t.Fatal("invalid provider envelope accepted")
			}
			if !strings.Contains(err.Error(), tc.guard) {
				t.Fatalf("hold did not identify failed guard %q: %v", tc.guard, err)
			}
			if len(err.Error()) > 350 || strings.Contains(err.Error(), "private-provider-name-canary") || strings.Contains(err.Error(), "private-test-token") {
				t.Fatal("provider diagnostic escaped its bounded public fields")
			}
		})
	}
}

func pagination(v map[string]any) map[string]any {
	return v["meta"].(map[string]any)["pagination"].(map[string]any)
}

func TestIncompleteProviderReadCannotGrantHeadroom(t *testing.T) {
	cases := map[string]func(map[string]any){
		"continuation":         func(v map[string]any) { v["meta"].(map[string]any)["pagination"].(map[string]any)["next_page"] = 2 },
		"count mismatch":       func(v map[string]any) { v["meta"].(map[string]any)["pagination"].(map[string]any)["total_entries"] = 9 },
		"missing continuation": func(v map[string]any) { delete(v["meta"].(map[string]any)["pagination"].(map[string]any), "next_page") },
		"wrong page":           func(v map[string]any) { v["meta"].(map[string]any)["pagination"].(map[string]any)["page"] = 2 },
		"additional pages":     func(v map[string]any) { v["meta"].(map[string]any)["pagination"].(map[string]any)["last_page"] = 2 },
		"missing inventory":    func(v map[string]any) { v["servers"] = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			body := providerResponse(t, change)
			s, err := collectServers(context.Background(), providerClient(t, body, 200), "private-test-token")
			if err == nil || s != nil {
				t.Fatal("incomplete provider inventory accepted")
			}
		})
	}
	for name, body := range map[string]string{"trailing response": providerResponse(t, nil) + `{}`, "oversized response": strings.Repeat(" ", 2*1024*1024+1), "malformed": `{"servers":`} {
		t.Run(name, func(t *testing.T) {
			s, err := collectServers(context.Background(), providerClient(t, body, 200), "private-test-token")
			if err == nil || s != nil {
				t.Fatal("invalid provider response accepted")
			}
		})
	}
	if s, err := collectServers(context.Background(), providerClient(t, providerResponse(t, nil), 403), "private-test-token"); err == nil || s != nil {
		t.Fatal("authorization failure accepted")
	}
}

func TestNativeNodeEnvelopeMustBeComplete(t *testing.T) {
	_, n := roster(t, false)
	raw, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "NodeList", "metadata": map[string]any{"resourceVersion": "123", "continue": ""}, "items": n})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := decodeNodes(raw)
	if err != nil || len(nodes) != 8 {
		t.Fatalf("native list rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"continued":             `{"apiVersion":"v1","kind":"NodeList","metadata":{"resourceVersion":"123","continue":"later"},"items":[]}`,
		"printer stripped kind": `{"metadata":{"resourceVersion":"123"},"items":[]}`,
		"missing revision":      `{"apiVersion":"v1","kind":"NodeList","metadata":{},"items":[]}`,
		"null inventory":        `{"apiVersion":"v1","kind":"NodeList","metadata":{"resourceVersion":"123"},"items":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			if n, err := decodeNodes([]byte(raw)); err == nil || n != nil {
				t.Fatal("incomplete node inventory accepted")
			}
		})
	}
}
