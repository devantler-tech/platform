package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// A pool scaled to zero still needs admission proof: it can grow while paused.
// Existing nodes are checked independently of which need temporary client labels.
func (e engine) replicaExclusion(ctx context.Context) error {
	w, err := e.c.read(ctx, "mutatingwebhookconfiguration", "", "kyverno-resource-mutating-webhook-cfg")
	if err != nil {
		return err
	}
	has := func(a []any, s string) bool {
		for _, v := range a {
			if v == s {
				return true
			}
		}
		return false
	}
	failClosed := false
	for _, hook := range items(w, "webhooks") {
		if str(hook, "failurePolicy") != "Fail" {
			continue
		}
		for _, rule := range items(hook, "rules") {
			groups, _ := val(rule, "apiGroups").([]any)
			resources, _ := val(rule, "resources").([]any)
			operations, _ := val(rule, "operations").([]any)
			versions, _ := val(rule, "apiVersions").([]any)
			if has(groups, "longhorn.io") && has(resources, "nodes") && has(operations, "CREATE") && has(operations, "UPDATE") && has(versions, "v1beta2") {
				failClosed = true
			}
		}
	}
	if !failClosed {
		return errors.New("compute-worker replica exclusion is not fail-closed")
	}
	probe := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Node", "metadata": object{"name": "autoscale-client-preflight-" + strings.ReplaceAll(e.holder, ".", "-"), "namespace": "longhorn-system"}, "spec": object{"allowScheduling": true, "evictionRequested": false, "disks": object{}}}
	b, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	b, err = e.c.command(ctx, b, "create", "--dry-run=server", "-f", "-", "-o", "json")
	if err != nil {
		return err
	}
	var result object
	if json.Unmarshal(b, &result) != nil || val(result, "spec", "allowScheduling") != false || val(result, "spec", "evictionRequested") != true {
		return errors.New("admission did not exclude replica storage")
	}
	nodes, err := e.c.list(ctx, "nodes.longhorn.io", "longhorn-system")
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if strings.HasPrefix(str(n, "metadata", "name"), "autoscale-") && (val(n, "spec", "allowScheduling") != false || val(n, "spec", "evictionRequested") != true) {
			return errors.New("existing compute-worker replica exclusion is unproven")
		}
	}
	replicas, err := e.c.list(ctx, "replicas.longhorn.io", "longhorn-system")
	if err != nil {
		return err
	}
	for _, r := range replicas {
		if strings.HasPrefix(str(r, "spec", "nodeID"), "autoscale-") {
			return errors.New("compute-worker replica storage exists")
		}
	}
	return nil
}
