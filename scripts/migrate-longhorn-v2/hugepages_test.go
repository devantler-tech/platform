package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestHugepageRefreshRestartsOnlyKubeletAndRequiresPositiveReadback(t *testing.T) {
	settle := refreshSettle
	refreshSettle = 0
	t.Cleanup(func() { refreshSettle = settle })
	for _, alreadyReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "restart needed", true: "already ready"}[alreadyReady], func(t *testing.T) {
			n := nodeFixture()
			n["status"].(object)["nodeInfo"].(object)["bootID"] = "original-boot"
			n["status"].(object)["capacity"] = object{"hugepages-2Mi": "0"}
			n["status"].(object)["allocatable"] = object{"hugepages-2Mi": "0", "memory": "14980Mi"}
			setReady := func() {
				n["status"].(object)["capacity"].(object)["hugepages-2Mi"] = "2Gi"
				n["status"].(object)["allocatable"].(object)["hugepages-2Mi"] = "2Gi"
			}
			if alreadyReady {
				setReady()
			}
			restarts := 0
			run := func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
				switch program {
				case "git":
					return []byte(strings.Repeat("1", 40) + "\trefs/heads/main\n"), nil
				case "kubectl":
					if slices.Contains(args, "pods") {
						return json.Marshal(object{"items": []object{podOnWorker(text(n, "metadata", "name"), "1Gi")}})
					}
					return json.Marshal(n)
				case "talosctl":
					if len(args) == 5 && args[2] == "systeminformation" {
						return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": "machine-uuid"}})
					}
					if reflect.DeepEqual(args, []string{"--nodes=10.0.99.4", "read", "/proc/sys/vm/nr_hugepages"}) {
						return []byte("1024\n"), nil
					}
					if reflect.DeepEqual(args, []string{"--nodes=10.0.99.4", "read", "/proc/modules"}) {
						return []byte("nvme_tcp 1 0 - Live 0\nvfio_pci 1 0 - Live 0\nuio_pci_generic 1 0 - Live 0\n"), nil
					}
					if !reflect.DeepEqual(args, []string{"--nodes=10.0.99.4", "service", "kubelet", "restart"}) {
						t.Fatalf("refresh exceeded kubelet-only scope: %v", args)
					}
					restarts++
					setReady()
					return nil, errors.New("response lost after acceptance")
				default:
					t.Fatalf("unexpected command %s", program)
				}
				return nil, nil
			}
			if err := refreshHugepages(context.Background(), client{run: run, context: "admin@prod"}, strings.Repeat("1", 40), n, []object{podOnWorker(text(n, "metadata", "name"), "1Gi")}); err != nil {
				t.Fatal(err)
			}
			want := 1
			if alreadyReady {
				want = 0
			}
			if restarts != want {
				t.Fatalf("restart count %d, expected %d", restarts, want)
			}
		})
	}
}

func TestHugepageReadbackRejectsPartialOrUnreadyCapacity(t *testing.T) {
	for _, raw := range []string{"0", "1Gi", "4Gi", "", "2048"} {
		n := nodeFixture()
		n["status"].(object)["capacity"] = object{"hugepages-2Mi": raw}
		n["status"].(object)["allocatable"] = object{"hugepages-2Mi": "2Gi"}
		if hugepagesReady(n) {
			t.Fatalf("invalid huge-page capacity %q accepted", raw)
		}
	}
	for _, kind := range []string{"DiskPressure", "MemoryPressure", "PIDPressure"} {
		n := nodeFixture()
		for _, raw := range value(n, "status", "conditions").([]any) {
			condition := raw.(object)
			if text(condition, "type") == kind {
				condition["status"] = "True"
			}
		}
		if hugepagesReady(n) {
			t.Fatalf("worker with %s was accepted as prepared", kind)
		}
	}
}

func podOnWorker(worker, memory string) object {
	pod := resource("pod-" + worker + "-" + memory)
	pod["spec"] = object{"nodeName": worker, "containers": []any{object{"resources": object{"requests": object{"memory": memory}}}}}
	pod["status"] = object{"phase": "Running"}
	return pod
}

func TestKubeletRefreshHoldsWhenRequestsWouldNotFitAfterHugepages(t *testing.T) {
	n := nodeFixture()
	name := text(n, "metadata", "name")
	n["status"].(object)["allocatable"] = object{"memory": "14980Mi", "hugepages-2Mi": "0"}
	// 14456 Mi fits today's allocatable and was the live request on a worker, but not 12932 Mi.
	if err := verifyRefreshFits(n, []object{podOnWorker(name, "14456Mi")}); err == nil {
		t.Fatal("a restart that would reject running Pods was allowed")
	}
	if err := verifyRefreshFits(n, []object{podOnWorker(name, "12932Mi")}); err != nil {
		t.Fatalf("requests that fit exactly were refused: %v", err)
	}
	if err := verifyRefreshFits(n, nil); err == nil {
		t.Fatal("an empty Pod inventory was accepted as proof")
	}
	if err := verifyRefreshFits(n, []object{podOnWorker(name, "12Gi"), podOnWorker("another-worker", "12Gi")}); err != nil {
		t.Fatalf("another worker's Pod was counted: %v", err)
	}
	finished := podOnWorker(name, "12Gi")
	finished["status"] = object{"phase": "Succeeded"}
	if err := verifyRefreshFits(n, []object{podOnWorker(name, "12Gi"), finished}); err != nil {
		t.Fatalf("a finished Pod was counted: %v", err)
	}
	if err := verifyRefreshFits(n, []object{podOnWorker(name, "lots")}); err == nil {
		t.Fatal("an unreadable request was accepted")
	}
	withInit := podOnWorker(name, "1Gi")
	withInit["spec"].(object)["initContainers"] = []any{object{"resources": object{"requests": object{"memory": "13Gi"}}}}
	if err := verifyRefreshFits(n, []object{withInit}); err == nil {
		t.Fatal("a large init container request was ignored")
	}
}

