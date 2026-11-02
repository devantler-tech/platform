package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

const (
	refreshFencePrefix = "longhorn-v2-refresh-"
	refreshAttempts    = 6
)

// refreshRetryDelay spaces attempts at a request the API server may refuse for a moment.
var refreshRetryDelay = 10 * time.Second

// Only permanent workers may be cordoned here: the migration-pool roster check refuses
// any cordoned pool node, so a cordon left on one could not be adopted or released.
var refreshMovableWorker = regexp.MustCompile(`^prod-worker-[123]$`)

// Only these volumes can be recreated elsewhere without losing anything.
var replaceableVolumes = map[string]bool{"name": true, "projected": true, "configMap": true, "secret": true, "downwardAPI": true}

// Pods in these namespaces never move. Elsewhere a Pod qualifies only by what it
// positively is, as movablePod decides.
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
	if !schedulesFreely(pod) {
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

// schedulesFreely refuses the constraints most likely to leave a replacement Pending. It is not
// a scheduler: a Pod moves only while another replica serves, so a miss costs capacity, not service.
func schedulesFreely(pod object) bool {
	selector, _ := value(pod, "spec", "nodeSelector").(map[string]any)
	for key := range selector {
		if key != "kubernetes.io/os" && key != "kubernetes.io/arch" {
			return false
		}
	}
	affinity, _ := value(pod, "spec", "affinity").(map[string]any)
	for _, raw := range affinity {
		kind, _ := raw.(map[string]any)
		if kind["requiredDuringSchedulingIgnoredDuringExecution"] != nil {
			return false
		}
	}
	constraints, _ := value(pod, "spec", "topologySpreadConstraints").([]any)
	for _, raw := range constraints {
		if constraint, _ := raw.(map[string]any); text(constraint, "whenUnsatisfiable") != "ScheduleAnyway" {
			return false
		}
	}
	return true
}

// restartableScanner names the one single-replica workload that may move. kubevuln serves no
// traffic and its next scheduled scan repeats the work, so even a replacement that stays Pending
// costs a delayed scan and no outage. Its scratch volume holds only a rebuildable cache.
func restartableScanner(pod object) bool {
	if text(pod, "metadata", "namespace") != "kubescape" || text(pod, "metadata", "labels", "app") != "kubevuln" ||
		replicaSetOwner(pod) == "" || text(pod, "status", "phase") != "Running" || value(pod, "metadata", "deletionTimestamp") != nil {
		return false
	}
	volumes, _ := value(pod, "spec", "volumes").([]any)
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		for kind := range volume {
			if !replaceableVolumes[kind] && kind != "emptyDir" {
				return false
			}
		}
	}
	return true
}

