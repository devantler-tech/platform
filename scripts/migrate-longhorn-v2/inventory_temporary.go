package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

// Resolve an ordinal only from the complete reviewed pool roster. The roster
// commitment binds the second observation even when another pool member changes.
func resolveTemporaryInventoryTarget(selection string, nodes []object, pools map[string]migrationPool) (string, string, error) {
	return resolveTemporarySelection(selection, nodes, pools, nil)
}

func resolveTemporarySelection(selection string, nodes []object, pools map[string]migrationPool, postReset *journal) (string, string, error) {
	var role string
	var ordinal int
	switch selection {
	case "temporary-v1-1":
		role = "v1"
	case "temporary-v1-2":
		role, ordinal = "v1", 1
	case "temporary-v2-1":
		role = "v2"
	default:
		return "", "", errInventory
	}
	if len(pools) != 2 {
		return "", "", errInventory
	}
	for _, expected := range []struct {
		role  string
		count int
	}{{"v1", 2}, {"v2", 1}} {
		pool := pools[expected.role]
		if pool.name != "autoscale-longhorn-"+expected.role || pool.role != expected.role || pool.count != expected.count || pool.serverType != "cpx42" {
			return "", "", errInventory
		}
	}
	selected := map[string][]string{}
	roster := []object{}
	seen := map[string]bool{}
	for _, node := range nodes {
		name := text(node, "metadata", "name")
		nodeRole := text(node, "metadata", "labels", migrationPoolLabel)
		if !strings.HasPrefix(name, "autoscale-longhorn-") && nodeRole == "" {
			continue
		}
		pool, exists := pools[nodeRole]
		memberErr := verifyMigrationNode(node, pool)
		if postReset != nil && name == postReset.Target {
			memberErr = verifyTemporaryResetMember(node, pool, *postReset)
		}
		if !exists || memberErr != nil || !inventoryBoot.MatchString(text(node, "status", "nodeInfo", "bootID")) {
			return "", "", errInventory
		}
		identity := object{"name": name, "uid": text(node, "metadata", "uid"), "providerID": text(node, "spec", "providerID"),
			"hardware": strings.ToLower(text(node, "status", "nodeInfo", "systemUUID")), "boot": text(node, "status", "nodeInfo", "bootID")}
		for _, key := range []string{"name", "uid", "providerID", "hardware"} {
			value := key + ":" + text(identity, key)
			if seen[value] {
				return "", "", errInventory
			}
			seen[value] = true
		}
		selected[nodeRole] = append(selected[nodeRole], name)
		roster = append(roster, identity)
	}
	for poolRole, pool := range pools {
		if len(selected[poolRole]) != pool.count {
			return "", "", errInventory
		}
		slices.Sort(selected[poolRole])
	}
	slices.SortFunc(roster, func(a, b object) int { return strings.Compare(text(a, "name"), text(b, "name")) })
	data, err := json.Marshal(roster)
	if err != nil {
		return "", "", errInventory
	}
	digest := sha256.Sum256(data)
	return selected[role][ordinal], hex.EncodeToString(digest[:]), nil
}
