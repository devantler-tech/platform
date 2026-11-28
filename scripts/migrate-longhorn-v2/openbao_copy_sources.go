package main

import (
	"strconv"
	"strings"
)

func verifyOpenBaoCopySources(plan openBaoPairPlan, copyPod, node object, pods, claims, pvs, attachments, provider []object) error {
	if verifyOpenBaoPairPlan(plan, plan.Run, plan.Source) != nil || pods == nil ||
		!recoveredNativeObject(node, "v1", "Node", "") || text(node, "metadata", "name") != plan.Original.Node ||
		!ready(valueObject(node, "status")) || value(node, "spec", "unschedulable") == true {
		return errOpenBaoProviderStorage
	}
	desired := plan.copyPod()
	expected, err := copyPodDesiredTopology(copyPod, desired, node, plan.Original.Node)
	if err != nil || !matchStorageCopyVolumePod(copyPod, copyPod, expected, openBaoNamespace, openBaoCopyFenceOwner) ||
		text(copyPod, "spec", "nodeName") != plan.Original.Node {
		return errOpenBaoProviderStorage
	}
	switch text(copyPod, "status", "phase") {
	case "Pending", "Running":
	default:
		return errOpenBaoProviderStorage
	}
	serverID, ok := openBaoServerID(node)
	if !ok {
		return errOpenBaoProviderStorage
	}
	for _, disk := range plan.Original.Disks {
		claim := uniqueOpenBaoObject(claims, disk.PVC.Name)
		pv := uniqueOpenBaoObject(pvs, disk.PV.Name)
		if benchmarkIdentity(claim) != disk.PVC || benchmarkIdentity(pv) != disk.PV ||
			!openBaoRetainedPVBinding(pv, disk.PV, disk.PVC, "hcloud", hetznerCSIDriver, disk.ProviderHandle, disk.Bytes) {
			return errOpenBaoProviderStorage
		}
		rows, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
		ticket, bindErr := bindOpenBaoProviderAttachment(attachments, disk.PV.Name, plan.Original.Node)
		if err != nil || len(rows) != 1 || rows[0].RequestedBytes != disk.Bytes || bindErr != nil || ticket.UID == disk.Attachment.UID ||
			!openBaoProviderAssignedToServer(provider, disk, serverID) {
			return errOpenBaoProviderStorage
		}
		if !openBaoCopyIsOnlySourceConsumer(plan.Original.Member.Name, disk.PVC.Name, copyPod, pods) {
			return errOpenBaoProviderStorage
		}
	}
	return nil
}

func openBaoServerID(node object) (int64, bool) {
	server, ok := strings.CutPrefix(text(node, "spec", "providerID"), "hcloud://")
	id, err := strconv.ParseInt(server, 10, 64)
	return id, ok && err == nil && id > 0 && id < 1<<53 && strconv.FormatInt(id, 10) == server
}

func openBaoProviderAssignedToServer(provider []object, disk openBaoProviderDisk, serverID int64) bool {
	id, err := strconv.ParseInt(disk.ProviderHandle, 10, 64)
	if err != nil || id <= 0 || id >= 1<<53 || strconv.FormatInt(id, 10) != disk.ProviderHandle {
		return false
	}
	found := false
	for _, volume := range provider {
		if volume["id"] != float64(id) {
			continue
		}
		legacy, present := volume["server"]
		if found || !openBaoProviderVolumeMatches(volume, disk) || present && legacy != float64(serverID) ||
			text(volume, "assignee", "type") != "server" || value(volume, "assignee", "id") != float64(serverID) {
			return false
		}
		found = true
	}
	return found
}

func openBaoCopyIsOnlySourceConsumer(original, claimName string, copyPod object, pods []object) bool {
	seen := false
	for _, pod := range pods {
		if text(pod, "metadata", "namespace") != openBaoNamespace {
			continue
		}
		if text(pod, "metadata", "name") == original {
			return false
		}
		volumes, _ := value(pod, "spec", "volumes").([]any)
		for _, raw := range volumes {
			volume, _ := raw.(object)
			if text(volume, "persistentVolumeClaim", "claimName") != claimName {
				continue
			}
			if seen || benchmarkIdentity(pod) != benchmarkIdentity(copyPod) ||
				value(pod, "metadata", "deletionTimestamp") != nil || value(volume, "persistentVolumeClaim", "readOnly") != true {
				return false
			}
			seen = true
		}
	}
	return seen
}
