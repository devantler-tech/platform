package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const runtimeInventoryOperation = "inventory-v1-runtime"
const runtimeRetirementImage = "ghcr.io/devantler-tech/platform-longhorn-instance-manager:v1.13.0-retirement.1-591ed0ccebd032c4d9705c356c267e724b30e2a9-1@sha256:4f7c2ff5666c6ce1ccab78a5e8c5c108147b12b80729303b9f1d91dfb9ee9cd7"

type runtimeInventoryQuery struct{ path, api, kind, namespace string }

func runtimeInventoryQueries() []runtimeInventoryQuery {
	const lh = "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/"
	return []runtimeInventoryQuery{
		{"/api/v1/nodes", "v1", "Node", ""},
		{"/api/v1/pods", "v1", "Pod", "*"},
		{lh + "nodes", "longhorn.io/v1beta2", "Node", "longhorn-system"},
		{lh + "volumes", "longhorn.io/v1beta2", "Volume", "longhorn-system"},
		{lh + "engines", "longhorn.io/v1beta2", "Engine", "longhorn-system"},
		{lh + "replicas", "longhorn.io/v1beta2", "Replica", "longhorn-system"},
		{lh + "instancemanagers", "longhorn.io/v1beta2", "InstanceManager", "longhorn-system"},
		{lh + "orphans", "longhorn.io/v1beta2", "Orphan", "longhorn-system"},
		{"/apis/apps/v1/namespaces/longhorn-system/daemonsets", "apps/v1", "DaemonSet", "longhorn-system"},
		{"/apis/apps/v1/namespaces/longhorn-system/deployments", "apps/v1", "Deployment", "longhorn-system"},
		{lh + "settings/default-instance-manager-image", "longhorn.io/v1beta2", "Setting", "longhorn-system"},
		{lh + "settings/guaranteed-instance-manager-cpu", "longhorn.io/v1beta2", "Setting", "longhorn-system"},
	}
}

func authorizeRuntimeInventory(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != runtimeInventoryOperation || getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return errors.New("the explicit target-free runtime inventory is required")
	}
	return authorizeWorkflow(getenv, runtimeInventoryOperation)
}

func runtimeObservationReadAllowed(program string, args ...string) bool {
	if program == "git" {
		return inventoryReadAllowed(program, args...)
	}
	if program != "kubectl" || len(args) != 4 || !slices.Equal(args[:3], []string{"--context=admin@prod", "--request-timeout=20s", "get"}) {
		return false
	}
	for _, q := range runtimeInventoryQueries() {
		if args[3] == "--raw="+q.path {
			return true
		}
	}
	return false
}

func runtimeInventoryReadCommand(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
	if input != nil || !runtimeObservationReadAllowed(program, args...) {
		return nil, errors.New("runtime inventory permits only fixed source and Kubernetes reads")
	}
	return inventoryExecuteCommand(ctx, program, args...)
}

// This receipt is an inventory, never adoption, headroom or retirement authority.
type runtimeInventoryReceipt struct {
	Nodes, KubernetesNodes, Pods, InstanceManagers, V1Volumes, V2Volumes int
	Default, EpochSHA                                                    string
}

func observeRuntimeInventory(ctx context.Context, c client, source string) (result runtimeInventoryReceipt, err error) {
	phase := "input"
	defer func() {
		if err != nil {
			result = runtimeInventoryReceipt{}
			err = fmt.Errorf("runtime inventory held: phase=%s", phase)
		}
	}()
	if c.context != "admin@prod" || c.run == nil || !commandContextActive(ctx) {
		return result, errors.New("protected production inventory context is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	delegate := c.run
	c.run = func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if input != nil || !runtimeObservationReadAllowed(program, args...) || !commandContextActive(ctx) {
			return nil, errors.New("runtime inventory request is outside its fixed read boundary")
		}
		return delegate(ctx, input, program, args...)
	}
	phase = "source-before"
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return result, err
	}
	phase = "inventory-before"
	before, err := readRuntimeInventory(ctx, c)
	if err != nil {
		return result, err
	}
	phase = "inventory-after"
	after, err := readRuntimeInventory(ctx, c)
	if err != nil || before != after {
		return result, errors.New("native identities, configuration or container epochs changed")
	}
	phase = "source-after"
	if err = sourceCurrent(ctx, c.run, source); err != nil || !commandContextActive(ctx) {
		return result, errors.New("source or context changed during inventory")
	}
	return after, nil
}

