package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAccountCensusReportsBuilderWithoutGrantingCapacity(t *testing.T) {
	servers, nodes := roster(t, true)
	servers = append(servers, server{ID: 999, Name: "temporary-provider-fixture", Status: "running", Labels: map[string]string{
		"ksail.io/cluster": "prod", "ksail.io/snapshot-build": "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}})
	body := providerResponse(t, func(response map[string]any) {
		response["servers"] = servers
		response["meta"].(map[string]any)["pagination"].(map[string]any)["total_entries"] = len(servers)
	})
	client := providerClient(t, body, 200)
	readNodes := func(context.Context) ([]byte, error) { return nodeResponse(t, nodes), nil }
	got, err := observeAccountCensus(t.Context(), client, "private-test-token", readNodes)
	if err != nil || got != (accountCensus{Servers: 10, Nodes: 9, Extra: 1, Classification: "snapshot-build-labeled", State: "running"}) {
		t.Fatalf("provider-only builder was not accounted for: %+v, %v", got, err)
	}
	if count, err := observe(t.Context(), client, "private-test-token", readNodes, false); err == nil || count != 0 {
		t.Fatal("the full account qualified for the reserved builder slot")
	}
}

func TestAccountCensusRefusesAmbiguousIdentityAndNames(t *testing.T) {
	servers, nodes := roster(t, true)
	servers = append(servers, server{ID: 999, Name: "snapshot-builder-prod", Status: "off"})
	got, err := classifyAccountCensus(servers, nodes)
	if err != nil || got.Classification != "unclassified" || got.State != "not-running" {
		t.Fatalf("an unowned name was attributed to a builder: %+v, %v", got, err)
	}
	servers[9].ID = servers[0].ID
	if _, err := classifyAccountCensus(servers, nodes); err == nil {
		t.Fatal("duplicate provider identity qualified")
	}
}

func TestAccountCensusRejectsProviderDrift(t *testing.T) {
	servers, nodes := roster(t, true)
	body := func(status string) string {
		return providerResponse(t, func(response map[string]any) {
			response["servers"] = append(servers, server{ID: 999, Name: "temporary-provider-fixture", Status: status})
			response["meta"].(map[string]any)["pagination"].(map[string]any)["total_entries"] = 10
		})
	}
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		payload := body("running")
		if calls > 0 {
			payload = body("stopping")
		}
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}
	read := func(context.Context) ([]byte, error) { return nodeResponse(t, nodes), nil }
	if result, err := observeAccountCensus(t.Context(), client, "private-test-token", read); err == nil || result != (accountCensus{}) || calls != 2 {
		t.Fatal("a changing provider account produced a reconciliation receipt")
	}
}

func TestAccountCensusRequiresExactProtectedDispatch(t *testing.T) {
	env := map[string]string{
		"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/observe-longhorn-migration-headroom.yaml@refs/heads/main",
		"GITHUB_RUN_ATTEMPT":  "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"LONGHORN_HEADROOM_CONFIRM": "observe-longhorn-provider-account",
	}
	get := func(key string) string { return env[key] }
	if !authorizeAccountCensus(get) {
		t.Fatal("exact protected account reconciliation was refused")
	}
	for key, value := range env {
		env[key] = ""
		if authorizeAccountCensus(get) {
			t.Fatalf("account reconciliation authorized without %s", key)
		}
		env[key] = value
	}
}
