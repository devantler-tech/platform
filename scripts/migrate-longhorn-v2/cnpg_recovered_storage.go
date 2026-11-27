package main

import (
	"errors"
	"path"
	"strconv"
	"strings"
)

// This binding is necessary storage evidence, not restore or retirement authority.
type cnpgRecoveredStorage struct {
	Cluster, Pod, Claim, PV, Volume, Engine benchmarkResource
	Process                                 postgresProcess
	Producer                                cnpgRecoveredProducer
	Replicas                                map[string]benchmarkResource
}

// Callers must separately collect complete inventories and bracket SQL with
// native execution, operator, isolation and storage-environment observations.
// This pure join never advances the journal or grants deletion authority.
func bindCNPGRecoveredStorage(r cnpgProofRecord, cluster, pod, claim, pv, volume, engine object, replicas []object) (cnpgRecoveredStorage, error) {
	refused := errors.New("recovered database storage is not qualified")
	var zero cnpgRecoveredStorage
	initial := r.Phase == "cluster-created" && !r.RestoreVerified && r.RestoreReceipt == nil
	retained := r.Phase == cnpgArchiveCredentialRetired && r.RestoreVerified && validateCNPGRestoreReceipt(r) == nil
	if r.Version != 1 || (!initial && !retained) || r.Cleaned || r.Plan == nil || len(r.Plan.Resources) != 4 || r.Admission == nil || r.Storage == nil || len(r.Storage.Disks) != 3 || !validBenchmarkOwner(r.Run, r.Source) {
		return zero, refused
	}
	ns := "longhorn-db-proof-" + r.Target + "-" + r.Run
	if !isolationNamespace.MatchString(ns) || r.Resources["Namespace"].Name != ns || benchmarkIdentity(cluster) != r.Resources["Cluster"] || text(cluster, "metadata", "uid") == r.Plan.Source.ClusterUID || !recoveredNativeObject(cluster, "postgresql.cnpg.io/v1", "Cluster", ns) {
		return zero, refused
	}
	if _, err := matchRecoveryAdmission(r.Plan.Resources[3], cluster, r.Admission.CNPG.Defaults); err != nil {
		return zero, refused
	}
	hostname := cnpgRecoveryHostname(r.Plan)
	if hostname == "" || !recoveredNativeObject(pod, "v1", "Pod", ns) || text(pod, "metadata", "name") != "restore-1" || text(pod, "metadata", "labels", "cnpg.io/instanceRole") != "primary" || text(pod, "spec", "nodeName") != hostname || text(cluster, "status", "currentPrimary") != "restore-1" || text(cluster, "status", "targetPrimary") != "restore-1" || value(cluster, "status", "instances") != float64(1) || value(cluster, "status", "readyInstances") != float64(1) || !cnpgCondition(cluster, "Ready") || verifyPostgresPod(pod, pod, cluster) != nil {
		return zero, refused
	}
	process, err := postgresExecutable(pod)
	_, digest, pinned := strings.Cut(text(cluster, "spec", "imageName"), "@")
	if err != nil || !pinned || (process.Image != digest && !strings.HasSuffix(process.Image, "@"+digest)) || !recoveredPostgresMount(pod, claim) || !recoveredPostgresProgram(pod) {
		return zero, refused
	}
	if !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", ns) || !cnpgOwned(claim, cluster) || text(claim, "metadata", "name") != "restore-1" || text(claim, "status", "phase") != "Bound" || text(claim, "spec", "storageClassName") != "longhorn-v2" || !providerRWOFilesystem(claim) || value(claim, "spec", "dataSource") != nil || value(claim, "spec", "dataSourceRef") != nil {
		return zero, refused
	}
	name := "pvc-" + text(claim, "metadata", "uid")
	size := r.Plan.Source.Size
	units := strings.TrimSuffix(size, "Gi")
	gib, e := strconv.ParseUint(units, 10, 64)
	if e != nil || gib < 1 || gib > 96 || strconv.FormatUint(gib, 10)+"Gi" != size || text(claim, "spec", "resources", "requests", "storage") != size || text(claim, "status", "capacity", "storage") != size {
		return zero, refused
	}
	bytes := strconv.FormatUint(gib*recoveryGiB, 10)
	if !recoveredPVBackend(claim, pv, volume, engine, ns, name, size, bytes, hostname) {
		return zero, refused
	}
	copies, err := recoveredNativeCopies(r.Storage.Disks, volume, engine, replicas, bytes)
	if err != nil {
		return zero, refused
	}
	binding := cnpgRecoveredStorage{Cluster: benchmarkIdentity(cluster), Pod: benchmarkIdentity(pod), Claim: benchmarkIdentity(claim), PV: benchmarkIdentity(pv), Volume: benchmarkIdentity(volume), Engine: benchmarkIdentity(engine), Process: process, Producer: recoveredProgramProducer(pod), Replicas: copies}
	if retained {
		receipt := r.RestoreReceipt
		if binding.Cluster != receipt.Cluster || binding.Pod != receipt.Consumer || binding.Claim != receipt.Claim ||
			binding.PV != receipt.PV || binding.Volume != receipt.Volume {
			return zero, refused
		}
	}
	return binding, nil
}

