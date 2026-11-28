package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

const (
	openBaoNamespace     = "openbao"
	openBaoPairOperation = "migrate-openbao-v2-pair"
)

var errOpenBaoProviderStorage = errors.New("the OpenBao member's retained provider storage pair is unverified")

type openBaoProviderDisk struct {
	PVC, PV, Attachment benchmarkResource
	ProviderHandle      string
	Bytes               uint64
}

type openBaoProviderStorage struct {
	Member, StatefulSet benchmarkResource
	Node                string
	Disks               [2]openBaoProviderDisk
}

type openBaoPairInput struct{ Member, Digest string }

func authorizeOpenBaoPair(getenv func(string) string) (openBaoPairInput, string, error) {
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	fields := strings.Split(confirmation, ":")
	if getenv("LONGHORN_V2_OPERATION") != openBaoPairOperation || getenv("LONGHORN_V2_NODE") != "" ||
		getenv("LONGHORN_V2_DATABASE") != "" || (len(fields) != 3 && len(fields) != 4) || fields[0] != openBaoPairOperation ||
		!knownOpenBaoMember(fields[1]) || !retentionDigest.MatchString(fields[2]) {
		return openBaoPairInput{}, "", errOpenBaoProviderStorage
	}
	var priorRun string
	if len(fields) == 4 {
		priorRun = fields[3]
		if !validBenchmarkOwner(priorRun, getenv("GITHUB_SHA")) || priorRun == getenv("GITHUB_RUN_ID") {
			return openBaoPairInput{}, "", errOpenBaoProviderStorage
		}
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return openBaoPairInput{}, "", err
	}
	return openBaoPairInput{Member: fields[1], Digest: fields[2]}, priorRun, nil
}

func knownOpenBaoMember(name string) bool {
	return slices.Contains([]string{"openbao-0", "openbao-1", "openbao-2"}, name)
}

// Use only the SHA-256 confirmation as a workflow input. Neither claim nor
// provider identities need to leave the protected executor.
func openBaoProviderStorageDigest(bound openBaoProviderStorage) (string, error) {
	encoded, err := json.Marshal(bound)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}

// Bind the exact pair still mounted by this member. The protected operation
// must independently prove Raft continuity and the provider's attachment.
func bindOpenBaoProviderStorage(target string, stateful object, pods, claims, pvs, attachments []object) (openBaoProviderStorage, error) {
	var zero openBaoProviderStorage
	if !knownOpenBaoMember(target) ||
		!recoveredNativeObject(stateful, "apps/v1", "StatefulSet", openBaoNamespace) ||
		text(stateful, "metadata", "name") != openBaoNamespace || text(stateful, "spec", "updateStrategy", "type") != "OnDelete" ||
		value(stateful, "spec", "replicas") != float64(3) || value(stateful, "status", "readyReplicas") != float64(3) ||
		value(stateful, "status", "updatedReplicas") != float64(3) ||
		value(stateful, "metadata", "generation") != value(stateful, "status", "observedGeneration") {
		return zero, errOpenBaoProviderStorage
	}
	pod := uniqueOpenBaoObject(pods, target)
	node := text(pod, "spec", "nodeName")
	if !openBaoOriginalStorageMember(pod, stateful) || !kubeNamespace.MatchString(node) {
		return zero, errOpenBaoProviderStorage
	}
	bound := openBaoProviderStorage{Member: benchmarkIdentity(pod), StatefulSet: benchmarkIdentity(stateful), Node: node}
	mounts := []struct {
		name string
		size uint64
	}{{"data", 10 << 30}, {"audit", 40 << 30}}
	selected := make([]providerVolumeClaim, 0, len(mounts))
	for i, mount := range mounts {
		claim := uniqueOpenBaoObject(claims, mount.name+"-"+target)
		pv := uniqueOpenBaoObject(pvs, text(claim, "spec", "volumeName"))
		rows, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
		if err != nil || len(rows) != 1 || !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", openBaoNamespace) ||
			!recoveredNativeObject(pv, "v1", "PersistentVolume", "") || text(claim, "spec", "storageClassName") != "hcloud" ||
			text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" || rows[0].RequestedBytes != mount.size ||
			providerVolumeBytes(text(claim, "spec", "resources", "requests", "storage")) != mount.size {
			return zero, errOpenBaoProviderStorage
		}
		attachment, err := bindOpenBaoProviderAttachment(attachments, rows[0].PV, node)
		if err != nil || !openBaoExactStorageMount(pod, mount.name) {
			return zero, errOpenBaoProviderStorage
		}
		bound.Disks[i] = openBaoProviderDisk{
			PVC: benchmarkIdentity(claim), PV: benchmarkIdentity(pv), Attachment: attachment,
			ProviderHandle: rows[0].Handle, Bytes: rows[0].RequestedBytes,
		}
		selected = append(selected, rows[0])
	}
	if bound.Disks[0].ProviderHandle == bound.Disks[1].ProviderHandle || bound.Disks[0].PV == bound.Disks[1].PV ||
		bound.Disks[0].Attachment == bound.Disks[1].Attachment || bound.Disks[0].PVC == bound.Disks[1].PVC {
		return zero, errOpenBaoProviderStorage
	}
	consumers, err := selectProviderPodMounts(selected, pods)
	if err != nil || len(consumers) != 2 {
		return zero, errOpenBaoProviderStorage
	}
	for _, mount := range consumers {
		if mount.Pod != target || mount.PodUID != bound.Member.UID || mount.Node != node ||
			mount.Volume != strings.TrimSuffix(mount.Claim.Claim, "-"+target) {
			return zero, errOpenBaoProviderStorage
		}
	}
	return bound, nil
}

