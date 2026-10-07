package main

import (
	"context"
	"net/http"
	"time"
)

const failHealth outcome = "FAIL_HEALTH"

func verifyTransportProduction(reviewed configuration) outcome {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return verifyTransportRuntime(ctx, reviewed, runtimeOperations{execute: kubectl, forward: forwardBao, handshake: listenerTLS, health: verifyTransportHealth})
}

// Transport mode terminates at unauthenticated health. It has no identity
// callback and never requests a reader token or accesses the stored App key.
func verifyTransportRuntime(ctx context.Context, reviewed configuration, operations runtimeOperations) outcome {
	ca, status := liveConfiguration(ctx, reviewed, operations.execute)
	if status != pass {
		if status == failIdentity {
			return failConfig
		}
		return status
	}
	if trustedRoots(ca) == nil {
		return holdTransport
	}
	port, ok := baoListenerPort(reviewed.store.server)
	if !ok {
		return holdTransport
	}
	endpoint, stop, err := operations.forward(ctx, port)
	if err != nil {
		return failTransport
	}
	defer stop()
	if operations.handshake(ctx, endpoint, ca) != nil {
		return failTransport
	}
	return operations.health(ctx, endpoint, ca)
}

func verifyTransportHealth(ctx context.Context, endpoint string, ca []byte) outcome {
	roots := trustedRoots(ca)
	if !httpsOrigin(endpoint) || roots == nil {
		return holdTransport
	}
	client := secureClient(roots, baoTLSName)
	defer client.CloseIdleConnections()
	var health struct {
		Initialized *bool `json:"initialized"`
		Sealed      *bool `json:"sealed"`
	}
	// standbyok allows a healthy standby, never sealed/uninitialized overrides.
	_, status := request(ctx, client, http.MethodGet, endpoint+"/v1/sys/health?standbyok=true", "", nil, &health)
	if status != pass {
		return status
	}
	if health.Initialized == nil || !*health.Initialized || health.Sealed == nil || *health.Sealed {
		return failHealth
	}
	return pass
}
