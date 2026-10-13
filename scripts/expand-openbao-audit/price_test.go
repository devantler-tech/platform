package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPriceGuardEnforcesConservativeGrossEuroBudget(t *testing.T) {
	for _, tc := range []struct {
		currency, price string
		allowed         bool
	}{
		{"EUR", "0.05236", true}, {"EUR", "0.0523600000", true}, {"EUR", "0.075", true}, {"EUR", "0.076", false},
		{"EUR", "0.0530740000000000", true}, {"EUR", "0.05000000000000000000000000000000", true},
		{"EUR", "0.07575757575757575", true}, {"EUR", "0.07575757575757576", false},
		{"EUR", "0.050000000000000000000000000000000", false}, {"EUR", "5e-2", false},
		{"USD", "0.044", false}, {"EUR", "0", false}, {"EUR", "-0.044", false}, {"EUR", "NaN", false}, {"EUR", "", false},
	} {
		b := []byte(`{"pricing":{"currency":"` + tc.currency + `","volume":{"price_per_gb_month":{"gross":"` + tc.price + `"}}}}`)
		if priceWithinBudget(b) != tc.allowed {
			t.Fatalf("wrong price decision: currency=%s price=%s", tc.currency, tc.price)
		}
	}
	large := `{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"0.05"}}},"other_resource_prices":"` + strings.Repeat("x", 100000) + `"}`
	if !priceWithinBudget([]byte(large)) {
		t.Fatal("complete expanded pricing response rejected")
	}
	for _, b := range []string{`{}`, `{"pricing":{"currency":"EUR"}}`, `{broken`, strings.Repeat("x", 65537), `{"pricing":{"currency":"USD","currency":"EUR","volume":{"price_per_gb_month":{"gross":"0.05"}}}}`, `{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"1","gross":"0.05"}}}}`} {
		if priceWithinBudget([]byte(b)) {
			t.Fatal("incomplete pricing accepted")
		}
	}
}

func TestPricingDiagnosticsExposeOnlyBoundedCodes(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"0.05"}}}}`, "WITHIN_BUDGET"},
		{`{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"1"}}}}`, "OVER_BUDGET"},
		{`{"pricing":{"currency":"USD","volume":{"price_per_gb_month":{"gross":"0.05"}}}}`, "UNSUPPORTED_CURRENCY"},
		{`{broken`, "MALFORMED_RESPONSE"},
		{`{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"0.0530740000000000"}}}}`, "WITHIN_BUDGET"},
		{`{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"0.07575757575757576"}}}}`, "OVER_BUDGET"},
		{`{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"PRIVATE"}}}}`, "UNSUPPORTED_DECIMAL"},
		{`{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"0"}}}}`, "NONPOSITIVE_PRICE"},
	} {
		if got := priceCode([]byte(tc.body)); got != tc.want {
			t.Fatalf("price code=%s want=%s", got, tc.want)
		}
	}
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPriceRequestIsFixedAndDoesNotFollowCredentialRedirects(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		calls := 0
		client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }, Transport: transport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() != "https://api.hetzner.cloud/v1/pricing" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer SYNTHETIC" {
				t.Fatal("pricing credential escaped fixed read")
			}
			status := 200
			headers := http.Header{}
			if redirect {
				status = 302
				headers.Set("Location", "https://unreviewed.invalid")
			}
			return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(`{"pricing":{"currency":"EUR","volume":{"price_per_gb_month":{"gross":"0.05"}}}}`)), Request: r}, nil
		})}
		if fetchPrice(context.Background(), client, "SYNTHETIC") == redirect || calls != 1 {
			t.Fatal("redirect or invalid pricing accepted")
		}
	}
}
