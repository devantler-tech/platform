package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// refreshCluster is a small stateful API: evictions and patches change what later reads see.
type refreshCluster struct {
	t              *testing.T
	node           object
	spare          object
	pods           []object
	events         []string
	refuseEviction bool
	loseRestart    bool
	returnToWorker bool
	evictions      []object
	evictionURLs   []string
	leaving        int
	loseCordonOnce bool
	stalePatches   int
	leavingPods    []object
	slowDeparture  bool
}

func movablePodOnWorker(worker, name, memory, owner string) object {
	pod := podOnWorker(worker, memory)
	metadata := pod["metadata"].(object)
	metadata["name"], metadata["uid"], metadata["namespace"] = name, name+"-uid", "apps"
	metadata["ownerReferences"] = []any{object{"kind": "ReplicaSet", "controller": true, "uid": owner}}
	pod["spec"].(object)["volumes"] = []any{object{"name": "token", "projected": object{}}}
	pod["status"].(object)["conditions"] = []any{object{"type": "Ready", "status": "True"}}
	return pod
}

func refreshWorker() object {
	n := nodeFixture()
	n["status"].(object)["nodeInfo"].(object)["bootID"] = "original-boot"
	n["status"].(object)["capacity"] = object{"hugepages-2Mi": "0"}
	n["status"].(object)["allocatable"] = object{"hugepages-2Mi": "0", "memory": "14980Mi"}
	return n
}

func spareWorker() object {
	spare := resource("spare-worker")
	spare["status"] = object{"allocatable": object{"hugepages-2Mi": "0", "memory": "14980Mi"}}
	return spare
}

func newRefreshCluster(t *testing.T, pods ...object) *refreshCluster {
	delay := refreshRetryDelay
	refreshRetryDelay = time.Millisecond
	t.Cleanup(func() { refreshRetryDelay = delay })
	return &refreshCluster{t: t, node: refreshWorker(), spare: spareWorker(), pods: pods}
}

func (f *refreshCluster) list(items ...object) ([]byte, error) {
	return json.Marshal(object{"items": items})
}

func (f *refreshCluster) evict(args []string, input []byte) ([]byte, error) {
	name := strings.Split(args[2], "/")[6]
	f.events = append(f.events, "evict "+name)
	if f.leaving > 0 {
		f.t.Fatal("a second Pod left before the previous workload was back")
	}
	var eviction object
	if err := json.Unmarshal(input, &eviction); err != nil {
		f.t.Fatal(err)
	}
	f.evictions = append(f.evictions, eviction)
	f.evictionURLs = append(f.evictionURLs, args[2])
	if f.refuseEviction {
		return nil, errors.New("external command failed")
	}
	for i, pod := range f.pods {
		if text(pod, "metadata", "name") != name {
			continue
		}
		worker := "spare-worker"
		if f.returnToWorker {
			worker = text(f.node, "metadata", "name")
		}
		owners := value(pod, "metadata", "ownerReferences").([]any)
		replacement := movablePodOnWorker(worker, name+"-new", "0", "")
		replacement["metadata"].(object)["ownerReferences"] = owners
		replacement["spec"].(object)["containers"] = value(pod, "spec", "containers")
		if f.leaving > 0 {
			continue
		}
		if f.slowDeparture {
			f.leavingPods = slices.Clone(f.pods)
			f.leaving = 1
		}
		f.pods[i] = replacement
	}
	return nil, nil
}

func (f *refreshCluster) patch(input []byte) ([]byte, error) {
	if f.stalePatches > 0 {
		f.stalePatches--
		return nil, errors.New("external command failed")
	}
	var changes []object
	if err := json.Unmarshal(input, &changes); err != nil {
		f.t.Fatal(err)
	}
	for _, change := range changes {
		switch {
		case text(change, "path") == "/spec/unschedulable" && value(change, "value") == true:
			f.events = append(f.events, "cordon")
			f.node["spec"].(object)["unschedulable"] = true
		case text(change, "path") == "/spec/unschedulable":
			f.events = append(f.events, "uncordon")
			delete(f.node["spec"].(object), "unschedulable")
		case strings.HasSuffix(text(change, "path"), "longhorn-v2-preparation") && text(change, "op") == "add":
			f.node["metadata"].(object)["annotations"].(object)[fenceAnnotation] = value(change, "value")
		case strings.HasSuffix(text(change, "path"), "longhorn-v2-preparation"):
			delete(f.node["metadata"].(object)["annotations"].(object), fenceAnnotation)
		}
	}
	if f.loseCordonOnce && value(f.node, "spec", "unschedulable") == true {
		f.loseCordonOnce = false
		return nil, errors.New("external command failed")
	}
	return json.Marshal(f.node)
}