func readRuntimeInventory(ctx context.Context, c client) (runtimeInventoryReceipt, error) {
	refused := errors.New("native runtime inventory is incomplete or ambiguous")
	var result runtimeInventoryReceipt
	var records []object
	uids := map[string]bool{}
	for _, q := range runtimeInventoryQueries() {
		raw, err := c.kubectl(ctx, nil, "get", "--raw="+q.path)
		var root object
		if err != nil || len(raw) == 0 || len(raw) > 16<<20 || strictjson.Validate(raw, nil) != nil || json.Unmarshal(raw, &root) != nil || text(root, "apiVersion") != q.api {
			return runtimeInventoryReceipt{}, refused
		}
		items := []any{root}
		if q.kind != "Setting" {
			if text(root, "kind") != q.kind+"List" || text(root, "metadata", "resourceVersion") == "" || value(root, "metadata", "continue") != nil && value(root, "metadata", "continue") != "" {
				return runtimeInventoryReceipt{}, refused
			}
			var ok bool
			items, ok = root["items"].([]any)
			if !ok || len(items) == 0 && q.kind != "Orphan" {
				return runtimeInventoryReceipt{}, refused
			}
		}
		names := map[string]bool{}
		for _, item := range items {
			o, ok := item.(object)
			name, ns, uid := text(o, "metadata", "name"), text(o, "metadata", "namespace"), text(o, "metadata", "uid")
			key := ns + "/" + name
			// Native typed lists may omit each item's TypeMeta. The complete
			// typed list and fixed endpoint bind it; contradictory TypeMeta holds.
			if !ok || name == "" || len(name) > 253 || !kubeName.MatchString(name) || uid == "" || text(o, "metadata", "resourceVersion") == "" || names[key] || uids[uid] || q.namespace == "*" && !kubeNamespace.MatchString(ns) || q.namespace != "*" && ns != q.namespace || text(o, "apiVersion") != "" && text(o, "apiVersion") != q.api || text(o, "kind") != "" && text(o, "kind") != q.kind {
				return runtimeInventoryReceipt{}, refused
			}
			names[key], uids[uid] = true, true
			projection := runtimeInventoryProjection(q, o)
			records = append(records, projection)
			switch {
			case q.kind == "Node" && q.api == "v1":
				result.KubernetesNodes++
			case q.kind == "Node":
				result.Nodes++
			case q.kind == "Pod":
				result.Pods++
			case q.kind == "InstanceManager":
				result.InstanceManagers++
			case q.kind == "Volume":
				switch text(o, "spec", "dataEngine") {
				case "v1":
					result.V1Volumes++
				case "v2":
					result.V2Volumes++
				default:
					return runtimeInventoryReceipt{}, refused
				}
			case q.kind == "Setting":
				if text(o, "kind") != "Setting" || name != q.path[strings.LastIndex(q.path, "/")+1:] || text(o, "value") == "" || value(o, "metadata", "deletionTimestamp") != nil {
					return runtimeInventoryReceipt{}, refused
				}
				if name == "default-instance-manager-image" {
					result.Default = "other"
					switch text(o, "value") {
					case runtimeRetirementImage:
						result.Default = "reviewed"
					case "docker.io/longhornio/longhorn-instance-manager:v1.13.0", "longhornio/longhorn-instance-manager:v1.13.0":
						result.Default = "stock"
					}
				}
			}
		}
	}
	sort.Slice(records, func(i, j int) bool { return text(records[i], "identity") < text(records[j], "identity") })
	encoded, err := json.Marshal(records)
	if err != nil || result.Default == "" {
		return runtimeInventoryReceipt{}, refused
	}
	result.EpochSHA = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return result, nil
}

// Do not serialize Pod environments, annotations, event messages or Secret data.
// RVs and incidental status advance normally; configuration and process epochs
// do not. The opaque digest contains native UIDs and is not a health clearance.
func runtimeInventoryProjection(q runtimeInventoryQuery, o object) object {
	projection := object{
		"identity": q.api + "/" + q.kind + "/" + text(o, "metadata", "namespace") + "/" + text(o, "metadata", "name"),
		"uid":      text(o, "metadata", "uid"), "generation": value(o, "metadata", "generation"),
		"owners": value(o, "metadata", "ownerReferences"), "deleting": value(o, "metadata", "deletionTimestamp"),
	}
	switch q.kind {
	case "Pod":
		projection["spec"] = runtimePodProjection(value(o, "spec"))
	case "Deployment", "DaemonSet":
		projection["spec"] = object{"replicas": value(o, "spec", "replicas"), "selector": value(o, "spec", "selector"), "strategy": value(o, "spec", "strategy"), "updateStrategy": value(o, "spec", "updateStrategy"), "template": runtimePodProjection(value(o, "spec", "template", "spec"))}
	default:
		projection["spec"] = value(o, "spec")
	}
	for _, field := range []string{"phase", "currentState", "state", "robustness", "currentNodeID", "instanceManagerName", "replicaModeMap", "nodeInfo", "allocatable", "observedGeneration", "desiredNumberScheduled", "numberReady", "updatedNumberScheduled", "readyReplicas", "updatedReplicas"} {
		projection[field] = value(o, "status", field)
	}
	var processes []any
	for _, field := range []string{"containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses"} {
		statuses, _ := value(o, "status", field).([]any)
		for _, status := range statuses {
			s, _ := status.(object)
			processes = append(processes, object{"group": field, "name": text(s, "name"), "imageID": text(s, "imageID"), "containerID": text(s, "containerID"), "restarts": value(s, "restartCount"), "ready": value(s, "ready"), "started": value(s, "started"), "runningSince": value(s, "state", "running", "startedAt"), "terminatedAt": value(s, "state", "terminated", "finishedAt")})
		}
	}
	projection["processes"] = processes
	if q.kind == "Setting" {
		projection["value"] = value(o, "value")
	}
	return projection
}

func runtimePodProjection(raw any) object {
	spec, _ := raw.(object)
	result := object{"node": value(spec, "nodeName"), "resources": value(spec, "resources"), "overhead": value(spec, "overhead"), "nodeSelector": value(spec, "nodeSelector")}
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		var containers []any
		list, _ := value(spec, field).([]any)
		for _, raw := range list {
			c, _ := raw.(object)
			containers = append(containers, object{"name": text(c, "name"), "image": text(c, "image"), "resources": value(c, "resources"), "restartPolicy": value(c, "restartPolicy"), "mounts": value(c, "volumeMounts"), "devices": value(c, "volumeDevices")})
		}
		result[field] = containers
	}
	return result
}
