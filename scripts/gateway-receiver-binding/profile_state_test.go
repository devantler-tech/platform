package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestProviderOperationRequiresTheRequestedNativeTransportProfile(t *testing.T) {
	for _, tc := range []struct {
		want, actual         bool
		operations, requests int
		fails                bool
	}{
		{false, false, 1, 4, false}, {true, true, 1, 4, false}, {false, true, 0, 2, true}, {true, false, 0, 2, true},
	} {
		lb, b := providerFixture()
		for _, service := range lb["services"].([]any) {
			service.(map[string]any)["proxyprotocol"] = tc.actual
		}
		client, calls := providerHTTP(t, lb)
		operations := 0
		err := withProviderMode(context.Background(), b, tc.want, func(ctx context.Context, actual binding, want bool) (providerIdentity, error) {
			return observeProvider(ctx, client, "disposable-test-token", actual, want)
		}, func() error { operations++; return nil })
		if (err != nil) != tc.fails || operations != tc.operations || *calls != tc.requests {
			t.Fatalf("native provider profile did not gate the operation: wanted=%t actual=%t operations=%d requests=%d error=%v", tc.want, tc.actual, operations, *calls, err)
		}
	}
}

func TestReceiverVerificationUsesTheRequestedFencedMode(t *testing.T) {
	for _, tc := range []struct{ want, actual, fence, passes bool }{
		{false, false, true, true}, {true, true, true, true}, {false, true, true, false}, {true, false, true, false}, {true, true, false, false},
	} {
		t.Run(stringMode(tc.want, tc.actual, tc.fence), func(t *testing.T) {
			t.Chdir(filepath.Join("..", ".."))
			config, b := disabledFixture()
			if tc.actual {
				config["spec"].(map[string]any)["resources"].([]any)[1].(map[string]any)["listenerFilters"] = activeReceiverFilters()
			}
			raw, _ := json.Marshal(config)
			state := map[string][]byte{"ciliumenvoyconfig": raw}
			for _, r := range preparedBindings {
				state[r.Resource] = registryFixture(t, r, b)
			}
			writes := 0
			c := fakeReceiver(state, &writes)
			err := c.receiverProfile(context.Background(), b, transportProfile{Fence: tc.fence, Proxy: tc.want})
			if (err == nil) != tc.passes || writes != 0 {
				t.Fatal("profile verification admitted the wrong transport or changed the receiver")
			}
		})
	}
}

func stringMode(want, actual, fence bool) string {
	name := "disabled"
	if want {
		name = "active"
	}
	if actual {
		name += "-observed-active"
	} else {
		name += "-observed-disabled"
	}
	if !fence {
		name += "-unselected"
	}
	return name
}