func recoveredNativeObject(o object, api, kind, namespace string) bool {
	name := text(o, "metadata", "name")
	return name != "" && len(name) <= 253 && kubeName.MatchString(name) && text(o, "apiVersion") == api && text(o, "kind") == kind && text(o, "metadata", "namespace") == namespace && isolationUID.MatchString(text(o, "metadata", "uid")) && text(o, "metadata", "resourceVersion") != "" && value(o, "metadata", "deletionTimestamp") == nil
}

// Longhorn's native Volume owner reference does not set controller=true.
// Match its exact parent identity rather than borrowing CNPG/Velero semantics.
func recoveredVolumeOwner(o, volume object) bool {
	refs, ok := value(o, "metadata", "ownerReferences").([]any)
	if !ok || len(refs) != 1 {
		return false
	}
	ref, _ := refs[0].(object)
	return text(ref, "apiVersion") == "longhorn.io/v1beta2" && text(ref, "kind") == "Volume" && text(ref, "name") == text(volume, "metadata", "name") && text(ref, "uid") == text(volume, "metadata", "uid")
}

// Bind the actual writable pgdata mount, rejecting extra claims and mounts that
// could shadow the fixed recovery data directory. SQL must still prove that
// PostgreSQL actually uses this directory, rather than merely mounting it.
func recoveredPostgresMount(pod, claim object) bool {
	volumes, _ := value(pod, "spec", "volumes").([]any)
	data := 0
	for _, raw := range volumes {
		v, _ := raw.(object)
		if value(v, "hostPath") != nil || value(v, "ephemeral") != nil {
			return false
		}
		if value(v, "persistentVolumeClaim") != nil {
			if text(v, "name") != "pgdata" || text(v, "persistentVolumeClaim", "claimName") != text(claim, "metadata", "name") || !falseOrAbsent(value(v, "persistentVolumeClaim", "readOnly")) {
				return false
			}
			data++
		}
	}
	if data != 1 {
		return false
	}
	const directory = "/var/lib/postgresql/data"
	containers, _ := value(pod, "spec", "containers").([]any)
	mounted := 0
	for _, raw := range containers {
		c, _ := raw.(object)
		if text(c, "name") != "postgres" {
			continue
		}
		mounts, _ := value(c, "volumeMounts").([]any)
		for _, raw := range mounts {
			m, _ := raw.(object)
			location := text(m, "mountPath")
			if location == "" || path.Clean(location) != location || !strings.HasPrefix(location, "/") {
				return false
			}
			if text(m, "name") == "pgdata" {
				if location != directory || !falseOrAbsent(value(m, "readOnly")) || !absentOrEmptyString(value(m, "subPath")) || !absentOrEmptyString(value(m, "subPathExpr")) {
					return false
				}
				mounted++
			} else if location == "/" || location == directory || strings.HasPrefix(location, directory+"/") || strings.HasPrefix(directory, location+"/") {
				return false
			}
		}
	}
	return mounted == 1
}