func (f *refreshCluster) run(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
	switch program {
	case "git":
		return []byte(strings.Repeat("1", 40) + "\trefs/heads/main\n"), nil
	case "kubectl":
		call := args[2:]
		switch {
		case call[0] == "create":
			return f.evict(call, input)
		case call[0] == "patch":
			return f.patch(input)
		case call[0] == "get" && call[1] == "nodes":
			return f.list(f.node, f.spare)
		case call[0] == "get" && call[1] == "pods":
			if f.leaving > 0 {
				f.leaving--
				return f.list(f.leavingPods...)
			}
			return f.list(f.pods...)
		case call[0] == "get" && call[1] == "node":
			return json.Marshal(f.node)
		}
		f.t.Fatalf("unexpected kubectl call %v", call)
	case "talosctl":
		switch {
		case len(args) == 5 && args[2] == "systeminformation":
			return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": "machine-uuid"}})
		case args[1] == "read" && args[2] == "/proc/sys/vm/nr_hugepages":
			return []byte("1024\n"), nil
		case args[1] == "read":
			return []byte("nvme_tcp 1 0 - Live 0\nvfio_pci 1 0 - Live 0\nuio_pci_generic 1 0 - Live 0\n"), nil
		case args[1] == "service":
			f.events = append(f.events, "restart")
			if !f.loseRestart {
				f.node["status"].(object)["capacity"].(object)["hugepages-2Mi"] = "2Gi"
				f.node["status"].(object)["allocatable"].(object)["hugepages-2Mi"] = "2Gi"
				f.node["status"].(object)["allocatable"].(object)["memory"] = "12932Mi"
			}
			return nil, nil
		}
		f.t.Fatalf("unexpected talosctl call %v", args)
	}
	f.t.Fatalf("unexpected command %s", program)
	return nil, nil
}

func (f *refreshCluster) refresh(ctx context.Context) error {
	return refreshHugepages(ctx, client{run: f.run, context: "admin@prod"}, strings.Repeat("1", 40), f.node, f.pods)
}

func TestRefreshMovesOnlyWhatDoesNotFitThenRestoresScheduling(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	pinned := podOnWorker(worker, "12Gi")
	f := newRefreshCluster(t, pinned, movablePodOnWorker(worker, "small", "500Mi", "small-rs"), movablePodOnWorker(worker, "large", "1500Mi", "large-rs"))
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cordon", "evict large", "restart", "uncordon"}; !slices.Equal(f.events, want) {
		t.Fatalf("events %v, expected %v", f.events, want)
	}
	if value(f.node, "metadata", "annotations", fenceAnnotation) != nil {
		t.Fatal("the refresh left its ownership annotation behind")
	}
}

