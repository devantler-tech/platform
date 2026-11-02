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
	loseEviction   bool
	loseUncordon   bool
	onEvict        func()
	onRestart      func()
	onPodsRead     func()
	failPodReads   int
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

func withPeers(pods []object) []object {
	all := slices.Clone(pods)
	for _, pod := range pods {
		if owner := replicaSetOwner(pod); owner != "" && !strings.HasSuffix(owner, "-single") {
			all = append(all, movablePodOnWorker("spare-worker", text(pod, "metadata", "name")+"-peer", "100Mi", owner))
		}
	}
	return all
}

func newRefreshCluster(t *testing.T, pods ...object) *refreshCluster {
	delay, poll, settle := refreshRetryDelay, readinessPoll, refreshSettle
	refreshRetryDelay, readinessPoll, refreshSettle = time.Millisecond, time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { refreshRetryDelay, readinessPoll, refreshSettle = delay, poll, settle })
	return &refreshCluster{t: t, node: refreshWorker(), spare: spareWorker(), pods: withPeers(pods)}
}

func (f *refreshCluster) list(items ...object) ([]byte, error) {
	return json.Marshal(object{"items": items})
}

func (f *refreshCluster) evict(args []string, input []byte) ([]byte, error) {
	name := strings.Split(args[2], "/")[6]
	f.events = append(f.events, "evict "+name)
	if f.onEvict != nil {
		f.onEvict()
	}
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
		if f.slowDeparture {
			f.leavingPods = slices.Clone(f.pods)
			f.leaving = 3
		}
		f.pods[i] = replacement
	}
	if f.loseEviction {
		f.loseEviction = false
		return nil, errors.New("external command failed")
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
	if f.loseUncordon && value(f.node, "spec", "unschedulable") != true {
		f.loseUncordon = false
		return nil, errors.New("external command failed")
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
			if f.onPodsRead != nil {
				f.onPodsRead()
			}
			if f.failPodReads > 0 {
				f.failPodReads--
				return nil, errors.New("external command failed")
			}
			switch f.leaving {
			case 3:
				f.leaving--
				return f.list(f.leavingPods...)
			case 2:
				f.leaving--
				pending := slices.Clone(f.pods)
				for i, pod := range pending {
					if strings.HasSuffix(text(pod, "metadata", "name"), "-new") {
						clone := movablePodOnWorker("spare-worker", text(pod, "metadata", "name"), "0", "pending")
						pending[i] = clone
					}
				}
				return f.list(pending...)
			case 1:
				f.leaving--
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
			if f.onRestart != nil {
				f.onRestart()
			}
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
	moves, err := planRefreshMoves(n, withPeers([]object{podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "a", "600Mi", "a"),
		movablePodOnWorker(worker, "b", "900Mi", "b"), movablePodOnWorker(worker, "c", "700Mi", "c"), movablePodOnWorker("spare-worker", "d", "5Gi", "d")}))
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

func TestASingleReplicaNeverMoves(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "only", "2Gi", "only-single"))
	if err := f.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "after huge pages") {
		t.Fatalf("a single replica was planned to move: %v", err)
	}
	if len(f.events) != 0 {
		t.Fatalf("a held plan still acted: %v", f.events)
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

func TestALostReleaseResponseIsNotAFalseRefusal(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
	f.loseUncordon = true
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestALostEvictionResponseStillCountsAsMoved(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
	f.loseEviction = true
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cordon", "evict large", "restart", "uncordon"}; !slices.Equal(f.events, want) {
		t.Fatalf("events %v, expected %v", f.events, want)
	}
}

func scannerOnWorker(worker, memory string) object {
	pod := movablePodOnWorker(worker, "kubevuln-abc", memory, "kubevuln-single")
	metadata := pod["metadata"].(object)
	metadata["namespace"], metadata["labels"] = "kubescape", object{"app": "kubevuln"}
	pod["spec"].(object)["volumes"] = []any{object{"name": "token", "projected": object{}}, object{"name": "cache", "emptyDir": object{}}}
	return pod
}

func moveNames(moves []object) []string {
	var names []string
	for _, pod := range moves {
		names = append(names, text(pod, "metadata", "name"))
	}
	return names
}

func TestTheScannerMovesOnlyWhenReplicatedPodsAreNotEnough(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	base := podOnWorker(worker, "12Gi")
	// The scanner is larger than each replicated Pod, so largest-first alone would pick it.
	moves, err := planRefreshMoves(refreshWorker(), withPeers([]object{base, scannerOnWorker(worker, "640Mi"),
		movablePodOnWorker(worker, "a", "400Mi", "a"), movablePodOnWorker(worker, "b", "400Mi", "b")}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b"}; !slices.Equal(moveNames(moves), want) {
		t.Fatalf("moves %v, expected %v", moveNames(moves), want)
	}
	moves, err = planRefreshMoves(refreshWorker(), withPeers([]object{base, scannerOnWorker(worker, "1Gi"), movablePodOnWorker(worker, "a", "300Mi", "a")}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "kubevuln-abc"}; !slices.Equal(moveNames(moves), want) {
		t.Fatalf("moves %v, expected %v", moveNames(moves), want)
	}
}

func TestRefreshMovesTheScannerAndRestoresScheduling(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), scannerOnWorker(worker, "1Gi"))
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.evictions) != 1 || value(f.node, "spec", "unschedulable") == true {
		t.Fatalf("evictions %v, node spec %v", f.evictions, f.node["spec"])
	}
}

