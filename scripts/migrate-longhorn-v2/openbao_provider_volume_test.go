package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type openBaoProviderTransport func(*http.Request) (*http.Response, error)

func (r openBaoProviderTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return r(request)
}

func TestOpenBaoReadsOnlyTheNativeProviderVolume(t *testing.T) {
	for _, tc := range []struct {
		name, handle, raw string
		status            int
		read, passes      bool
	}{
		{"explicit assignee", "10", `{"volume":{"id":10,"assignee":null}}`, 200, true, true},
		{"no assignee", "10", `{"volume":{"id":10,"server":null}}`, 200, true, false},
		{"other resource", "10", `{"volume":{"id":11,"assignee":null}}`, 200, true, false},
		{"duplicate id", "10", `{"volume":{"id":11,"id":10,"assignee":null}}`, 200, true, false},
		{"duplicate assignee", "10", `{"volume":{"id":10,"assignee":{"id":11},"assignee":null}}`, 200, true, false},
		{"provider denied", "10", `{"volume":{"id":10,"assignee":null}}`, 403, true, false},
		{"provider redirected", "10", `{"volume":{"id":10,"assignee":null}}`, 302, true, false},
		{"malformed handle", "10/attach", "", 0, false, false},
		{"outside numeric identity", "9007199254740992", "", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			transport := openBaoProviderTransport(func(req *http.Request) (*http.Response, error) {
				reached = true
				if req.Method != http.MethodGet || req.URL.String() != "https://api.hetzner.cloud/v1/volumes/10" ||
					req.Header.Get("Authorization") != "Bearer synthetic-provider-token" || req.Body != nil {
					t.Fatalf("unexpected provider request: %s %s", req.Method, req.URL)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.raw)),
					Header: http.Header{"Location": []string{"https://example.invalid/other"}}}, nil
			})
			observed, err := readOpenBaoProviderVolume(context.Background(), transport, "synthetic-provider-token", tc.handle)
			if reached != tc.read || (err == nil) != tc.passes || (observed != nil) != tc.passes {
				t.Fatalf("native volume observed=%t provider read=%t, error=%v", observed != nil, reached, err)
			}
		})
	}
}
