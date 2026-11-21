package main

import (
	"errors"
	"sort"
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
				!kubeNamespace.MatchString(uid) || !kubeNamespace.MatchString(node) || safeKubeRequest("pods", ns, name, false) != nil ||
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
