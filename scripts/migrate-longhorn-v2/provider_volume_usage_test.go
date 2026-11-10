package main

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestProviderUsageBindsFreshStatsToTheObservedMount(t *testing.T) {
	now := time.Date(2026, time.November, 10, 12, 0, 0, 0, time.UTC)
	identity := providerVolumeClaim{Namespace: "provider-app", Claim: "data", ClaimUID: "claim-uid", RequestedBytes: 10 << 30}
	mount := providerPodMount{Claim: identity, Node: "storage-worker", Namespace: "provider-app", Pod: "app-0", PodUID: "pod-uid", Volume: "data"}
	for _, test := range []struct {
		name   string
		edit   func(object, object, object)
		proven bool
	}{
		{"matching mounted filesystem", func(object, object, object) {}, true},
		{"replacement Pod", func(_ object, pod, _ object) { valueObject(pod, "podRef")["uid"] = "other" }, false},
		{"different claim", func(_, _ object, volume object) { valueObject(volume, "pvcRef")["name"] = "other" }, false},
		{"different node", func(summary, _, _ object) { valueObject(summary, "node")["nodeName"] = "other" }, false},
		{"old filesystem measurement", func(_, _ object, volume object) { volume["time"] = now.Add(-10 * time.Minute) }, false},
		{"missing usage", func(_, _ object, volume object) { delete(volume, "usedBytes") }, false},
		{"larger than the provider volume", func(_, _ object, volume object) { volume["capacityBytes"] = 11 << 30 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			volume := object{"name": "data", "pvcRef": object{"name": "data", "namespace": "provider-app"},
				"time": now.Add(-20 * time.Second), "usedBytes": 3 << 30, "availableBytes": 5 << 30, "capacityBytes": 9 << 30}
			pod := object{"podRef": object{"uid": "pod-uid", "name": "app-0", "namespace": "provider-app"}, "volume": []object{volume}}
			summary := object{"node": object{"nodeName": mount.Node}, "pods": []object{pod}}
			test.edit(summary, pod, volume)
			raw, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			got, err := readProviderNodeUsage(raw, mount.Node, []providerPodMount{mount}, now)
			if test.proven {
				if err != nil || got[identity.ClaimUID] != 3<<30 {
					t.Fatalf("matching native usage was rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("unrelated or incomplete filesystem stats qualified")
			}
		})
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
		{[]string{"--raw=/api/v1/nodes/prod-worker-1/proxy/stats/summary"}, true},
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
}