// Before the isolated copy can remount the pair, prove the original member
// has released both disks and each provider still holds the exact CSI source.
func verifyOpenBaoDetachedSources(bound openBaoProviderStorage, claims, pvs, pods, attachments, provider []object) error {
	if !knownOpenBaoMember(bound.Member.Name) || bound.Member.Namespace != openBaoNamespace ||
		bound.Disks[0].ProviderHandle == bound.Disks[1].ProviderHandle || pods == nil || attachments == nil {
		return errOpenBaoProviderStorage
	}
	for i, disk := range bound.Disks {
		name := [2]string{"data-", "audit-"}[i] + bound.Member.Name
		size := [2]uint64{10 << 30, 40 << 30}[i]
		claim := uniqueOpenBaoObject(claims, name)
		pv := uniqueOpenBaoObject(pvs, disk.PV.Name)
		if disk.PVC.Name != name || disk.Bytes != size || benchmarkIdentity(claim) != disk.PVC ||
			!recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", openBaoNamespace) ||
			!openBaoRetainedPVBinding(pv, disk.PV, disk.PVC, "hcloud", hetznerCSIDriver, disk.ProviderHandle, size) {
			return errOpenBaoProviderStorage
		}
		bindings, err := selectProviderVolumeClaims([]object{pv}, []object{claim})
		if err != nil || len(bindings) != 1 || bindings[0].RequestedBytes != size || !openBaoProviderVolumeDetached(provider, disk) {
			return errOpenBaoProviderStorage
		}
		for _, candidate := range pvs {
			if text(candidate, "spec", "csi", "driver") == hetznerCSIDriver &&
				text(candidate, "spec", "csi", "volumeHandle") == disk.ProviderHandle && benchmarkIdentity(candidate) != disk.PV {
				return errOpenBaoProviderStorage
			}
		}
		for _, attachment := range attachments {
			if benchmarkIdentity(attachment) == disk.Attachment || text(attachment, "spec", "source", "persistentVolumeName") == disk.PV.Name {
				return errOpenBaoProviderStorage
			}
		}
		for _, pod := range pods {
			if text(pod, "metadata", "namespace") != openBaoNamespace {
				continue
			}
			if text(pod, "metadata", "name") == bound.Member.Name {
				return errOpenBaoProviderStorage
			}
			volumes, _ := value(pod, "spec", "volumes").([]any)
			for _, entry := range volumes {
				volume, _ := entry.(object)
				if text(volume, "persistentVolumeClaim", "claimName") == name {
					return errOpenBaoProviderStorage
				}
			}
		}
	}
	return nil
}

