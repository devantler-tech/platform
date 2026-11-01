package contractprobeimage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const completedDNSFailure = `contract_probe_requests_total{result="ContractUnavailable"} 1
contract_probe_failures_total{http_status="none",reason="dns",stage="request"} 1
contract_probe_ready 0
contract_probe_last_observation_timestamp_seconds 1793510000
`

func TestManagementContract(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		metrics      string
		healthStatus int
		readyStatus  int
		mutateScrape bool
		wantError    bool
	}{
		{name: "completed DNS failure", metrics: completedDNSFailure},
		{name: "completed DNS timeout", metrics: strings.ReplaceAll(completedDNSFailure, `reason="dns"`, `reason="timeout"`)},
		{name: "missing diagnostics", metrics: strings.ReplaceAll(completedDNSFailure, "contract_probe_failures_total", "obsolete_counter"), wantError: true},
		{name: "unexpected raw error label", metrics: strings.ReplaceAll(completedDNSFailure, `reason="dns"`, `reason="PRIVATE-ERROR-CANARY"`), wantError: true},
		{name: "wrong stage", metrics: strings.ReplaceAll(completedDNSFailure, `stage="request"`, `stage="body"`), wantError: true},
		{name: "unexpected HTTP status", metrics: strings.ReplaceAll(completedDNSFailure, `http_status="none"`, `http_status="503"`), wantError: true},
		{name: "extra label", metrics: strings.ReplaceAll(completedDNSFailure, `stage="request"`, `stage="request",url="PRIVATE-ERROR-CANARY"`), wantError: true},
		{name: "duplicate series", metrics: completedDNSFailure + "contract_probe_failures_total{http_status=\"none\",reason=\"dns\",stage=\"request\"} 1\n", wantError: true},
		{name: "missing result", metrics: strings.ReplaceAll(completedDNSFailure, "ContractUnavailable", "ContractConfigurationInvalid"), wantError: true},
		{name: "no completed observation", metrics: strings.ReplaceAll(completedDNSFailure, "1793510000", "0"), wantError: true},
		{name: "scraping initiates work", metrics: completedDNSFailure, mutateScrape: true, wantError: true},
		{name: "unhealthy process", metrics: completedDNSFailure, healthStatus: http.StatusServiceUnavailable, wantError: true},
		{name: "wrong readiness decision", metrics: completedDNSFailure, readyStatus: http.StatusOK, wantError: true},
		{name: "redirect", metrics: completedDNSFailure, healthStatus: http.StatusFound, wantError: true},
		{name: "oversized metrics", metrics: completedDNSFailure + strings.Repeat("x", 65537), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scrapes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz":
					status := tc.healthStatus
					if status == 0 {
						status = http.StatusOK
					}
					w.WriteHeader(status)
				case "/readyz":
					status := tc.readyStatus
					if status == 0 {
						status = http.StatusServiceUnavailable
					}
					w.WriteHeader(status)
				case "/metrics":
					scrapes++
					body := tc.metrics
					if tc.mutateScrape && scrapes == 2 {
						body = strings.ReplaceAll(body, "} 1\n", "} 2\n")
					}
					_, _ = w.Write([]byte(body))
				default:
					t.Errorf("unexpected request path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			err := verifyManagement(context.Background(), server.URL)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, wantError=%v", err, tc.wantError)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE-ERROR-CANARY") {
				t.Fatal("response data escaped into the failure receipt")
			}
		})
	}
}
