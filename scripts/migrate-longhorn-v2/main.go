package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

func main() {
	target := os.Getenv("LONGHORN_V2_NODE")
	operation := os.Getenv("LONGHORN_V2_OPERATION")
	var authorization error
	switch operation {
	case "enable-capacity":
		authorization = authorizeCapacity(os.Getenv, os.Getenv("LONGHORN_V2_POOL"))
	case "refresh-clients":
		authorization = authorizeRefresh(os.Getenv)
	case "benchmark-v1", "benchmark-v2":
		authorization = authorizeBenchmark(os.Getenv, operation[len("benchmark-"):])
	default:
		authorization = authorize(os.Getenv, target)
	}
	if authorization != nil {
		fmt.Fprintln(os.Stderr, authorization)
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
	if operation == "enable-capacity" {
		if err = activateCapacityAfterBaseline(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), pools[os.Getenv("LONGHORN_V2_POOL")]); err != nil {
			fmt.Fprintln(os.Stderr, "Migration capacity activation held:", err)
			os.Exit(1)
		}
		return
	}
	if operation == "refresh-clients" {
		if err = refreshClients(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), pools); err != nil {
			fmt.Fprintln(os.Stderr, "Worker client refresh held:", err)
			os.Exit(1)
		}
		return
	}
	if operation == "benchmark-v1" || operation == "benchmark-v2" {
		if err = runBenchmark(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), operation[len("benchmark-"):]); err != nil {
			fmt.Fprintln(os.Stderr, "Storage benchmark held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its journal and scratch resources for explicit recovery; no baseline is accepted.")
			os.Exit(1)
		}
		return
	}
	p := preparation{c: client{run: execute, context: "admin@prod"}, target: target, name: "longhorn-v2-prepare-" + target,
		source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools}
	if err := p.prepare(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Node preparation held:", err)
		fmt.Fprintln(os.Stderr, "Preserve the journal and any owned cordon; diagnose before a new confirmed main dispatch.")
		os.Exit(1)
	}
}