func TestOnlyTheNamedScannerMovesWithoutAPeer(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	otherNamespace := scannerOnWorker(worker, "2Gi")
	otherNamespace["metadata"].(object)["namespace"] = "apps"
	otherApp := scannerOnWorker(worker, "2Gi")
	otherApp["metadata"].(object)["labels"] = object{"app": "storage"}
	withClaim := scannerOnWorker(worker, "2Gi")
	withClaim["spec"].(object)["volumes"] = []any{object{"name": "data", "persistentVolumeClaim": object{}}}
	statefulSet := scannerOnWorker(worker, "2Gi")
	statefulSet["metadata"].(object)["ownerReferences"] = []any{object{"kind": "StatefulSet", "controller": true, "uid": "kubevuln-single"}}
	leaving := scannerOnWorker(worker, "2Gi")
	leaving["metadata"].(object)["deletionTimestamp"] = "2026-10-31T00:00:00Z"
	for name, pod := range map[string]object{"namespace": otherNamespace, "app": otherApp, "claim": withClaim, "owner": statefulSet, "leaving": leaving} {
		if moves, err := planRefreshMoves(refreshWorker(), []object{podOnWorker(worker, "12Gi"), pod}); err == nil {
			t.Fatalf("%s: planned %v", name, moveNames(moves))
		}
	}
}

func TestAMoveHoldsWhenItsPeerStoppedBeingReady(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "a", "1Gi", "a-rs"), movablePodOnWorker(worker, "b", "900Mi", "b-rs"))
	f.onEvict = func() {
		f.pods = slices.DeleteFunc(f.pods, func(pod object) bool { return text(pod, "metadata", "name") == "b-peer" })
	}
	if err := f.refresh(context.Background()); err == nil {
		t.Fatal("a Pod left although its workload no longer had another Ready replica")
	}
	if want := []string{"cordon", "evict a", "uncordon"}; !slices.Equal(f.events, want) {
		t.Fatalf("events %v, expected %v", f.events, want)
	}
}

func TestRefreshHoldsUnderPressureBeforeAnyPodMoves(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
	f.node["status"].(object)["conditions"] = []any{object{"type": "Ready", "status": "True"}, object{"type": "MemoryPressure", "status": "True"}}
	if err := f.refresh(context.Background()); err == nil {
		t.Fatal("a refresh started under memory pressure")
	}
	if len(f.events) != 0 {
		t.Fatalf("a held refresh acted: %v", f.events)
	}
}

func TestAPodThatLandedAfterMeasuringHoldsTheRestart(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "1Gi"))
	measured := slices.Clone(f.pods)
	f.pods = append(f.pods, podOnWorker(worker, "14Gi"))
	if err := refreshHugepages(context.Background(), client{run: f.run, context: "admin@prod"}, strings.Repeat("1", 40), f.node, measured); err == nil {
		t.Fatal("a restart went ahead on a stale measurement")
	}
	if len(f.events) != 0 {
		t.Fatalf("a held refresh acted: %v", f.events)
	}
}

