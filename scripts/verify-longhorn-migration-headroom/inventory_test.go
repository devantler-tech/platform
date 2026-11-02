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
