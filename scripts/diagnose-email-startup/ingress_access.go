package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"
)

type ingressAccessResult struct {
	Blocked, Allowed, Unblocked int
	Public200, Public503        int
}

func ingressHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errIngressEvidence }}
}

func probeIngressPublic(ctx context.Context, client *http.Client) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://mail.platform.devantler.tech/health/ready", nil)
	if err != nil {
		return 0, errIngressEvidence
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, errIngressEvidence
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16385))
	defer clear(body)
	if err != nil || len(body) > 16384 || (response.StatusCode != 200 && response.StatusCode != 503) {
		return 0, errIngressEvidence
	}
	return response.StatusCode, nil
}

// assessIngressAccess releases counts only after both complete native reads
// agree, and every observed peer has the same effective classification at both
// ends of the observation. Addresses and security records never reach output.
func assessIngressAccess(before, after []byte, observation ingressObservation, since, until time.Time) (ingressAccessResult, error) {
	refuse := func() (ingressAccessResult, error) { return ingressAccessResult{}, errIngressEvidence }
	if len(observation.Sources) == 0 || len(observation.Sources) > 32 || observation.Public200 < 0 || observation.Public503 < 0 || observation.Public200+observation.Public503 != 2 || until.Before(since) {
		return refuse()
	}
	security := func(raw []byte) (any, error) {
		var measurement accessObservation
		if len(raw) > maxAccessOutput || strictJSON(raw, &measurement) != nil || uniqueJSON(measurement.Security) != nil {
			return nil, errIngressEvidence
		}
		decoder := json.NewDecoder(bytes.NewReader(measurement.Security))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil || value == nil {
			return nil, errIngressEvidence
		}
		return value, nil
	}
	one, err := security(before)
	if err != nil {
		return refuse()
	}
	two, err := security(after)
	if err != nil || !reflect.DeepEqual(one, two) {
		return refuse()
	}
	result := ingressAccessResult{Public200: observation.Public200, Public503: observation.Public503}
	for _, peer := range observation.Sources {
		if !peer.IsGlobalUnicast() || peer.Is4In6() || peer.Zone() != "" {
			return refuse()
		}
		first, err := decodeAccess(before, peer, since)
		if err != nil {
			return refuse()
		}
		last, err := decodeAccess(after, peer, until)
		if err != nil || first.Access != last.Access || first.Paths != last.Paths {
			return refuse()
		}
		switch first.Access {
		case "STORED_HOST_BLOCK_MATCH":
			result.Blocked++
		case "STORED_HOST_ALLOW_OVERRIDE":
			result.Allowed++
		case "NO_ACTIVE_STORED_BLOCKS", "OTHER_ACTIVE_STORED_BLOCKS":
			result.Unblocked++
		default:
			return refuse()
		}
	}
	return result, nil
}

func runIngressEntry(ctx context.Context, env func(string) string, run executor, send inputExecutor, pause func(context.Context) error, out io.Writer) int {
	unknown := func() int { _, _ = io.WriteString(out, "MAIL_INGRESS_STATE=INCONCLUSIVE\n"); return 3 }
	if env("GITHUB_REPOSITORY") != "devantler-tech/platform" || env("GITHUB_EVENT_NAME") != "workflow_dispatch" || env("GITHUB_REF") != "refs/heads/main" || env("GITHUB_RUN_ATTEMPT") != "1" || env("MAIL_INGRESS_CONFIRM") != "read-email-ingress-source" || send == nil {
		return unknown()
	}
	client := ingressHTTPClient()
	defer client.CloseIdleConnections()
	observer := func(ctx context.Context, app binding, addresses accessAddresses) (ingressObservation, error) {
		return observeIngressSources(ctx, run, app, addresses, func(ctx context.Context) (int, error) { return probeIngressPublic(ctx, client) }, pause, time.Now)
	}
	result, err := collectAccessWithIngress(ctx, run, send, pause, time.Now, observer)
	if err != nil || result.Ingress == nil {
		return unknown()
	}
	value := result.Ingress
	line := fmt.Sprintf("MAIL_INGRESS_STATE=OBSERVED\nMAIL_INGRESS_BLOCKED_PEERS=%d\nMAIL_INGRESS_ALLOWED_PEERS=%d\nMAIL_INGRESS_UNBLOCKED_PEERS=%d\nMAIL_INGRESS_PUBLIC_HTTP_200=%d\nMAIL_INGRESS_PUBLIC_HTTP_503=%d\nMAIL_INGRESS_LOCAL_PATHS=%s\n", value.Blocked, value.Allowed, value.Unblocked, value.Public200, value.Public503, result.Paths)
	if n, err := io.WriteString(out, line); err != nil || n != len(line) {
		return 3
	}
	return 0
}
