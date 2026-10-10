package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func verifyRunnerGroupProduction(reviewed configuration) outcome {
	interrupt, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(interrupt, 2*time.Minute)
	defer cancel()
	runID := os.Getenv("GITHUB_RUN_ID")
	return verifyRuntime(ctx, reviewed, runtimeOperations{execute: kubectl, forward: forwardBao, handshake: listenerTLS, identity: func(ctx context.Context, options verificationOptions) outcome {
		options.afterIdentity = func(ctx context.Context, verified verificationOptions, jwt string, installationID int64) outcome {
			return verifyRunnerGroupCapability(ctx, verified, jwt, installationID, runID)
		}
		return verify(ctx, options)
	}})
}
