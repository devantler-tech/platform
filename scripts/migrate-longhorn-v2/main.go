package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

func main() {
	if err := authorizeRefresh(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Minute)
	defer cancel()
	config, err := os.ReadFile("ksail.prod.yaml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Reviewed capacity configuration is unreadable.")
		os.Exit(1)
	}
	pools, err := configuredMigrationPools(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = refreshClients(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), pools); err != nil {
		fmt.Fprintln(os.Stderr, "Worker client refresh held:", err)
		os.Exit(1)
	}
}
