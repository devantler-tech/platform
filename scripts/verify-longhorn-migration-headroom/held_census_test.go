package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHeldCensusReportsOnlyExtraRecordLabelsAfterStableReads(t *testing.T) {
	servers, nodes := roster(t, true)
	servers[0].Labels["ksail.node.index"] = "private-index-canary"
	servers = append(servers, server{ID: 999, Name: "private-provider-canary", Status: "running", Labels: map[string]string{
		"ksail.io/cluster": "prod", "ksail.io/snapshot-build": "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}})
	body := providerResponse(t, func(response map[string]any) {
		response["servers"] = servers
		response["meta"].(map[string]any)["pagination"].(map[string]any)["total_entries"] = 10
	})
	providerReads, nodeReads := 0, 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		providerReads++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	read := func(context.Context) ([]byte, error) {
		nodeReads++
		return nodeResponse(t, nodes), nil
	}
	result, err := observeAccountCensus(t.Context(), client, "private-token-canary", read)
	if result != (accountCensus{}) || !errors.Is(err, errCensusHeld) || !strings.Contains(err.Error(), "field=index") || !strings.Contains(err.Error(), "provider_only=1 classification=snapshot-build-labeled state=running ownership=UNVERIFIED headroom=UNVERIFIED") {
		t.Fatalf("held census lost its safe diagnostic or emitted a receipt: %+v %v", result, err)
	}
	if providerReads != 2 || nodeReads != 2 {
		t.Fatalf("held diagnostic lacked both complete brackets: provider=%d node=%d", providerReads, nodeReads)
	}
	if strings.Contains(err.Error(), "private-") {
		t.Fatal("held diagnostic exposed an unreviewed native value")
	}
}

func TestHeldCensusCannotClassifyChangingOrUnreadableInventories(t *testing.T) {
	for _, mode := range []string{"provider change", "node change", "final node failure", "second provider failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			servers, nodes := roster(t, true)
			servers[0].Labels["ksail.node.index"] = "private-index-canary"
			servers = append(servers, server{ID: 999, Name: "private-provider-canary", Status: "running", Labels: map[string]string{
				"ksail.io/cluster": "prod", "ksail.io/snapshot-build": "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			providerReads, nodeReads := 0, 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				providerReads++
				if providerReads == 2 && mode == "second provider failure" {
					return nil, errors.New("private-transport-canary")
				}
				if providerReads == 2 && mode == "provider change" {
					servers[9].Status = "stopping"
				}
				body := providerResponse(t, func(response map[string]any) {
					response["servers"] = servers
					response["meta"].(map[string]any)["pagination"].(map[string]any)["total_entries"] = 10
				})
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			read := func(context.Context) ([]byte, error) {
				nodeReads++
				if nodeReads == 2 {
					switch mode {
					case "node change":
						nodes[0].Metadata.UID = "private-replaced-canary"
					case "final node failure":
						return nil, errors.New("private-final-read-canary")
					case "cancelled":
						cancel()
					}
				}
				return nodeResponse(t, nodes), nil
			}
			result, err := observeAccountCensus(ctx, client, "private-token-canary", read)
			if err == nil || result != (accountCensus{}) || strings.Contains(err.Error(), "classification=") || strings.Contains(err.Error(), "private-") || providerReads != 2 {
				t.Fatalf("unqualified held observation reached classification: %+v %v reads=%d", result, err, providerReads)
			}
		})
	}
}