// planRefreshMoves picks the fewest Pods, largest first, that make the worker fit. A Pod with
// another Ready replica qualifies, so its workload keeps serving whatever happens next. Only
// when those are not enough does the restartable scanner move too.
func planRefreshMoves(n object, pods []object) ([]object, error) {
	excess, err := refreshExcess(n, pods)
	if err != nil || excess <= 0 {
		return nil, err
	}
	if !refreshMovableWorker.MatchString(text(n, "metadata", "name")) {
		return nil, errors.New("worker " + text(n, "metadata", "name") + " requests more memory than remains after huge pages, and only permanent workers may move Pods for a refresh")
	}
	type candidate struct {
		pod     object
		request float64
	}
	var candidates []candidate
	for _, pod := range pods {
		if text(pod, "spec", "nodeName") != text(n, "metadata", "name") || !movablePod(pod) || readyPodsOf(replicaSetOwner(pod), pods) < 2 {
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
	for _, pod := range pods {
		if excess <= 0 {
			break
		}
		if text(pod, "spec", "nodeName") != text(n, "metadata", "name") || !restartableScanner(pod) || readyPodsOf(replicaSetOwner(pod), pods) >= 2 {
			continue
		}
		request, requestErr := podMemoryRequest(pod)
		if requestErr != nil {
			return nil, requestErr
		}
		if request > 0 {
			moves = append(moves, pod)
			excess -= request
		}
	}
	if excess > 0 {
		return nil, errors.New("worker " + text(n, "metadata", "name") + " requests more memory than remains after huge pages, and the workloads allowed to move cannot cover the difference")
	}
	return moves, nil
}

// verifyMovesHaveRoom checks every moved Pod's memory request fits on another schedulable
// worker once huge pages are counted there, before anything is cordoned or evicted. It does
// not predict the scheduler: a replacement that stays Pending is caught by movePod's wait.
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

func refreshPause(ctx context.Context) error {
	timer := time.NewTimer(refreshRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// patchNode re-reads the Node before every attempt. A stale resource version fails its JSON
// patch test as an ordinary refusal, and a lost response leaves the patch applied, so any
// failed attempt is judged again from fresh state. changes returns nil once nothing is left.
func (c client) patchNode(ctx context.Context, name string, changes func(object) ([]object, error)) error {
	var err error
	for attempt := range refreshAttempts {
		if attempt > 0 {
			if pause := refreshPause(ctx); pause != nil {
				return errors.Join(err, pause)
			}
		}
		var n object
		if n, err = c.get(ctx, "node", "", name, false); err != nil {
			continue
		}
		patch, refusal := changes(n)
		if refusal != nil || patch == nil {
			return refusal
		}
		if err = c.patch(ctx, "node", "", n, patch...); err == nil {
			return nil
		}
	}
	return err
}

func (c client) cordonForRefresh(ctx context.Context, name string) error {
	return c.patchNode(ctx, name, func(n object) ([]object, error) {
		fence := value(n, "metadata", "annotations", fenceAnnotation)
		if value(n, "spec", "unschedulable") == true && fence == refreshFencePrefix+name {
			return nil, nil
		}
		if value(n, "spec", "unschedulable") == true || fence != nil {
			return nil, errors.New("worker cordon appeared during the kubelet refresh")
		}
		return append(annotationPatch(n, refreshFencePrefix+name), object{"op": "add", "path": "/spec/unschedulable", "value": true}), nil
	})
}

// releaseRefreshCordon removes only the cordon this procedure placed.
func (c client) releaseRefreshCordon(ctx context.Context, name string) error {
	return c.patchNode(ctx, name, func(n object) ([]object, error) {
		fence := value(n, "metadata", "annotations", fenceAnnotation)
		if fence == nil && value(n, "spec", "unschedulable") != true {
			return nil, nil
		}
		if fence != refreshFencePrefix+name {
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

func podListed(pods []object, uid string) bool {
	for _, pod := range pods {
		if text(pod, "metadata", "uid") == uid {
			return true
		}
	}
	return false
}

// podLeaving is true once the Pod is gone or terminating, which is what an accepted
// eviction leaves behind.
func podLeaving(pods []object, uid string) bool {
	for _, pod := range pods {
		if text(pod, "metadata", "uid") == uid {
			return value(pod, "metadata", "deletionTimestamp") != nil
		}
	}
	return true
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
	// The plan is minutes old by now. Only the scanner may leave without another Ready replica.
	keepsServing := func(live []object) error {
		if !restartableScanner(pod) && readyPodsOf(owner, live) < 2 {
			return errors.New(namespace + "/" + name + " no longer has another Ready replica, so it stays")
		}
		return nil
	}
	if err = keepsServing(pods); err != nil {
		return err
	}
	wanted := readyPodsOf(owner, pods)
	// A budget refuses with 429 meaning retry, and its status can lag a replacement turning Ready.
	for attempt := range refreshAttempts {
		if attempt > 0 {
			if err = refreshPause(ctx); err != nil {
				return err
			}
			// A lost response can hide a success, and a retry would then only report the Pod missing.
			live, listErr := c.list(ctx, "pods", namespace)
			if listErr != nil {
				// Never evict on an unread state: this attempt only retries the read.
				err = listErr
				continue
			}
			if podLeaving(live, uid) {
				err = nil
				break
			}
			if err = keepsServing(live); err != nil {
				return err
			}
		}
		if _, err = c.kubectl(ctx, eviction, "create", "--raw", "/api/v1/namespaces/"+namespace+"/pods/"+name+"/eviction", "-f", "-"); err == nil {
			break
		}
	}
	if err != nil {
		return errors.New("moving " + namespace + "/" + name + " was refused, so no kubelet restart was requested")
	}
	return waitReady(ctx, func(ctx context.Context) error {
		live, listErr := c.list(ctx, "pods", namespace)
		if listErr != nil {
			return listErr
		}
		if podListed(live, uid) {
			return errors.New("the moved Pod has not left yet")
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
	// Held before the request: a lost response may still have placed the cordon.
	*held = true
	if err = c.cordonForRefresh(ctx, name); err != nil {
		return err
	}
	for _, pod := range moves {
		live, listErr := c.list(ctx, "pods", "*")
		if listErr != nil {
			return listErr
		}
		if err = c.movePod(ctx, pod, live); err != nil {
			return err
		}
	}
	fmt.Printf("Moved %d Pods off %s before its kubelet refresh.\n", len(moves), name)
	live, err := c.list(ctx, "pods", "*")
	if err != nil {
		return err
	}
	// A replacement that tolerates the cordon can land back here, so measure again.
	return verifyRefreshFits(n, live)
}
