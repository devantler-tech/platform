package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCountHoldReportsOnlyAggregatesWithoutReceipt(t *testing.T) {
	for _, counts := range [][2]int{{9, 8}, {8, 9}, {7, 7}} {
		t.Run(fmt.Sprintf("provider%d-node%d", counts[0], counts[1]), func(t *testing.T) {
			servers, nodes := roster(t, true)
			servers = append(servers, server{Name: "private-unmatched-server"})[:counts[0]]
			nodes = append(nodes, node{})[:counts[1]]
			body := providerResponse(t, func(response map[string]any) {
				response["servers"] = servers
				response["meta"].(map[string]any)["pagination"].(map[string]any)["total_entries"] = counts[0]
			})
			read := func(context.Context) ([]byte, error) { return nodeResponse(t, nodes), nil }
			n, err := observe(context.Background(), providerClient(t, body, 200), "private-test-token", read, false)
			want := fmt.Sprintf("inventory count gate failed: provider_servers=%d kubernetes_nodes=%d expected_equal=true permitted_nodes=8..9", counts[0], counts[1])
			if n != 0 || err == nil || err.Error() != want {
				t.Fatalf("count failure must expose only aggregates and no receipt: receipt=%d error=%v want=%q", n, err, want)
			}
		})
	}
}

func nodeResponse(t *testing.T, n []node) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "NodeList", "metadata": map[string]any{"resourceVersion": "123"}, "items": n})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestObservationBracketsProviderRead(t *testing.T) {
	_, nodes := roster(t, false)
	calls := 0
	read := func(context.Context) ([]byte, error) { calls++; return nodeResponse(t, nodes), nil }
	n, err := observe(context.Background(), providerClient(t, providerResponse(t, nil), 200), "private-test-token", read, false)
	if err != nil || n != 8 || calls != 2 {
		t.Fatalf("qualified observation failed: nodes=%d calls=%d err=%v", n, calls, err)
	}
}

func TestObservationDiscardsChangedOrUnavailableInventory(t *testing.T) {
	for _, mode := range []string{"node replacement", "read failure", "expired context"} {
		t.Run(mode, func(t *testing.T) {
			_, nodes := roster(t, false)
			calls := 0
			ctx := context.Background()
			if mode == "expired context" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			read := func(context.Context) ([]byte, error) {
				calls++
				if calls == 2 {
					if mode == "read failure" {
						return nil, errors.New("private error")
					}
					nodes[0].Metadata.UID = "replacement"
				}
				return nodeResponse(t, nodes), nil
			}
			n, err := observe(ctx, providerClient(t, providerResponse(t, nil), 200), "private-test-token", read, false)
			if err == nil || n != 0 {
				t.Fatal("unsafe observation produced a capacity receipt")
			}
		})
	}
}
