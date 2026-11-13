package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

var runtimeFixturePaths = []string{
	"/api/v1/nodes", "/api/v1/pods",
	"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/nodes",
	"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes",
	"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/engines",
	"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/replicas",
	"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/instancemanagers",
	"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/orphans",
	"/apis/apps/v1/namespaces/longhorn-system/daemonsets",
	"/apis/apps/v1/namespaces/longhorn-system/deployments",
	"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/settings/default-instance-manager-image",
	"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/settings/guaranteed-instance-manager-cpu",
}

func TestRuntimeObservationAdmitsOnlyFixedReads(t *testing.T) {
	for _, path := range runtimeFixturePaths {
		args := []string{"--context=admin@prod", "--request-timeout=20s", "get", "--raw=" + path}
		if !runtimeObservationReadAllowed("kubectl", args...) {
			t.Fatalf("fixed native read refused: %s", path)
		}
		for _, position := range []int{0, 2, 3} {
			changed := slices.Clone(args)
			changed[position] = "--foreign"
			if runtimeObservationReadAllowed("kubectl", changed...) {
				t.Fatal("changed scope, verb or path admitted")
			}
		}
	}
	for _, args := range [][]string{
		{"--context=admin@prod", "--request-timeout=20s", "exec", "pod", "--", "true"},
		{"--context=admin@prod", "--request-timeout=20s", "get", "--raw=/api/v1/secrets"},
		{"--context=admin@prod", "--request-timeout=20s", "get", "--raw=/api/v1/nodes/worker/proxy/stats/summary"},
		{"--context=admin@prod", "--request-timeout=20s", "get", "--raw=/api/v1/pods?labelSelector=hidden"},
	} {
		if runtimeObservationReadAllowed("kubectl", args...) {
			t.Fatal("a non-inventory request was admitted")
		}
	}
	if runtimeObservationReadAllowed("talosctl", "get", "nodes") || runtimeObservationReadAllowed("hcloud", "server", "list") {
		t.Fatal("runtime-only reader admitted a host or provider client")
	}
}

