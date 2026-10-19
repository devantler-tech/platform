package native_test

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestNativeListenerReservations proves initial listeners cannot reuse one another's addresses.
func TestNativeListenerReservations(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		start bool
	}{
		{name: "before startup"},
		{name: "after first startup", start: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// A no-op executable exercises the real start path without binding
			// sockets itself, making each reservation's release observable.
			binary, err := exec.LookPath("true")
			if err != nil {
				t.Fatal(err)
			}
			c := newCluster(t, binary)
			if scenario.start {
				c.start(serverNames[0], true)
			}
			seen := map[string]bool{}
			for _, name := range serverNames {
				for _, addr := range []string{c.servers[name].apiAddr, c.servers[name].clusterAddr} {
					if seen[addr] {
						t.Fatal("native servers share a listener address")
					}
					seen[addr] = true
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					probe, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
					cancel()
					wantReserved := !scenario.start || name != serverNames[0]
					if err == nil {
						if closeErr := probe.Close(); closeErr != nil {
							t.Fatal(closeErr)
						}
						if wantReserved {
							t.Errorf("%s listener was released before its server started", name)
						}
					} else if !wantReserved {
						t.Errorf("started server retained a reservation: %v", err)
					} else if !errors.Is(err, syscall.EADDRINUSE) {
						t.Errorf("listener was not proven reserved: %v", err)
					}
				}
			}
		})
	}
}
