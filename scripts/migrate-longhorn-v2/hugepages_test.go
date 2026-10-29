package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestHugepageRefreshRestartsOnlyKubeletAndRequiresPositiveReadback(t *testing.T) {
	for _, alreadyReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "restart needed", true: "already ready"}[alreadyReady], func(t *testing.T) {
			n := nodeFixture()
			n["status"].(object)["nodeInfo"].(object)["bootID"] = "original-boot"
			n["status"].(object)["capacity"] = object{"hugepages-2Mi": "0"}
			n["status"].(object)["allocatable"] = object{"hugepages-2Mi": "0"}
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
			if err := refreshHugepages(context.Background(), client{run: run, context: "admin@prod"}, strings.Repeat("1", 40), n); err != nil {
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