func TestRefreshDoesNotRestartKubeletWhenRequestsWouldNotFit(t *testing.T) {
	n := nodeFixture()
	n["status"].(object)["nodeInfo"].(object)["bootID"] = "original-boot"
	n["status"].(object)["capacity"] = object{"hugepages-2Mi": "0"}
	n["status"].(object)["allocatable"] = object{"hugepages-2Mi": "0", "memory": "14980Mi"}
	run := func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
		switch program {
		case "git":
			return []byte(strings.Repeat("1", 40) + "\trefs/heads/main\n"), nil
		case "kubectl":
			return json.Marshal(n)
		case "talosctl":
			if len(args) == 5 && args[2] == "systeminformation" {
				return json.Marshal(object{"metadata": object{"id": "systeminformation", "namespace": "hardware"}, "spec": object{"uuid": "machine-uuid"}})
			}
			if args[1] == "read" && args[2] == "/proc/sys/vm/nr_hugepages" {
				return []byte("1024\n"), nil
			}
			if args[1] == "read" {
				return []byte("nvme_tcp 1 0 - Live 0\nvfio_pci 1 0 - Live 0\nuio_pci_generic 1 0 - Live 0\n"), nil
			}
			t.Fatalf("kubelet was restarted over a request that does not fit: %v", args)
		}
		return nil, nil
	}
	err := refreshHugepages(context.Background(), client{run: run, context: "admin@prod"}, strings.Repeat("1", 40), n, []object{podOnWorker(text(n, "metadata", "name"), "14456Mi")})
	if err == nil || !strings.Contains(err.Error(), "after huge pages") {
		t.Fatalf("refresh did not hold: %v", err)
	}
}

func TestPodMemoryRequestCountsSidecarsAndPodLevelRequests(t *testing.T) {
	sized := func(memory string, sidecar bool) object {
		c := object{"resources": object{"requests": object{"memory": memory}}}
		if sidecar {
			c["restartPolicy"] = "Always"
		}
		return c
	}
	pod := podOnWorker("w", "12Gi")
	pod["spec"].(object)["initContainers"] = []any{sized("1Gi", true), sized("2Gi", false)}
	if got, err := podMemoryRequest(pod); err != nil || got != 13*(1<<30) {
		t.Fatalf("native sidecar must add to the running containers: %v %v", got, err)
	}
	pod["spec"].(object)["initContainers"] = []any{sized("1Gi", true), sized("13Gi", false)}
	if got, err := podMemoryRequest(pod); err != nil || got != 14*(1<<30) {
		t.Fatalf("init peak must include the sidecars started before it: %v %v", got, err)
	}
	pod = podOnWorker("w", "1Gi")
	pod["spec"].(object)["resources"] = object{"requests": object{"memory": "3Gi"}}
	pod["spec"].(object)["overhead"] = object{"memory": "1Gi"}
	if got, err := podMemoryRequest(pod); err != nil || got != 4*(1<<30) {
		t.Fatalf("a Pod-level request must win and keep its overhead: %v %v", got, err)
	}
}

func TestRefreshFitSubtractsOnlyUncountedHugepages(t *testing.T) {
	n := nodeFixture()
	name := text(n, "metadata", "name")
	n["status"].(object)["allocatable"] = object{"memory": "12932Mi", "hugepages-2Mi": "2Gi"}
	if err := verifyRefreshFits(n, []object{podOnWorker(name, "12932Mi")}); err != nil {
		t.Fatalf("already counted huge pages were subtracted twice: %v", err)
	}
	n["status"].(object)["allocatable"] = object{"memory": "14980Mi", "hugepages-2Mi": "1Gi"}
	if err := verifyRefreshFits(n, []object{podOnWorker(name, "13956Mi")}); err != nil {
		t.Fatalf("partly counted huge pages were subtracted in full: %v", err)
	}
	if err := verifyRefreshFits(n, []object{podOnWorker(name, "13957Mi")}); err == nil {
		t.Fatal("requests above the remaining allocatable were allowed")
	}
}

func TestRefreshJudgesThePodsARestartKept(t *testing.T) {
	pod := func(uid, node, phase, reason string) object {
		return object{"metadata": object{"uid": uid}, "spec": object{"nodeName": node}, "status": object{"phase": phase, "reason": reason}}
	}
	before := []object{pod("old-failure", "prod-worker-1", "Failed", "OutOfmemory"), pod("kept", "prod-worker-1", "Running", "")}
	if err := verifyRestartKeptPods("prod-worker-1", before, append(before, pod("job", "prod-worker-1", "Failed", "Error"), pod("elsewhere", "prod-worker-2", "Failed", "OutOfmemory"))); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"OutOfmemory", "OutOfcpu", "UnexpectedAdmissionError"} {
		if err := verifyRestartKeptPods("prod-worker-1", before, []object{pod("kept", "prod-worker-1", "Failed", reason)}); err == nil {
			t.Fatal("rejected Pod passed: " + reason)
		}
	}
	if err := verifyRestartKeptPods("prod-worker-1", before, []object{pod("other", "prod-worker-2", "Running", "")}); err == nil {
		t.Fatal("a Pod that vanished after the restart passed")
	}
	if err := verifyRestartKeptPods("prod-worker-1", before[:1], nil); err == nil {
		t.Fatal("an empty inventory passed")
	}
}
