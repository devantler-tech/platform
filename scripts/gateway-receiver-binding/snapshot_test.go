package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFenceInstallationRetainsTheConvergenceObservation(t *testing.T) {
	for _, scenario := range []string{"complete", "receiver-replaced", "binding-updated", "source-changed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Chdir(filepath.Join("..", ".."))
			api, b := newFenceAPI(t)
			observed := receiverObservation{Binding: b, ReceiverUID: "22222222-2222-4222-8222-222222222222", Bindings: [2]admissionIdentity{{UID: "44444444-4444-4444-8444-444444444444", Version: "10"}, {UID: "44444444-4444-4444-8444-444444444444", Version: "10"}}}
			switch scenario {
			case "receiver-replaced":
				api.config["metadata"].(map[string]any)["uid"] = "33333333-3333-4333-8333-333333333333"
			case "binding-updated":
				api.bindings[preparedBindings[0].Resource] = []byte(strings.Replace(string(api.bindings[preparedBindings[0].Resource]), `"resourceVersion":"10"`, `"resourceVersion":"11"`, 1))
			case "source-changed":
				observed.Binding.CIDR = "192.0.2.6/32"
			}
			wrote, err := (receiverClient{context: "admin@prod", command: api.command, expected: &observed}).installFence(context.Background(), b, func(context.Context, time.Duration) error { return nil })
			if scenario == "complete" {
				if err != nil || !wrote || len(api.writes) != 1 {
					t.Fatal("matching observation did not install its fence")
				}
			} else if err == nil || wrote || len(api.writes) != 0 {
				t.Fatal("changed convergence observation authorized a fence write")
			}
		})
	}
}