// The API fixture is synthetic; only the opt-in native test is live evidence.
func runtimeObservationFixture(t *testing.T, fault string) (client, map[string]int) {
	t.Helper()
	reads := map[string]int{}
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if input != nil || !runtimeObservationReadAllowed(program, args...) {
			t.Fatal("runtime observer selected a write or an unreviewed request")
		}
		if program == "git" {
			reads["source"]++
			sha := strings.Repeat("a", 40)
			if fault == "source changed" && reads["source"] > 1 {
				sha = strings.Repeat("b", 40)
			}
			return []byte(sha + "\trefs/heads/main\n"), nil
		}
		path := strings.TrimPrefix(args[3], "--raw=")
		reads[path]++
		if fault == "read failed" {
			return nil, errors.New("private-runtime-read-canary")
		}
		index := slices.Index(runtimeFixturePaths, path)
		if index < 0 {
			t.Fatal("unlisted fixture API")
		}
		api, kind, namespace := "longhorn.io/v1beta2", "Node", "longhorn-system"
		switch index {
		case 0:
			api, kind, namespace = "v1", "Node", ""
		case 1:
			api, kind = "v1", "Pod"
		case 3:
			kind = "Volume"
		case 4:
			kind = "Engine"
		case 5:
			kind = "Replica"
		case 6:
			kind = "InstanceManager"
		case 7:
			kind = "Orphan"
		case 8:
			api, kind = "apps/v1", "DaemonSet"
		case 9:
			api, kind = "apps/v1", "Deployment"
		case 10, 11:
			kind = "Setting"
		}
		name := strings.ToLower(kind) + "-fixture"
		item := object{"metadata": object{"name": name, "namespace": namespace, "uid": fmt.Sprintf("%08d-1111-4111-8111-111111111111", index+1), "resourceVersion": "10"}, "spec": object{"dataEngine": "v1"}, "status": object{"currentState": "running"}}
		if index == 0 {
			value(item, "metadata").(object)["name"] = "worker-fixture"
			item["status"] = object{"nodeInfo": object{"bootID": "boot-one"}, "allocatable": object{"cpu": "8", "memory": "12Gi"}}
		}
		if index == 1 {
			item["spec"] = object{"nodeName": "worker-fixture", "containers": []any{object{"name": "fixture", "image": "image@sha256:" + strings.Repeat("a", 64), "resources": object{"requests": object{"cpu": "480m", "memory": "128Mi"}}}}}
			item["status"] = object{"phase": "Running", "containerStatuses": []any{object{"name": "fixture", "containerID": "containerd://" + strings.Repeat("a", 64), "imageID": "sha256:" + strings.Repeat("a", 64), "restartCount": float64(0), "state": object{"running": object{"startedAt": "2026-11-12T00:00:00Z"}}}}}
		}
		if index == 10 || index == 11 {
			value(item, "metadata").(object)["name"] = path[strings.LastIndex(path, "/")+1:]
			item["apiVersion"], item["kind"] = api, kind
			item["value"] = "docker.io/longhornio/longhorn-instance-manager:v1.13.0"
			if index == 11 {
				item["value"] = `{"v1":"6","v2":"6"}`
			}
		}
		if fault == "epoch changed" && index == 1 && reads[path] > 1 {
			value(item, "status", "containerStatuses").([]any)[0].(object)["containerID"] = "containerd://" + strings.Repeat("b", 64)
		}
		if fault == "UID changed" && index == 3 && reads[path] > 1 {
			value(item, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
		}
		if fault == "missing identity" {
			delete(value(item, "metadata").(object), "uid")
		}
		if fault == "incidental status" && reads[path] > 1 {
			value(item, "metadata").(object)["resourceVersion"] = "11"
			value(item, "status").(object)["incidental"] = "advanced"
		}
		if kind == "Setting" {
			return json.Marshal(item)
		}
		body := object{"apiVersion": api, "kind": kind + "List", "metadata": object{"resourceVersion": "20"}, "items": []any{item}}
		if fault == "partial inventory" {
			value(body, "metadata").(object)["continue"] = "unread"
		}
		if fault == "wrong API" {
			body["apiVersion"] = "foreign/v1"
		}
		if fault == "duplicate identity" {
			body["items"] = []any{item, item}
		}
		return json.Marshal(body)
	}}
	return c, reads
}

func TestRuntimeObservationBracketsEveryInventoryAndRemainsReadOnly(t *testing.T) {
	c, reads := runtimeObservationFixture(t, "")
	got, err := observeRuntimeInventory(t.Context(), c, strings.Repeat("a", 40))
	if err != nil || got.Nodes != 1 || got.InstanceManagers != 1 || got.V1Volumes != 1 || len(got.EpochSHA) != 64 || got.Default != "stock" || reads["source"] != 2 {
		t.Fatalf("complete read-only inventory refused: %+v %v", got, err)
	}
	for _, path := range runtimeFixturePaths {
		if reads[path] != 2 {
			t.Errorf("native inventory not bracketed: %s reads=%d", path, reads[path])
		}
	}
}

func TestRuntimeObservationRefusesAmbiguityAndDriftWithoutLeakingReadErrors(t *testing.T) {
	for _, fault := range []string{"source changed", "epoch changed", "UID changed", "missing identity", "partial inventory", "wrong API", "duplicate identity", "read failed"} {
		t.Run(fault, func(t *testing.T) {
			c, _ := runtimeObservationFixture(t, fault)
			got, err := observeRuntimeInventory(t.Context(), c, strings.Repeat("a", 40))
			if err == nil || got.EpochSHA != "" || strings.Contains(err.Error(), "private-runtime-read-canary") {
				t.Fatal("changed or incomplete inventory produced a witness or disclosed a native error")
			}
		})
	}
	c, _ := runtimeObservationFixture(t, "incidental status")
	if _, err := observeRuntimeInventory(t.Context(), c, strings.Repeat("a", 40)); err != nil {
		t.Fatal("incidental resource version/status update blocked stable runtime evidence")
	}
}