func TestRefreshHoldsBeforeTouchingAnythingWhenMovesCannotWork(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	big := func() object { return movablePodOnWorker(worker, "large", "2Gi", "large-rs") }
	withVolume := func(kind string) object {
		pod := big()
		pod["spec"].(object)["volumes"] = []any{object{"name": "data", kind: object{}}}
		return pod
	}
	inNamespace := func(namespace string) object {
		pod := big()
		pod["metadata"].(object)["namespace"] = namespace
		return pod
	}
	ownedBy := func(kind string) object {
		pod := big()
		pod["metadata"].(object)["ownerReferences"] = []any{object{"kind": kind, "controller": true, "uid": "owner"}}
		return pod
	}
	scheduled := func(field string, setting any) object {
		pod := big()
		pod["spec"].(object)[field] = setting
		return pod
	}
	for name, pod := range map[string]object{
		"persistent claim": withVolume("persistentVolumeClaim"),
		"scratch data":     withVolume("emptyDir"),
		"host path":        withVolume("hostPath"),
		"unknown volume":   withVolume("somethingNew"),
		"storage system":   inNamespace("longhorn-system"),
		"cluster system":   inNamespace("kube-system"),
		"stateful set":     ownedBy("StatefulSet"),
		"pinned to nodes":  scheduled("nodeSelector", object{"pool": "x"}),
		"node affinity":    scheduled("affinity", object{"nodeAffinity": object{"requiredDuringSchedulingIgnoredDuringExecution": object{}}}),
		"anti affinity":    scheduled("affinity", object{"podAntiAffinity": object{"requiredDuringSchedulingIgnoredDuringExecution": []any{}}}),
		"hard spread":      scheduled("topologySpreadConstraints", []any{object{"whenUnsatisfiable": "DoNotSchedule"}}),
		"daemon set":       ownedBy("DaemonSet"),
		"unowned":          podOnWorker(worker, "2Gi"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), pod)
			err := f.refresh(context.Background())
			if err == nil || !strings.Contains(err.Error(), "after huge pages") {
				t.Fatalf("refresh did not hold: %v", err)
			}
			if len(f.events) != 0 {
				t.Fatalf("a held plan still acted: %v", f.events)
			}
		})
	}
	t.Run("no room elsewhere", func(t *testing.T) {
		f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), big(), podOnWorker("spare-worker", "12500Mi"))
		if err := f.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "room") {
			t.Fatalf("refresh did not hold: %v", err)
		}
		if len(f.events) != 0 {
			t.Fatalf("a held plan still acted: %v", f.events)
		}
	})
	t.Run("the other worker is tainted", func(t *testing.T) {
		f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), big())
		f.spare["spec"] = object{"taints": []any{object{"key": "dedicated", "effect": "NoSchedule"}}}
		if err := f.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "room") {
			t.Fatalf("refresh did not hold: %v", err)
		}
	})
}

func TestRefreshReleasesItsCordonWhenAMoveDoesNotWorkOut(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	t.Run("eviction refused", func(t *testing.T) {
		f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
		f.refuseEviction = true
		if err := f.refresh(context.Background()); err == nil {
			t.Fatal("a refused eviction was accepted")
		}
		want := []string{"cordon"}
		for range refreshAttempts {
			want = append(want, "evict large")
		}
		if want = append(want, "uncordon"); !slices.Equal(f.events, want) {
			t.Fatalf("events %v, expected %v", f.events, want)
		}
	})
	t.Run("replacement came back", func(t *testing.T) {
		f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
		f.returnToWorker = true
		if err := f.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "after huge pages") {
			t.Fatalf("a Pod back on the worker was accepted: %v", err)
		}
		if want := []string{"cordon", "evict large", "uncordon"}; !slices.Equal(f.events, want) {
			t.Fatalf("events %v, expected %v", f.events, want)
		}
	})
}

func TestRefreshKeepsItsCordonAfterAnUncertainRestartAndAdoptsItLater(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
	f.loseRestart = true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := f.refresh(ctx); err == nil {
		t.Fatal("an unproven restart was accepted")
	}
	if want := []string{"cordon", "evict large", "restart"}; !slices.Equal(f.events, want) {
		t.Fatalf("events %v, expected %v", f.events, want)
	}
	if value(f.node, "spec", "unschedulable") != true {
		t.Fatal("the cordon was released before the smaller allocatable was proven")
	}
	f.events = nil
	f.node["status"].(object)["capacity"].(object)["hugepages-2Mi"] = "2Gi"
	f.node["status"].(object)["allocatable"].(object)["hugepages-2Mi"] = "2Gi"
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"uncordon"}; !slices.Equal(f.events, want) {
		t.Fatalf("a later run did not simply release its own cordon: %v", f.events)
	}
}

func TestRefreshNeverAdoptsAForeignCordon(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	for name, annotation := range map[string]any{"unowned": nil, "preparation": "longhorn-v2-prepare-" + worker, "another worker": refreshFencePrefix + "prod-worker-2"} {
		t.Run(name, func(t *testing.T) {
			f := newRefreshCluster(t, podOnWorker(worker, "1Gi"))
			f.node["spec"].(object)["unschedulable"] = true
			if annotation != nil {
				f.node["metadata"].(object)["annotations"].(object)[fenceAnnotation] = annotation
			}
			if err := f.refresh(context.Background()); err == nil {
				t.Fatal("a foreign cordon was accepted")
			}
			if len(f.events) != 0 {
				t.Fatalf("a foreign cordon was touched: %v", f.events)
			}
		})
	}
}

