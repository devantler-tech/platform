package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTalosReadsMustBindToTheKubernetesWorkerHardware(t *testing.T) {
	for _, uuid := range []string{"machine-uuid", "other-cluster-hardware", ""} {
		c := client{run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
			if program != "talosctl" || len(args) != 5 || args[1] != "get" || args[2] != "systeminformation" {
				t.Fatal("hardware proof attempted a privileged command")
			}
			return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": uuid}})
		}}
		err := c.verifyHost(context.Background(), "10.0.99.4", nodeFixture())
		if (err == nil) != (uuid == "machine-uuid") {
			t.Fatalf("incorrect endpoint binding for %q: %v", uuid, err)
		}
	}
}
