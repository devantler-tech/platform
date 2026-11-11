package main

import (
	"slices"
	"testing"

	machine "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

func TestProviderUsageBindsNativeStatsToTheObservedMount(t *testing.T) {
	claim := providerVolumeClaim{ClaimUID: "claim-uid", PV: "pvc-original", Handle: "123", RequestedBytes: 10 << 30}
	mount := providerPodMount{Claim: claim, PodUID: "pod-uid"}
	devices := map[string]string{"123": "/dev/sdb"}
	for _, test := range []struct {
		name   string
		edit   func(*machine.MountStat) []*machine.MountStat
		proven bool
	}{
		{"matching filesystem", func(s *machine.MountStat) []*machine.MountStat { return []*machine.MountStat{s} }, true},
		{"replacement Pod", func(s *machine.MountStat) []*machine.MountStat {
			s.MountedOn = "/var/lib/kubelet/pods/other/volumes/kubernetes.io~csi/pvc-original/mount"
			return []*machine.MountStat{s}
		}, false},
		{"different device", func(s *machine.MountStat) []*machine.MountStat {
			s.Filesystem = "/dev/sdc"
			return []*machine.MountStat{s}
		}, false},
		{"duplicate mount path", func(s *machine.MountStat) []*machine.MountStat { return []*machine.MountStat{s, s} }, false},
		{"oversized filesystem", func(s *machine.MountStat) []*machine.MountStat {
			s.Size = 11 << 30
			return []*machine.MountStat{s}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			stat := &machine.MountStat{MountedOn: "/var/lib/kubelet/pods/pod-uid/volumes/kubernetes.io~csi/pvc-original/mount",
				Filesystem: "/dev/sdb", Size: 9 << 30, Available: 5 << 30}
			got, err := matchProviderMounts(test.edit(stat), []providerPodMount{mount}, devices)
			if test.proven {
				if err != nil || got[claim.ClaimUID] != 4<<30 {
					t.Fatalf("matching native usage was rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("unrelated or ambiguous filesystem usage qualified")
			}
		})
	}
}

func TestProviderDevicesBindAUniqueNativeDisk(t *testing.T) {
	good := `{"metadata":{"namespace":"runtime","type":"Disks.block.talos.dev","phase":"running","id":"sdb"},"spec":{"dev_path":"/dev/sdb","symlinks":["/dev/disk/by-id/scsi-0HC_Volume_123"]}}`
	mapper := `{"metadata":{"namespace":"runtime","type":"Disks.block.talos.dev","phase":"running","id":"dm-0"},"spec":{"dev_path":"/dev/dm-0"}}`
	devices, err := parseProviderDevices([]byte(good + mapper))
	if err != nil || devices["123"] != "/dev/sdb" {
		t.Fatalf("matching disk was rejected: %v", err)
	}
	for _, raw := range []string{good + good, good + "{", `{"metadata":{},"metadata":{},"spec":{}}`} {
		if _, err := parseProviderDevices([]byte(raw)); err == nil {
			t.Fatal("an ambiguous disk inventory qualified")
		}
	}
}

func TestProviderUsageKeepsUnmeasuredSourcesAtTheirFullRequest(t *testing.T) {
	claims := []providerVolumeClaim{
		{Namespace: "provider-app", Claim: "data", ClaimUID: "live", RequestedBytes: 10 << 30},
		{Namespace: "provider-app", Claim: "archive", ClaimUID: "dormant", RequestedBytes: 20 << 30},
	}
	pod := func(uid, claim, phase string) object {
		return object{
			"metadata": object{"name": "app-" + uid, "namespace": "provider-app", "uid": uid},
			"spec": object{
				"nodeName":   "prod-worker-1",
				"volumes":    []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": claim}}},
				"containers": []any{object{"volumeMounts": []any{object{"name": "data", "mountPath": "/data"}}}},
			},
			"status": object{"phase": phase},
		}
	}
	mounts, err := selectProviderPodMounts(claims, []object{pod("live-reader", "data", "Running"), pod("old-job", "archive", "Succeeded")})
	if err != nil || len(mounts) != 1 || mounts[0].Claim.ClaimUID != "live" {
		t.Fatalf("original running mount was not identified: %v", err)
	}
	if _, err := selectProviderPodMounts(claims, []object{pod("live-reader", "data", "Running"), pod("competing", "data", "Running")}); err == nil {
		t.Fatal("multiple consumers of one provider volume qualified")
	}
	receipt, err := summarizeProviderVolumeUsage(claims, map[string]uint64{"live": 3 << 30})
	if err != nil || receipt.Measured != 1 || receipt.Unmeasured != 1 || receipt.MeasuredUsedBytes != 3<<30 ||
		receipt.UnmeasuredRequestedBytes != 20<<30 || receipt.RequestedBytes != 30<<30 {
		t.Fatalf("unmeasured original was undercounted: %+v %v", receipt, err)
	}
	if _, err := summarizeProviderVolumeUsage(claims, map[string]uint64{"unknown-uid": 0}); err == nil {
		t.Fatal("usage outside the verified source inventory was counted")
	}
}

func TestProviderFootprintNativeCommandHasOnlyFixedReadRoutes(t *testing.T) {
	prefix := []string{"--context=admin@prod", "--request-timeout=20s", "get"}
	for _, test := range []struct {
		tail    []string
		allowed bool
	}{
		{[]string{"persistentvolumes", "--output=json"}, true},
		{[]string{"persistentvolumeclaims", "--output=json", "--all-namespaces"}, true},
		{[]string{"pods", "--output=json", "--namespace=openbao"}, true},
		{[]string{"node", "prod-worker-2", "--output=json"}, true},
		{[]string{"--raw=/api/v1/nodes/prod-worker-1/proxy/stats/summary"}, false},
		{[]string{"--raw=/api/v1/nodes/prod-worker-1/proxy/stats/summary?foo=bar"}, false},
		{[]string{"--raw=/api/v1/nodes/prod-worker-1/proxy/exec"}, false},
		{[]string{"--raw=/api/v1/nodes/another-node/proxy/stats/summary"}, false},
		{[]string{"pods", "--output=json", "--namespace=other"}, false},
		{[]string{"secrets", "--output=json", "--all-namespaces"}, false},
		{[]string{"node", "prod-worker-1", "--output=json", "--watch"}, false},
	} {
		if got := providerFootprintReadAllowed("kubectl", append(slices.Clone(prefix), test.tail...)...); got != test.allowed {
			t.Errorf("fixed provider read authorization = %t, want %t for %v", got, test.allowed, test.tail)
		}
	}
	if providerFootprintReadAllowed("kubectl", "--context=admin@prod", "--request-timeout=20s", "patch", "node", "prod-worker-1", "--output=json") {
		t.Fatal("a write was accepted")
	}
	if providerFootprintReadAllowed("kubectl", "--context=other", "--request-timeout=20s", "get", "persistentvolumes", "--output=json") {
		t.Fatal("a different context was accepted")
	}
	if !providerFootprintReadAllowed("talosctl", "--nodes=10.0.1.2", "get", "disks", "--output=json") ||
		providerFootprintReadAllowed("talosctl", "--nodes=192.0.2.4", "get", "disks", "--output=json") ||
		providerFootprintReadAllowed("talosctl", "--nodes=10.0.1.2", "get", "machineconfig", "--output=json") {
		t.Fatal("Talos read scope differs from the fixed disk inventory")
	}
}