func openBaoProviderVolumeDetached(volumes []object, disk openBaoProviderDisk) bool {
	id, err := strconv.ParseInt(disk.ProviderHandle, 10, 64)
	if err != nil || id <= 0 || id >= 1<<53 || strconv.FormatInt(id, 10) != disk.ProviderHandle {
		return false
	}
	found := false
	for _, volume := range volumes {
		if volume["id"] != float64(id) {
			continue
		}
		assignee, known := volume["assignee"]
		if found || !known || assignee != nil || volume["server"] != nil ||
			!openBaoProviderVolumeMatches(volume, disk) {
			return false
		}
		found = true
	}
	return found
}

func openBaoProviderVolumeMatches(volume object, disk openBaoProviderDisk) bool {
	id, err := strconv.ParseInt(disk.ProviderHandle, 10, 64)
	return err == nil && id > 0 && id < 1<<53 && strconv.FormatInt(id, 10) == disk.ProviderHandle &&
		volume["id"] == float64(id) && volume["size"] == float64(disk.Bytes>>30) && volume["status"] == "available" &&
		text(volume, "linux_device") == "/dev/disk/by-id/scsi-0HC_Volume_"+disk.ProviderHandle &&
		text(volume, "labels", "managed-by") == "csi-driver" && text(volume, "labels", "pv-name") == disk.PV.Name &&
		text(volume, "labels", "pvc-name") == disk.PVC.Name && text(volume, "labels", "pvc-namespace") == openBaoNamespace
}

func uniqueOpenBaoObject(items []object, name string) object {
	var chosen object
	if name == "" {
		return nil
	}
	for _, item := range items {
		if text(item, "metadata", "name") == name {
			if chosen != nil {
				return nil
			}
			chosen = item
		}
	}
	return chosen
}

func openBaoOriginalStorageMember(pod, stateful object) bool {
	revision := text(stateful, "status", "updateRevision")
	if !recoveredNativeObject(pod, "v1", "Pod", openBaoNamespace) || text(pod, "status", "phase") != "Running" ||
		!ready(valueObject(pod, "status")) || revision == "" || text(pod, "metadata", "labels", "controller-revision-hash") != revision ||
		value(pod, "spec", "securityContext", "runAsUser") != float64(100) ||
		value(pod, "spec", "securityContext", "runAsGroup") != float64(1000) ||
		value(pod, "spec", "securityContext", "fsGroup") != float64(1000) {
		return false
	}
	refs, _ := value(pod, "metadata", "ownerReferences").([]any)
	if len(refs) != 1 {
		return false
	}
	owner, _ := refs[0].(object)
	return text(owner, "apiVersion") == "apps/v1" && text(owner, "kind") == "StatefulSet" &&
		text(owner, "name") == openBaoNamespace && text(owner, "uid") == text(stateful, "metadata", "uid") && value(owner, "controller") == true
}

func openBaoExactStorageMount(pod object, name string) bool {
	expected := "/openbao/" + name
	containers, _ := value(pod, "spec", "containers").([]any)
	matches := 0
	for _, entry := range containers {
		container, _ := entry.(object)
		service := text(container, "name") == openBaoNamespace
		mounts, _ := value(container, "volumeMounts").([]any)
		for _, entry := range mounts {
			mount, _ := entry.(object)
			location := text(mount, "mountPath")
			if text(mount, "name") != name {
				if service && (!path.IsAbs(location) || path.Clean(location) != location ||
					location == "/" || location == expected || strings.HasPrefix(location, expected+"/") ||
					strings.HasPrefix(expected, location+"/")) {
					return false
				}
				continue
			}
			if !service || location != expected ||
				value(mount, "readOnly") == true || !absentOrEmptyString(value(mount, "subPath")) ||
				!absentOrEmptyString(value(mount, "subPathExpr")) {
				return false
			}
			matches++
		}
	}
	return matches == 1
}

func bindOpenBaoProviderAttachment(attachments []object, pv, node string) (benchmarkResource, error) {
	var identity benchmarkResource
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") != pv {
			continue
		}
		if identity.UID != "" || !recoveredNativeObject(attachment, "storage.k8s.io/v1", "VolumeAttachment", "") ||
			text(attachment, "spec", "attacher") != hetznerCSIDriver || text(attachment, "spec", "nodeName") != node ||
			value(attachment, "status", "attached") != true {
			return benchmarkResource{}, errOpenBaoProviderStorage
		}
		identity = benchmarkIdentity(attachment)
	}
	if identity.UID == "" {
		return benchmarkResource{}, errOpenBaoProviderStorage
	}
	return identity, nil
}
