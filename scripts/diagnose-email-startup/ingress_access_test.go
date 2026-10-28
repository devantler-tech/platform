package main

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type ingressRoundTrip func(*http.Request) (*http.Response, error)

func (f ingressRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIngressPublicProbeHasFixedTargetAndRefusesRedirectsOrLargeBodies(t *testing.T) {
	for _, mode := range []string{"200", "503", "redirect", "oversized", "other-status"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := ingressHTTPClient()
			client.Transport = ingressRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "https://mail.platform.devantler.tech/health/ready" || r.Method != "GET" || len(r.Header) != 0 {
					t.Fatal("probe target or credential boundary changed")
				}
				code, body := 200, "ok"
				header := http.Header{}
				switch mode {
				case "503":
					code = 503
				case "redirect":
					code = 302
					header.Set("Location", "https://unapproved.example.test/")
				case "oversized":
					body = strings.Repeat("x", 16385)
				case "other-status":
					code = 401
				}
				return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			status, err := probeIngressPublic(context.Background(), client)
			allowed := mode == "200" || mode == "503"
			if (err == nil) != allowed || calls != 1 || (err != nil && status != 0) {
				t.Fatal("probe accepted incomplete evidence, redirected or lost a valid result")
			}
		})
	}
}

func TestIngressAssessmentUsesActualPeersAndRefusesChangingRules(t *testing.T) {
	before := accessMeasurement(t, securityStateFor(t, []any{}, []any{accessEntry("192.0.2.10", nil)}))
	for _, mode := range []string{"blocked", "unblocked", "mixed", "rules-changed", "invalid-after", "expired-during-window"} {
		t.Run(mode, func(t *testing.T) {
			after := append([]byte(nil), before...)
			observation := ingressObservation{Sources: []netip.Addr{netip.MustParseAddr("192.0.2.10")}, Public503: 2}
			switch mode {
			case "unblocked":
				observation.Sources[0] = netip.MustParseAddr("198.51.100.1")
			case "mixed":
				observation.Sources = append(observation.Sources, netip.MustParseAddr("198.51.100.1"))
			case "rules-changed":
				after = accessMeasurement(t, securityStateFor(t, []any{}, []any{}))
			case "invalid-after":
				after = []byte(`{"security":null}`)
			case "expired-during-window":
				before = accessMeasurement(t, securityStateFor(t, []any{}, []any{accessEntry("192.0.2.10", accessNow.Add(time.Second).Format(time.RFC3339))}))
				after = append([]byte(nil), before...)
			}
			result, err := assessIngressAccess(before, after, observation, accessNow, accessNow.Add(10*time.Second))
			allowed := mode == "blocked" || mode == "unblocked" || mode == "mixed"
			if (err == nil) != allowed {
				t.Fatal("assessment accepted changing evidence or refused stable observations")
			}
			if mode == "blocked" && (result.Blocked != 1 || result.Unblocked != 0) || mode == "unblocked" && (result.Blocked != 0 || result.Unblocked != 1) || mode == "mixed" && (result.Blocked != 1 || result.Unblocked != 1) {
				t.Fatal("assessment substituted a candidate for the observed peer")
			}
		})
	}
}
