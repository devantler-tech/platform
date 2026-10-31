package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
)

const refreshFencePrefix = "longhorn-v2-refresh-"

// Only these volumes can be recreated elsewhere without losing anything.
var replaceableVolumes = map[string]bool{"name": true, "projected": true, "configMap": true, "secret": true, "downwardAPI": true}

// Moving a Pod here must never move data or the cluster's own machinery, so a Pod
// qualifies only by what it positively is, and anything else holds the refresh.
var refreshPinnedNamespaces = map[string]bool{"kube-system": true, "longhorn-system": true, "flux-system": true}

func replicaSetOwner(pod object) string {
	owners, _ := value(pod, "metadata", "ownerReferences").([]any)
	if len(owners) != 1 {
		return ""
	}
	owner, _ := owners[0].(map[string]any)
	if text(owner, "kind") != "ReplicaSet" || value(owner, "controller") != true {
		return ""
	}
	return text(owner, "uid")
}

func movablePod(pod object) bool {
	if refreshPinnedNamespaces[text(pod, "metadata", "namespace")] || replicaSetOwner(pod) == "" ||
		text(pod, "status", "phase") != "Running" || value(pod, "metadata", "deletionTimestamp") != nil {
		return false
	}
	volumes, _ := value(pod, "spec", "volumes").([]any)
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		for kind := range volume {
			if !replaceableVolumes[kind] {
				return false
			}
		}
	}
	return true
}

// planRefreshMoves picks the fewest movable Pods, largest first, that make the worker fit.
func planRefreshMoves(n object, pods []object) ([]object, error) {
	excess, err := refreshExcess(n, pods)
	if err != nil || excess <= 0 {
		return nil, err
	}
	type candidate struct {
		pod     object
		request float64
	}
	var candidates []candidate
	for _, pod := range pods {
		if text(pod, "spec", "nodeName") != text(n, "metadata", "name") || !movablePod(pod) {
			continue
		}
		request, requestErr := podMemoryRequest(pod)
		if requestErr != nil {
			return nil, requestErr
		}
		candidates = append(candidates, candidate{pod, request})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].request != candidates[j].request {
			return candidates[i].request > candidates[j].request
		}
		return text(candidates[i].pod, "metadata", "uid") < text(candidates[j].pod, "metadata", "uid")
	})
	var moves []object
	for _, c := range candidates {
		if excess <= 0 {
			break
		}
		if c.request > 0 {
			moves = append(moves, c.pod)
			excess -= c.request
		}
	}
	if excess > 0 {
		return nil, errors.New("worker " + text(n, "metadata", "name") + " requests more memory than remains after huge pages, and its stateless workloads cannot cover the difference")
	}
	return moves, nil
}

// verifyMovesHaveRoom proves every moved Pod's request fits on another schedulable
// worker once huge pages are counted there, before anything is cordoned or evicted.
func verifyMovesHaveRoom(target string, moves []object, nodes, pods []object) error {
	free := map[string]float64{}
	for _, n := range nodes {
		name := text(n, "metadata", "name")
		taints, _ := value(n, "spec", "taints").([]any)
		if name == target || value(n, "spec", "unschedulable") == true || len(taints) > 0 {
			continue
		}
		excess, err := refreshExcess(n, pods)
		if err != nil {
			return err
		}
		free[name] = -excess
	}
	for _, pod := range moves {
		request, err := podMemoryRequest(pod)
		if err != nil {
			return err
		}
		best := ""
		for name, room := range free {
			if room >= request && (best == "" || room > free[best] || room == free[best] && name < best) {
				best = name
			}
		}
		if best == "" {
			return errors.New("no other schedulable worker has room for the workloads this refresh would move")
		}
		free[best] -= request
	}
	return nil
}

func (c client) patchNode(ctx context.Context, name string, changes func(object) ([]object, error)) error {
	var err error
	for range 5 {
		var n object
		if n, err = c.get(ctx, "node", "", name, false); err != nil {
			return err
		}
		var patch []object
		if patch, err = changes(n); err != nil || patch == nil {
			return err
		}
		if err = c.patch(ctx, "node", "", n, patch...); !errors.Is(err, errConflict) {
			return err
		}
	}
	return err
}