func recoveredPVBackend(claim, pv, volume, engine object, ns, name, size, bytes, hostname string) bool {
	if !recoveredNativeObject(pv, "v1", "PersistentVolume", "") || text(pv, "metadata", "name") != name || text(claim, "spec", "volumeName") != name || text(pv, "status", "phase") != "Bound" || text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" || text(pv, "spec", "claimRef", "uid") != text(claim, "metadata", "uid") || text(pv, "spec", "claimRef", "name") != text(claim, "metadata", "name") || text(pv, "spec", "claimRef", "namespace") != ns || text(pv, "spec", "storageClassName") != "longhorn-v2" || !providerRWOFilesystem(pv) || text(pv, "spec", "capacity", "storage") != size || text(pv, "spec", "csi", "driver") != "driver.longhorn.io" || text(pv, "spec", "csi", "volumeHandle") != name {
		return false
	}
	if !recoveredNativeObject(volume, "longhorn.io/v1beta2", "Volume", "longhorn-system") || text(volume, "metadata", "name") != name || text(volume, "spec", "dataEngine") != "v2" || text(volume, "spec", "size") != bytes || value(volume, "spec", "numberOfReplicas") != float64(3) || text(volume, "status", "state") != "attached" || text(volume, "status", "robustness") != "healthy" || text(volume, "status", "currentNodeID") != hostname || text(volume, "status", "kubernetesStatus", "pvName") != name || text(volume, "status", "kubernetesStatus", "pvcName") != text(claim, "metadata", "name") || text(volume, "status", "kubernetesStatus", "namespace") != ns {
		return false
	}
	return recoveredNativeObject(engine, "longhorn.io/v1beta2", "Engine", "longhorn-system") && recoveredVolumeOwner(engine, volume) && text(engine, "spec", "volumeName") == name && text(engine, "spec", "dataEngine") == "v2" && value(engine, "spec", "active") == true && text(engine, "spec", "nodeID") == hostname && text(engine, "status", "currentState") == "running"
}

func recoveredNativeCopies(disks []cnpgRecoveryDiskBinding, volume, engine object, replicas []object, size string) (map[string]benchmarkResource, error) {
	refused := errors.New("restored copies do not join three independently qualified raw disks")
	bindings, nodes := map[string]cnpgRecoveryDiskBinding{}, map[string]bool{}
	for _, disk := range disks {
		if disk.Node == "" || disk.NodeUID == "" || disk.DiskUUID == "" || disk.InstanceManager == "" || bindings[disk.DiskUUID].Node != "" || nodes[disk.Node] {
			return nil, refused
		}
		bindings[disk.DiskUUID], nodes[disk.Node] = disk, true
	}
	modes, ok := value(engine, "status", "replicaModeMap").(object)
	if !ok || len(modes) != 3 {
		return nil, refused
	}
	copies, seenDisks, seenUIDs := map[string]benchmarkResource{}, map[string]bool{}, map[string]bool{}
	for _, replica := range replicas {
		if !recoveredCopyAssociated(replica, volume, engine) {
			continue
		}
		disk := bindings[text(replica, "spec", "diskID")]
		name, uid := text(replica, "metadata", "name"), text(replica, "metadata", "uid")
		if !recoveredNativeObject(replica, "longhorn.io/v1beta2", "Replica", "longhorn-system") || text(replica, "spec", "volumeName") != text(volume, "metadata", "name") || !recoveredVolumeOwner(replica, volume) || name == "" || copies[name].UID != "" || seenUIDs[uid] || disk.Node == "" || seenDisks[disk.DiskUUID] || text(replica, "spec", "nodeID") != disk.Node || text(replica, "spec", "dataEngine") != "v2" || text(replica, "spec", "engineName") != text(engine, "metadata", "name") || value(replica, "spec", "active") != true || text(replica, "spec", "healthyAt") == "" || !absentOrEmptyString(value(replica, "spec", "failedAt")) || text(replica, "spec", "volumeSize") != size || text(replica, "status", "currentState") != "running" || text(replica, "status", "instanceManagerName") != disk.InstanceManager || modes[name] != "RW" {
			return nil, refused
		}
		copies[name], seenDisks[disk.DiskUUID], seenUIDs[uid] = benchmarkIdentity(replica), true, true
	}
	if len(copies) != 3 {
		return nil, refused
	}
	return copies, nil
}

// Any native linkage makes a Replica relevant. A contradictory declared name
// must be refused rather than filtered out of the three-copy observation.
func recoveredCopyAssociated(replica, volume, engine object) bool {
	if name := text(engine, "metadata", "name"); name != "" && text(replica, "spec", "engineName") == name {
		return true
	}
	return recoveredVolumeAssociated(replica, volume)
}

func recoveredVolumeAssociated(child, volume object) bool {
	if text(child, "spec", "volumeName") == text(volume, "metadata", "name") {
		return true
	}
	refs, _ := value(child, "metadata", "ownerReferences").([]any)
	for _, raw := range refs {
		ref, _ := raw.(object)
		if text(ref, "uid") == text(volume, "metadata", "uid") || text(ref, "kind") == "Volume" && text(ref, "name") == text(volume, "metadata", "name") {
			return true
		}
	}
	return false
}