func TestAPodRejectedByTheRestartFailsTheRunAndKeepsTheCordon(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
	f.onRestart = func() {
		f.pods[0]["status"].(object)["phase"] = "Failed"
		f.pods[0]["status"].(object)["reason"] = "OutOfmemory"
	}
	if err := f.refresh(context.Background()); err == nil {
		t.Fatal("a rejected Pod was reported as success")
	}
	if value(f.node, "spec", "unschedulable") != true || slices.Contains(f.events, "uncordon") {
		t.Fatalf("the cordon was released after a rejection: %v", f.events)
	}
}

func TestAProvenCordonIsReleasedAheadOfOtherGates(t *testing.T) {
	cordoned := func(annotation, pages string) (*refreshCluster, client) {
		f := newRefreshCluster(t, podOnWorker(text(refreshWorker(), "metadata", "name"), "1Gi"))
		f.node["spec"].(object)["unschedulable"] = true
		f.node["metadata"].(object)["annotations"] = object{fenceAnnotation: annotation}
		f.node["status"].(object)["capacity"].(object)["hugepages-2Mi"] = pages
		f.node["status"].(object)["allocatable"].(object)["hugepages-2Mi"] = pages
		return f, client{run: f.run, context: "admin@prod"}
	}
	f, c := cordoned(refreshFencePrefix+"prod-worker-1", "2Gi")
	if err := c.releaseProvenRefreshCordons(context.Background(), []object{f.node}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.events, []string{"uncordon"}) {
		t.Fatalf("a proven cordon stayed: %v", f.events)
	}
	for name, values := range map[string][2]string{"capacity unproven": {refreshFencePrefix + "prod-worker-1", "0"}, "foreign cordon": {"someone-else", "2Gi"}} {
		f, c = cordoned(values[0], values[1])
		if err := c.releaseProvenRefreshCordons(context.Background(), []object{f.node}); err != nil {
			t.Fatal(err)
		}
		if len(f.events) != 0 {
			t.Fatalf("%s: released: %v", name, f.events)
		}
	}
}

func TestARejectionAfterCapacityIsProvenStillFailsTheRun(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
	reads := -1
	f.onRestart = func() { reads = 0 }
	f.onPodsRead = func() {
		if reads >= 0 {
			if reads++; reads == 3 {
				f.pods[0]["status"].(object)["phase"] = "Failed"
				f.pods[0]["status"].(object)["reason"] = "OutOfmemory"
			}
		}
	}
	refreshSettle = 2 * time.Second
	if err := f.refresh(context.Background()); err == nil {
		t.Fatal("a rejection during the settling window was reported as success")
	}
	if slices.Contains(f.events, "uncordon") {
		t.Fatalf("the cordon was released after a rejection: %v", f.events)
	}
}

func TestAnUnreadRetryNeverEvicts(t *testing.T) {
	worker := text(refreshWorker(), "metadata", "name")
	f := newRefreshCluster(t, podOnWorker(worker, "12Gi"), movablePodOnWorker(worker, "large", "2Gi", "large-rs"))
	pod := f.pods[1]
	f.refuseEviction = true
	first := true
	f.onEvict = func() {
		if first {
			first = false
			f.failPodReads = 1000
		}
	}
	err := client{run: f.run, context: "admin@prod"}.movePod(context.Background(), pod, f.pods)
	if err == nil {
		t.Fatal("a refused eviction was reported as moved")
	}
	if len(f.evictions) != 1 {
		t.Fatalf("evicted %d times with no fresh read of the workload", len(f.evictions))
	}
}

func TestATerminatingPodCountsAsMovedAfterALostResponse(t *testing.T) {
	gone := object{"metadata": object{"uid": "a", "deletionTimestamp": "2026-11-01T00:00:00Z"}}
	if !podLeaving([]object{gone}, "a") || !podLeaving(nil, "a") {
		t.Fatal("a terminating or missing Pod was not treated as leaving")
	}
	if podLeaving([]object{{"metadata": object{"uid": "a"}}}, "a") {
		t.Fatal("a running Pod was treated as leaving")
	}
}