func (c client) cordonForRefresh(ctx context.Context, name string) error {
	return c.patchNode(ctx, name, func(n object) ([]object, error) {
		if value(n, "spec", "unschedulable") == true || value(n, "metadata", "annotations", fenceAnnotation) != nil {
			return nil, errors.New("worker cordon appeared during the kubelet refresh")
		}
		return append(annotationPatch(n, refreshFencePrefix+name), object{"op": "add", "path": "/spec/unschedulable", "value": true}), nil
	})
}

// releaseRefreshCordon removes only the cordon this procedure placed.
func (c client) releaseRefreshCordon(ctx context.Context, name string) error {
	return c.patchNode(ctx, name, func(n object) ([]object, error) {
		if text(n, "metadata", "annotations", fenceAnnotation) != refreshFencePrefix+name {
			return nil, errors.New("refusing to release a cordon this refresh does not own")
		}
		return []object{{"op": "add", "path": "/spec/unschedulable", "value": false},
			{"op": "remove", "path": "/metadata/annotations/storage.platform.devantler.tech~1longhorn-v2-preparation"}}, nil
	})
}

func readyPodsOf(owner string, pods []object) int {
	count := 0
	for _, pod := range pods {
		status, _ := value(pod, "status").(map[string]any)
		if replicaSetOwner(pod) == owner && text(pod, "status", "phase") == "Running" && value(pod, "metadata", "deletionTimestamp") == nil && ready(status) {
			count++
		}
	}
	return count
}

// movePod asks for an eviction, so disruption budgets decide, and waits until the
// workload is back at its Ready count before the next Pod may leave.
func (c client) movePod(ctx context.Context, pod object, pods []object) error {
	namespace, name, uid := text(pod, "metadata", "namespace"), text(pod, "metadata", "name"), text(pod, "metadata", "uid")
	if err := safeKubeRequest("pods", namespace, name, false); err != nil || namespace == "" {
		return errors.New("a moved Pod's identity cannot select another request scope")
	}
	eviction, err := json.Marshal(object{"apiVersion": "policy/v1", "kind": "Eviction", "metadata": object{"name": name, "namespace": namespace},
		"deleteOptions": object{"preconditions": object{"uid": uid}}})
	if err != nil {
		return err
	}
	owner := replicaSetOwner(pod)
	wanted := readyPodsOf(owner, pods)
	if _, err = c.kubectl(ctx, eviction, "create", "--raw", "/api/v1/namespaces/"+namespace+"/pods/"+name+"/eviction", "-f", "-"); err != nil {
		return errors.New("moving " + namespace + "/" + name + " was refused, so no kubelet restart was requested")
	}
	return waitReady(ctx, func() error {
		live, listErr := c.list(ctx, "pods", namespace)
		if listErr != nil {
			return listErr
		}
		for _, other := range live {
			if text(other, "metadata", "uid") == uid {
				return errors.New("the moved Pod has not left yet")
			}
		}
		if readyPodsOf(owner, live) < wanted {
			return errors.New("the moved workload is not back at its Ready count")
		}
		return nil
	})
}

// moveForRefresh makes the worker fit and returns with its cordon held. Scheduling must
// stay closed until kubelet reports the smaller allocatable, or evicted Pods can return.
func (c client) moveForRefresh(ctx context.Context, n object, moves, pods []object, held *bool) error {
	name := text(n, "metadata", "name")
	nodes, err := c.list(ctx, "nodes", "")
	if err != nil {
		return err
	}
	if err = verifyMovesHaveRoom(name, moves, nodes, pods); err != nil {
		return err
	}
	if !*held {
		if err = c.cordonForRefresh(ctx, name); err != nil {
			return err
		}
		*held = true
	}
	for _, pod := range moves {
		if err = c.movePod(ctx, pod, pods); err != nil {
			return err
		}
	}
	live, err := c.list(ctx, "pods", "*")
	if err != nil {
		return err
	}
	// A replacement that tolerates the cordon can land back here, so measure again.
	return verifyRefreshFits(n, live)
}