func TestMovesAreChosenLargestFirstAndStopAtEnough(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	n := refreshWorker()
	moves, err := planRefreshMoves(n, []object{podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "a", "600Mi", "a"),
		movablePodOnWorker(worker, "b", "900Mi", "b"), movablePodOnWorker(worker, "c", "700Mi", "c"), movablePodOnWorker("spare-worker", "d", "5Gi", "d")})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, pod := range moves {
		names = append(names, text(pod, "metadata", "name"))
	}
	if want := []string{"b", "c"}; !slices.Equal(names, want) {
		t.Fatalf("moves %v, expected %v", names, want)
	}
	if moves, err = planRefreshMoves(n, []object{podOnWorker(worker, "1Gi")}); err != nil || moves != nil {
		t.Fatalf("a fitting worker planned moves: %v %v", moves, err)
	}
}

func TestMigrationPoolWorkerNeverMovesPods(t *testing.T) {
	const pool = "autoscale-longhorn-v1-abc"
	f := newRefreshCluster(t, podOnWorker(pool, "12Gi"), movablePodOnWorker(pool, "large", "2Gi", "large-rs"))
	f.node["metadata"].(object)["name"] = pool
	if err := f.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "only permanent workers") {
		t.Fatalf("a pool worker did not hold: %v", err)
	}
	if len(f.events) != 0 {
		t.Fatalf("a pool worker was touched: %v", f.events)
	}
}

func TestMovesPreferAWorkloadWithAnotherReplica(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	single := movablePodOnWorker(worker, "single", "1500Mi", "single-rs")
	moves, err := planRefreshMoves(refreshWorker(), []object{podOnWorker(worker, "11Gi"), single,
		movablePodOnWorker(worker, "spare", "400Mi", "spare-rs"), movablePodOnWorker("spare-worker", "spare-peer", "400Mi", "spare-rs"),
		movablePodOnWorker(worker, "other", "1Gi", "other-rs"), movablePodOnWorker("spare-worker", "other-peer", "1Gi", "other-rs")})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, pod := range moves {
		names = append(names, text(pod, "metadata", "name"))
	}
	if want := []string{"other", "spare"}; !slices.Equal(names, want) {
		t.Fatalf("moves %v, expected %v", names, want)
	}
}

func TestEvictionIsTheOnlyWayAPodLeavesAndNamesThatExactPod(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/api/v1/namespaces/apps/pods/large/eviction"}; !slices.Equal(f.evictionURLs, want) {
		t.Fatalf("eviction URLs %v, expected %v", f.evictionURLs, want)
	}
	e := f.evictions[0]
	if text(e, "apiVersion") != "policy/v1" || text(e, "kind") != "Eviction" || text(e, "metadata", "name") != "large" ||
		text(e, "metadata", "namespace") != "apps" || text(e, "deleteOptions", "preconditions", "uid") != "large-uid" {
		t.Fatalf("eviction body does not bind the exact Pod through the budget path: %v", e)
	}
}

func TestNextPodWaitsForTheMovedOneToLeave(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "a", "1Gi", "a-rs"), movablePodOnWorker(worker, "b", "900Mi", "b-rs"))
	f.slowDeparture = true
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cordon", "evict a", "evict b", "restart", "uncordon"}; !slices.Equal(f.events, want) {
		t.Fatalf("events %v, expected %v", f.events, want)
	}
}

func TestStaleNodeWritesAreJudgedAgainFromFreshState(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	pods := func() []object {
		return []object{podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs")}
	}
	t.Run("a failed resource version test is retried", func(t *testing.T) {
		f := newRefreshCluster(t, pods()...)
		f.stalePatches = 2
		if err := f.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if want := []string{"cordon", "evict large", "restart", "uncordon"}; !slices.Equal(f.events, want) {
			t.Fatalf("events %v, expected %v", f.events, want)
		}
	})
	t.Run("a lost cordon response is not applied twice and is still released", func(t *testing.T) {
		f := newRefreshCluster(t, pods()...)
		f.loseCordonOnce = true
		if err := f.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if want := []string{"cordon", "evict large", "restart", "uncordon"}; !slices.Equal(f.events, want) {
			t.Fatalf("events %v, expected %v", f.events, want)
		}
		if value(f.node, "spec", "unschedulable") == true {
			t.Fatal("the cordon leaked")
		}
	})
}
