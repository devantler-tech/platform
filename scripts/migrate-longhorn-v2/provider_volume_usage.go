package main

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type providerPodMount struct {
	Claim                                providerVolumeClaim
	Node, Namespace, Pod, PodUID, Volume string
}

// A finished job or dormant claim remains part of requested capacity. Only a
// unique current Pod mount can contribute observed filesystem usage.
func selectProviderPodMounts(claims []providerVolumeClaim, pods []object) ([]providerPodMount, error) {
	unverified := errors.New("provider claim mounts changed or are ambiguous")
	if len(claims) == 0 || pods == nil {
		return nil, unverified
	}
	byClaim := make(map[string]providerVolumeClaim, len(claims))
	for _, claim := range claims {
		byClaim[claim.Namespace+"/"+claim.Claim] = claim
	}
	seen := map[string]bool{}
	mounts := make([]providerPodMount, 0, len(claims))
	for _, pod := range pods {
		phase := text(pod, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" {
			continue
		}
		ns, name, uid := text(pod, "metadata", "namespace"), text(pod, "metadata", "name"), text(pod, "metadata", "uid")
		node := text(pod, "spec", "nodeName")
		volumes, _ := value(pod, "spec", "volumes").([]any)
		for _, entry := range volumes {
			volume, _ := entry.(object)
			claim, selected := byClaim[ns+"/"+text(volume, "persistentVolumeClaim", "claimName")]
			if !selected {
				continue
			}
			volumeName := text(volume, "name")
			if seen[claim.ClaimUID] || phase != "Running" || value(pod, "metadata", "deletionTimestamp") != nil ||
				uid == "" || !kubeNamespace.MatchString(node) || safeKubeRequest("pods", ns, name, false) != nil ||
				!kubeNamespace.MatchString(volumeName) || !providerPodUsesMount(pod, volumeName) {
				return nil, unverified
			}
			seen[claim.ClaimUID] = true
			mounts = append(mounts, providerPodMount{Claim: claim, Node: node, Namespace: ns, Pod: name, PodUID: uid, Volume: volumeName})
		}
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Claim.ClaimUID < mounts[j].Claim.ClaimUID })
	return mounts, nil
}

func providerPodUsesMount(pod object, volume string) bool {
	containers, _ := value(pod, "spec", "containers").([]any)
	for _, entry := range containers {
		container, _ := entry.(object)
		mounts, _ := value(container, "volumeMounts").([]any)
		for _, raw := range mounts {
			mount, _ := raw.(object)
			if text(mount, "name") == volume && text(mount, "mountPath") != "" {
				return true
			}
		}
	}
	return false
}

type providerFilesystemStats struct {
	Name   string    `json:"name"`
	Time   time.Time `json:"time"`
	PVCRef struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"pvcRef"`
	Used      *uint64 `json:"usedBytes"`
	Available *uint64 `json:"availableBytes"`
	Capacity  *uint64 `json:"capacityBytes"`
}

func readProviderNodeUsage(raw []byte, node string, mounts []providerPodMount, now time.Time) (map[string]uint64, error) {
	unverified := errors.New("provider filesystem usage is incomplete or unrelated to the observed mount")
	if len(raw) > 16<<20 || node == "" || len(mounts) == 0 || now.IsZero() || strictjson.Validate(raw, map[string][]string{
		"":                       {"node", "pods"},
		"node":                   {"nodeName"},
		"pods[]":                 {"podRef", "volume"},
		"pods[].podRef":          {"name", "namespace", "uid"},
		"pods[].volume[]":        {"name", "time", "pvcRef", "usedBytes", "availableBytes", "capacityBytes"},
		"pods[].volume[].pvcRef": {"name", "namespace"},
	}) != nil {
		return nil, unverified
	}
	var summary struct {
		Node struct {
			Name string `json:"nodeName"`
		} `json:"node"`
		Pods []struct {
			Ref struct {
				UID       string `json:"uid"`
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"podRef"`
			Volumes []providerFilesystemStats `json:"volume"`
		} `json:"pods"`
	}
	if json.Unmarshal(raw, &summary) != nil || summary.Node.Name != node || summary.Pods == nil {
		return nil, unverified
	}
	type podMountKey struct{ namespace, pod, uid, volume string }
	lookup := make(map[podMountKey]providerFilesystemStats, len(mounts))
	for _, pod := range summary.Pods {
		for _, volume := range pod.Volumes {
			key := podMountKey{pod.Ref.Namespace, pod.Ref.Name, pod.Ref.UID, volume.Name}
			if _, exists := lookup[key]; exists {
				return nil, unverified
			}
			lookup[key] = volume
		}
	}
	usage := make(map[string]uint64, len(mounts))
	for _, mount := range mounts {
		stats, found := lookup[podMountKey{mount.Namespace, mount.Pod, mount.PodUID, mount.Volume}]
		uid := mount.Claim.ClaimUID
		_, repeated := usage[uid]
		if !found || repeated || uid == "" || mount.PodUID == "" || mount.Node != node ||
			stats.PVCRef.Name != mount.Claim.Claim || stats.PVCRef.Namespace != mount.Claim.Namespace ||
			stats.Time.Before(now.Add(-3*time.Minute)) || stats.Time.After(now.Add(30*time.Second)) ||
			stats.Used == nil || stats.Available == nil || stats.Capacity == nil || *stats.Capacity == 0 ||
			*stats.Capacity > mount.Claim.RequestedBytes || *stats.Available > *stats.Capacity ||
			*stats.Used > *stats.Capacity-*stats.Available {
			return nil, unverified
		}
		usage[uid] = *stats.Used
	}
	return usage, nil
}
