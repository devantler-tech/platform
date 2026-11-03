package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// main admits explicit migration operations through their distinct confirmations
// and protected workflow identities before constructing any production client.
func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "Migration operations accept no command-line arguments.")
		os.Exit(1)
	}
	operation := os.Getenv("LONGHORN_V2_OPERATION")
	target := os.Getenv("LONGHORN_V2_NODE")
	var authorization error
	switch operation {
	case "prepare-worker":
		authorization = authorize(os.Getenv, target)
	case "prepare-unused-temporary-worker":
		authorization = authorizeTemporaryPreparation(os.Getenv, target)
	case "benchmark-v1", "benchmark-v2":
		authorization = authorizeBenchmark(os.Getenv, operation[len("benchmark-"):])
	case "register-temporary-v2-storage":
		authorization = authorizeTemporaryActivation(os.Getenv)
	case "inventory-replicas":
		authorization = authorizeInventory(os.Getenv, target)
	case "inspect-host-memory":
		authorization = authorizeHostMemory(os.Getenv, target)
	case "reboot-host-for-hugepages":
		authorization = authorizeHostReboot(os.Getenv, target)
	case "inventory-storage-root":
		authorization = authorizeStorageRoot(os.Getenv, target)
	case "inventory-storage-layout":
		authorization = authorizeStorageLayout(os.Getenv, target)
	default:
		authorization = authorizeRefresh(os.Getenv)
	}
	if authorization != nil {
		fmt.Fprintln(os.Stderr, authorization)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Minute)
	defer cancel()
	if operation == "inventory-storage-layout" {
		if err := runStorageLayoutInventory(ctx, target, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, inventoryFailureMessage(err))
			os.Exit(1)
		}
		return
	}
	if operation == "inspect-host-memory" {
		if err := runHostMemory(ctx, target, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, inventoryFailureMessage(err))
			os.Exit(1)
		}
		return
	}
	if operation == "inventory-storage-root" {
		if err := runStorageRootInventory(ctx, target, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, inventoryFailureMessage(err))
			os.Exit(1)
		}
		return
	}
	if operation == "inventory-replicas" {
		if err := runReplicaInventory(ctx, target, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID")); err != nil {
			fmt.Fprintln(os.Stderr, inventoryFailureMessage(err))
			os.Exit(1)
		}
		return
	}
	if operation == "benchmark-v1" || operation == "benchmark-v2" {
		if err := runBenchmark(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), operation[len("benchmark-"):]); err != nil {
			fmt.Fprintln(os.Stderr, "Storage benchmark held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its journal and scratch resources for explicit recovery; no baseline is accepted.")
			os.Exit(1)
		}
		return
	}
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
	if operation == "register-temporary-v2-storage" {
		activation := temporaryV2Activation{c: client{run: execute, context: "admin@prod", inventoryPools: pools}, source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools}
		if err = activation.run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Temporary V2 registration held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its identity journal and any partial registration for explicit recovery.")
			os.Exit(1)
		}
		return
	}
	if operation == "prepare-unused-temporary-worker" {
		c := client{run: execute, context: "admin@prod", inventoryPools: pools}
		nodes, readErr := c.list(ctx, "nodes", "")
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "Temporary preparation held: node inventory is unreadable.")
			os.Exit(1)
		}
		resolved, _, resolveErr := resolveTemporaryInventoryTarget(target, nodes, pools)
		if resolveErr != nil {
			// Recovery may begin with the selected worker's owned cordon. A
			// journal, not the ordinal alone, must prove that exception.
			cm, cmErr := c.get(ctx, "configmap", "longhorn-system", "longhorn-v2-prepare-"+target, true)
			if cmErr != nil || cm == nil {
				fmt.Fprintln(os.Stderr, "Temporary preparation held: reviewed pool selection is unproven.")
				os.Exit(1)
			}
			var record journal
			if json.Unmarshal([]byte(text(cm, "data", "record")), &record) != nil || record.TemporarySelection != target {
				fmt.Fprintln(os.Stderr, "Temporary preparation held: recovery identity is unproven.")
				os.Exit(1)
			}
			if record.Phase == "reset-requested" {
				resolved, resolveErr = resolveTemporaryResetRecovery(target, nodes, pools, record)
			} else {
				view, viewErr := temporaryInventoryNodes(nodes, &record)
				if viewErr != nil {
					fmt.Fprintln(os.Stderr, "Temporary preparation held: owned cordon is unproven.")
					os.Exit(1)
				}
				resolved, _, resolveErr = resolveTemporaryInventoryTarget(target, view, pools)
			}
			if resolveErr != nil || resolved != record.Target {
				fmt.Fprintln(os.Stderr, "Temporary preparation held: recovery roster changed.")
				os.Exit(1)
			}
		}
		p := temporaryPreparation{preparation: preparation{c: c, target: resolved, name: "longhorn-v2-prepare-" + target, temporarySelection: target, source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools}}
		if err = p.prepare(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Unused temporary preparation held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its journal and owned cordon; uncertain resets are never replayed.")
			os.Exit(1)
		}
		return
	}
	if operation == "reboot-host-for-hugepages" {
		h := hostMemoryReboot{c: client{run: execute, context: "admin@prod"}, target: target, source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools}
		if err = h.run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Host memory maintenance held:", err)
			fmt.Fprintln(os.Stderr, "Preserve its journal and owned cordon; uncertain reboot requests are never repeated.")
			os.Exit(1)
		}
		if _, err = fmt.Fprintln(os.Stdout, "Host memory maintenance recovered with original storage identities, native allocation, advertised capacity and database identity proved."); err != nil {
			os.Exit(1)
		}
		return
	}
	if operation == "prepare-worker" {
		p := preparation{c: client{run: execute, context: "admin@prod"}, target: target, name: "longhorn-v2-prepare-" + target,
			source: os.Getenv("GITHUB_SHA"), owner: os.Getenv("GITHUB_RUN_ID"), pools: pools}
		if err = p.prepare(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "Node preparation held:", err)
			fmt.Fprintln(os.Stderr, "Preserve the journal and any owned cordon; diagnose before a new confirmed main dispatch.")
			os.Exit(1)
		}
		return
	}
	if err = refreshClients(ctx, client{run: execute, context: "admin@prod"}, os.Getenv("GITHUB_SHA"), pools); err != nil {
		fmt.Fprintln(os.Stderr, "Worker client refresh held:", err)
		os.Exit(1)
	}
}
