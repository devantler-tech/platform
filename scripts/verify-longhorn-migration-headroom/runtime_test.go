package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

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
