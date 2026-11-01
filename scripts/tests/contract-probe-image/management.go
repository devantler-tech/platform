// Package contractprobeimage checks the published probe's isolated management contract.
package contractprobeimage

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const metricsBodyLimit = 65536

var errMetrics = errors.New("unexpected probe diagnostic metrics")

type observation struct {
	reason   string
	observed float64
}

// verifyManagement observes one failed readiness request and two passive scrapes.
// The native fixture denies all networking, so a completed DNS failure is expected.
func verifyManagement(ctx context.Context, base string) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   8 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if _, err := readEndpoint(ctx, client, base+"/healthz", http.StatusOK); err != nil {
		return err
	}
	if _, err := readEndpoint(ctx, client, base+"/readyz", http.StatusServiceUnavailable); err != nil {
		return err
	}
	var first observation
	for scrape := range 2 {
		body, err := readEndpoint(ctx, client, base+"/metrics", http.StatusOK)
		if err != nil {
			return err
		}
		current, err := parseObservation(body)
		if err != nil {
			return err
		}
		if scrape == 0 {
			first = current
		} else if current != first {
			return errors.New("scraping changed the completed observation")
		}
	}
	return nil
}

// readEndpoint bounds every management response without retaining response errors.
func readEndpoint(ctx context.Context, client *http.Client, url string, status int) (body []byte, resultErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("invalid management request")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("management endpoint read failed")
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && resultErr == nil {
			body = nil
			resultErr = errors.New("management response close failed")
		}
	}()
	if response.StatusCode != status {
		return nil, errors.New("unexpected management endpoint status")
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, metricsBodyLimit+1))
	if err != nil || len(body) > metricsBodyLimit {
		return nil, errors.New("incomplete or oversized management response")
	}
	return body, nil
}

// parseObservation requires the new bounded diagnostic series and coherent legacy gauges.
func parseObservation(body []byte) (observation, error) {
	var result observation
	seen := make(map[string]bool)
	for line := range strings.SplitSeq(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		name, _, _ := strings.Cut(fields[0], "{")
		switch name {
		case "contract_probe_requests_total", "contract_probe_failures_total",
			"contract_probe_ready", "contract_probe_last_observation_timestamp_seconds":
		default:
			continue
		}
		if seen[name] || len(fields) != 2 {
			return observation{}, errMetrics
		}
		seen[name] = true
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return observation{}, errMetrics
		}
		labels, err := parseLabels(strings.TrimPrefix(fields[0], name))
		if err != nil {
			return observation{}, err
		}
		switch name {
		case "contract_probe_requests_total":
			if value != 1 || len(labels) != 1 || labels["result"] != "ContractUnavailable" {
				return observation{}, errMetrics
			}
		case "contract_probe_failures_total":
			if value != 1 || len(labels) != 3 || labels["http_status"] != "none" ||
				labels["stage"] != "request" || (labels["reason"] != "dns" && labels["reason"] != "timeout") {
				return observation{}, errMetrics
			}
			result.reason = labels["reason"]
		case "contract_probe_ready":
			if value != 0 || len(labels) != 0 {
				return observation{}, errMetrics
			}
		case "contract_probe_last_observation_timestamp_seconds":
			if value <= 0 || len(labels) != 0 {
				return observation{}, errMetrics
			}
			result.observed = value
		}
	}
	if len(seen) != 4 {
		return observation{}, errMetrics
	}
	return result, nil
}

// parseLabels accepts only a simple fixed-vocabulary label set, without duplicates.
func parseLabels(text string) (map[string]string, error) {
	labels := make(map[string]string)
	if text == "" {
		return labels, nil
	}
	if !strings.HasPrefix(text, "{") || !strings.HasSuffix(text, "}") {
		return nil, errMetrics
	}
	for field := range strings.SplitSeq(text[1:len(text)-1], ",") {
		key, quoted, found := strings.Cut(field, "=")
		value, err := strconv.Unquote(quoted)
		if !found || key == "" || err != nil {
			return nil, errMetrics
		}
		if _, duplicate := labels[key]; duplicate {
			return nil, errMetrics
		}
		labels[key] = value
	}
	return labels, nil
}
