package main

import (
	"fmt"
	"strings"
	"testing"
)

type recoveredStorageFixture struct {
	p                                       *cnpgProofRun
	cluster, pod, claim, pv, volume, engine object
	replicas                                []object
}

func newRecoveredStorageFixture(t *testing.T) recoveredStorageFixture {
	t.Helper()
	p, cnpgSHA, barmanSHA, _ := databaseCreationFixture(t, "confirmed")
	if _, err := p.createDatabaseResourceWithSchemas(t.Context(), "ObjectStore", cnpgSHA, barmanSHA); err != nil {
		t.Fatal(err)
	}
	cluster, err := p.createDatabaseResourceWithSchemas(t.Context(), "Cluster", cnpgSHA, barmanSHA)
	if err != nil {
		t.Fatal(err)
	}
	ns := p.record.Resources["Namespace"].Name
	hostname := cnpgRecoveryHostname(p.record.Plan)
	uid := func(n int) string { return fmt.Sprintf("%08d-1111-4111-8111-111111111111", n) }
	meta := func(name string, n int) object {
		return object{"name": name, "namespace": ns, "uid": uid(n), "resourceVersion": "10", "labels": object{"cnpg.io/cluster": "restore"}, "ownerReferences": []any{object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "restore", "uid": text(cluster, "metadata", "uid"), "controller": true}}}
	}
	claim := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("restore-1", 2), "spec": object{"storageClassName": "longhorn-v2", "volumeName": "pvc-" + uid(2), "volumeMode": "Filesystem", "accessModes": []any{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": p.record.Plan.Source.Size}}}, "status": object{"phase": "Bound", "capacity": object{"storage": p.record.Plan.Source.Size}}}
	pod := cnpgObservationFixture(t).Pods[0]
	pod["apiVersion"], pod["kind"] = "v1", "Pod"
	pod["metadata"] = meta("restore-1", 1)
	pod["metadata"].(object)["labels"].(object)["cnpg.io/instanceRole"] = "primary"
	pod["spec"].(object)["nodeName"] = hostname
	pod["spec"].(object)["volumes"] = []any{object{"name": "pgdata", "persistentVolumeClaim": object{"claimName": "restore-1"}}}
	pod["spec"].(object)["containers"] = []any{object{"name": "postgres", "image": text(cluster, "spec", "imageName"), "volumeMounts": []any{object{"name": "pgdata", "mountPath": "/var/lib/postgresql/data"}}}}
	value(pod, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "ghcr.io/cloudnative-pg/postgresql@sha256:" + strings.Repeat("a", 64)
	addRecoveredProgramFixture(pod)
	cluster["status"] = object{"instances": float64(1), "readyInstances": float64(1), "currentPrimary": "restore-1", "targetPrimary": "restore-1", "conditions": []any{object{"type": "Ready", "status": "True"}}}
	name := "pvc-" + uid(2)
	const allocation = "5368709120" // The source fixture requests 5 GiB.
	pv := object{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": object{"name": name, "uid": uid(3), "resourceVersion": "10"}, "spec": object{"claimRef": object{"name": "restore-1", "namespace": ns, "uid": uid(2)}, "storageClassName": "longhorn-v2", "volumeMode": "Filesystem", "accessModes": []any{"ReadWriteOnce"}, "capacity": object{"storage": p.record.Plan.Source.Size}, "csi": object{"driver": "driver.longhorn.io", "volumeHandle": name}}, "status": object{"phase": "Bound"}}
	value(pv, "spec").(object)["persistentVolumeReclaimPolicy"] = "Retain"
	volume := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "metadata": object{"name": name, "namespace": "longhorn-system", "uid": uid(4), "resourceVersion": "10"}, "spec": object{"dataEngine": "v2", "size": "10737418240", "numberOfReplicas": float64(3)}, "status": object{"state": "attached", "robustness": "healthy", "currentNodeID": hostname, "kubernetesStatus": object{"pvName": name, "pvcName": "restore-1", "namespace": ns}}}
	value(volume, "spec").(object)["size"] = allocation
	engine := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Engine", "metadata": object{"name": name + "-e-0", "namespace": "longhorn-system", "uid": uid(5), "resourceVersion": "10"}, "spec": object{"dataEngine": "v2", "volumeName": name, "active": true, "nodeID": hostname}, "status": object{"currentState": "running", "replicaModeMap": object{}}}
	owner := func() []any {
		return []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": name, "uid": uid(4)}}
	}
	value(engine, "metadata").(object)["ownerReferences"] = owner()
	var replicas []object
	for i, disk := range p.record.Storage.Disks {
		rname := fmt.Sprintf("%s-r-%d", name, i)
		replicas = append(replicas, object{"apiVersion": "longhorn.io/v1beta2", "kind": "Replica", "metadata": object{"name": rname, "namespace": "longhorn-system", "uid": uid(i + 6), "resourceVersion": "10"}, "spec": object{"dataEngine": "v2", "volumeName": name, "engineName": text(engine, "metadata", "name"), "active": true, "nodeID": disk.Node, "diskID": disk.DiskUUID, "volumeSize": "10737418240", "healthyAt": "2026-11-12T00:00:00Z"}, "status": object{"currentState": "running", "instanceManagerName": disk.InstanceManager}})
		value(engine, "status", "replicaModeMap").(object)[rname] = "RW"
		value(replicas[len(replicas)-1], "metadata").(object)["ownerReferences"] = owner()
		value(replicas[len(replicas)-1], "spec").(object)["volumeSize"] = allocation
	}
	return recoveredStorageFixture{p, cluster, pod, claim, pv, volume, engine, replicas}
}

func (f recoveredStorageFixture) bind() (cnpgRecoveredStorage, error) {
	return bindCNPGRecoveredStorage(f.p.record, f.cluster, f.pod, f.claim, f.pv, f.volume, f.engine, f.replicas)
}

func (f recoveredStorageFixture) copyBindingObjects(t *testing.T) recoveredStorageFixture {
	t.Helper()
	copyObject := func(original object) object {
		t.Helper()
		copied, err := cloneRecoverySpec(original)
		if err != nil {
			t.Fatal(err)
		}
		return copied
	}
	f.cluster = copyObject(f.cluster)
	f.pod = copyObject(f.pod)
	f.claim = copyObject(f.claim)
	f.pv = copyObject(f.pv)
	f.volume = copyObject(f.volume)
	f.engine = copyObject(f.engine)
	replicas := make([]object, len(f.replicas))
	for index, replica := range f.replicas {
		replicas[index] = copyObject(replica)
	}
	f.replicas = replicas
	return f
}

func TestRecoveredDatabaseStorageBindsTheCreatedGraphAndThreeCopies(t *testing.T) {
	f := newRecoveredStorageFixture(t)
	got, err := f.bind()
	if err != nil || got.Cluster != f.p.record.Resources["Cluster"] || got.Pod != benchmarkIdentity(f.pod) || got.Claim != benchmarkIdentity(f.claim) || got.PV != benchmarkIdentity(f.pv) || got.Volume != benchmarkIdentity(f.volume) || got.Engine != benchmarkIdentity(f.engine) || len(got.Replicas) != 3 {
		t.Fatalf("created recovered storage graph did not qualify: %+v %v", got, err)
	}
	if f.p.record.RestoreVerified || f.p.record.Cleaned || f.p.record.Phase != "cluster-created" {
		t.Fatal("a storage binding advanced restore or cleanup acceptance")
	}
}

func TestRecoveredCopiesAllowMultipleQualifiedDisksPerHost(t *testing.T) {
	f := newRecoveredStorageFixture(t)
	disks := f.p.record.Storage.Disks
	extra := disks[0]
	extra.DiskUUID = "alternate-qualified-disk"
	disks = append(disks, extra)
	size := text(f.volume, "spec", "size")
	if _, err := recoveredNativeCopies(disks, f.volume, f.engine, f.replicas, size); err != nil {
		t.Fatalf("an extra qualified disk must leave existing three-host placement valid: %v", err)
	}
	spec := valueObject(f.replicas[0], "spec")
	spec["diskID"] = extra.DiskUUID
	if _, err := recoveredNativeCopies(disks, f.volume, f.engine, f.replicas, size); err != nil {
		t.Fatalf("a replica on the alternate disk still spans three hosts: %v", err)
	}
	spec = valueObject(f.replicas[1], "spec")
	spec["diskID"], spec["nodeID"] = disks[0].DiskUUID, disks[0].Node
	valueObject(f.replicas[1], "status")["instanceManagerName"] = disks[0].InstanceManager
	if _, err := recoveredNativeCopies(disks, f.volume, f.engine, f.replicas, size); err == nil {
		t.Fatal("two replicas on different disks of the same host must fail placement")
	}
	spec["diskID"], spec["nodeID"] = disks[1].DiskUUID, disks[1].Node
	valueObject(f.replicas[1], "status")["instanceManagerName"] = disks[1].InstanceManager
	disks[len(disks)-1].NodeUID = "different-host-identity"
	if _, err := recoveredNativeCopies(disks, f.volume, f.engine, f.replicas, size); err == nil {
		t.Fatal("contradictory host identities must fail inventory validation")
	}
}

func TestRecoveredDatabaseStorageRechecksTheRetainedArchiveCopy(t *testing.T) {
	f := newRecoveredStorageFixture(t)
	f.p.record = recoveredArchiveReceiptFixture(t)
	f.p.record.Phase = cnpgArchiveCredentialRetired
	if _, err := f.bind(); err != nil {
		t.Fatalf("verified retained archive copy did not qualify: %v", err)
	}
	f.p.record.RestoreReceipt.Consumer.UID = "67891234-1111-4111-8111-111111111111"
	if _, err := f.bind(); err == nil {
		t.Fatal("a different restore Pod must not stand in for the verified archive copy")
	}
}

func TestRecoveredDatabaseStorageRefusesSubstitutionAndPartialCopies(t *testing.T) {
	baseline := newRecoveredStorageFixture(t)
	for _, scenario := range []string{"cluster replaced", "changed admission", "foreign pod", "pod restart image", "wrong host", "foreign claim", "claim replaced", "wrong driver", "wrong handle", "V1 backend", "unhealthy backend", "missing copy", "duplicate disk", "stopped copy", "unhealthy copy", "copy rebuilding", "wrong manager", "shadowed data", "different mount", "extra claim mount", "foreign engine", "foreign replica", "unretained PV", "ephemeral claim", "contradictory owner", "contradictory engine"} {
		t.Run(scenario, func(t *testing.T) {
			f := baseline.copyBindingObjects(t)
			switch scenario {
			case "cluster replaced":
				f.cluster["metadata"].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			case "changed admission":
				value(f.cluster, "spec").(object)["enableSuperuserAccess"] = true
			case "foreign pod":
				value(f.pod, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "foreign"
			case "pod restart image":
				value(f.pod, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "sha256:" + strings.Repeat("f", 64)
			case "wrong host":
				value(f.pod, "spec").(object)["nodeName"] = "different-node"
			case "foreign claim":
				value(f.claim, "metadata").(object)["namespace"] = "umami"
			case "claim replaced":
				value(f.claim, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			case "wrong driver":
				value(f.pv, "spec", "csi").(object)["driver"] = "csi.hetzner.cloud"
			case "wrong handle":
				value(f.pv, "spec", "csi").(object)["volumeHandle"] = "source-volume"
			case "V1 backend":
				value(f.volume, "spec").(object)["dataEngine"] = "v1"
			case "unhealthy backend":
				value(f.volume, "status").(object)["robustness"] = "degraded"
			case "missing copy":
				f.replicas = f.replicas[:2]
			case "duplicate disk":
				value(f.replicas[1], "spec").(object)["diskID"] = text(f.replicas[0], "spec", "diskID")
			case "stopped copy":
				value(f.replicas[0], "status").(object)["currentState"] = "stopped"
			case "unhealthy copy":
				value(f.replicas[0], "spec").(object)["failedAt"] = "2026-11-12T00:00:00Z"
			case "copy rebuilding":
				value(f.engine, "status", "replicaModeMap").(object)[text(f.replicas[0], "metadata", "name")] = "WO"
			case "wrong manager":
				value(f.replicas[0], "status").(object)["instanceManagerName"] = "foreign-manager"
			case "shadowed data":
				c := value(f.pod, "spec", "containers").([]any)[0].(object)
				c["volumeMounts"] = append(c["volumeMounts"].([]any), object{"name": "other", "mountPath": "/var/lib/postgresql/data/pgdata"})
			case "different mount":
				value(f.pod, "spec", "containers").([]any)[0].(object)["volumeMounts"].([]any)[0].(object)["mountPath"] = "/data"
			case "extra claim mount":
				value(f.pod, "spec").(object)["volumes"] = append(value(f.pod, "spec", "volumes").([]any), object{"name": "foreign", "persistentVolumeClaim": object{"claimName": "source"}})
			case "foreign engine":
				value(f.engine, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			case "foreign replica":
				value(f.replicas[0], "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "99999999-9999-4999-8999-999999999999"
			case "unretained PV":
				value(f.pv, "spec").(object)["persistentVolumeReclaimPolicy"] = "Delete"
			case "ephemeral claim":
				value(f.pod, "spec").(object)["volumes"] = append(value(f.pod, "spec", "volumes").([]any), object{"name": "extra", "ephemeral": object{"volumeClaimTemplate": object{"spec": object{"accessModes": []any{"ReadWriteOnce"}, "storageClassName": "longhorn", "resources": object{"requests": object{"storage": "1Gi"}}}}}})
				container := value(f.pod, "spec", "containers").([]any)[0].(object)
				container["volumeMounts"] = append(container["volumeMounts"].([]any), object{"name": "extra", "mountPath": "/scratch"})
			case "contradictory owner", "contradictory engine":
				extra, err := cloneRecoverySpec(f.replicas[0])
				if err != nil {
					t.Fatal(err)
				}
				value(extra, "metadata").(object)["name"] = "contradictory-copy"
				value(extra, "metadata").(object)["uid"] = "99999999-9999-4999-8999-999999999999"
				value(extra, "spec").(object)["volumeName"] = "other-volume"
				if scenario == "contradictory owner" {
					value(extra, "spec").(object)["engineName"] = "other-engine"
				} else {
					ref := value(extra, "metadata", "ownerReferences").([]any)[0].(object)
					ref["name"], ref["uid"] = "other-volume", "88888888-8888-4888-8888-888888888888"
				}
				f.replicas = append(f.replicas, extra)
			}
			if got, err := f.bind(); err == nil || len(got.Replicas) != 0 {
				t.Fatal("an unqualified recovered storage graph produced a witness")
			}
		})
	}
}
