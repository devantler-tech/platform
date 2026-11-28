package main

import (
	"fmt"
	"testing"
)

func TestOpenBaoMigrationBindsBothOriginalVolumesToTheSameMember(t *testing.T) {
	for _, fault := range []string{
		"", "stale template", "only one claim", "wrong audit capacity", "shared provider handle",
		"other attachment host", "extra source consumer", "different filesystem mount", "deleting source",
	} {
		t.Run(fault, func(t *testing.T) {
			stateful, pods, claims, pvs, attachments := openBaoProviderStorageFixture()
			switch fault {
			case "stale template":
				valueObject(pods[0], "metadata", "labels")["controller-revision-hash"] = "openbao-old"
			case "only one claim":
				claims = claims[:1]
			case "wrong audit capacity":
				valueObject(claims[1], "spec", "resources", "requests")["storage"] = "10Gi"
			case "shared provider handle":
				valueObject(pvs[1], "spec", "csi")["volumeHandle"] = text(pvs[0], "spec", "csi", "volumeHandle")
			case "other attachment host":
				valueObject(attachments[1], "spec")["nodeName"] = "worker-2"
			case "extra source consumer":
				_, second, _, _, _ := openBaoProviderStorageFixture()
				valueObject(second[0], "metadata")["name"] = "foreign"
				pods = append(pods, second[0])
			case "different filesystem mount":
				containers := pods[0]["spec"].(object)["containers"].([]any)
				mounts := containers[0].(object)["volumeMounts"].([]any)
				mounts[1].(object)["mountPath"] = "/somewhere-else"
			case "deleting source":
				valueObject(claims[1], "metadata")["deletionTimestamp"] = "2026-11-27T14:30:00Z"
			}
			bound, err := bindOpenBaoProviderStorage("openbao-1", stateful, pods, claims, pvs, attachments)
			if fault != "" {
				if err == nil {
					t.Fatal("the source gate accepted an incomplete storage pair")
				}
				return
			}
			if err != nil || bound.Member.Name != "openbao-1" || bound.Node != "worker-1" ||
				bound.Disks[0].ProviderHandle != "101" || bound.Disks[1].ProviderHandle != "102" ||
				bound.Disks[0].Bytes != 10<<30 || bound.Disks[1].Bytes != 40<<30 {
				t.Fatalf("the exact provider storage pair was not proven: %+v, %v", bound, err)
			}
		})
	}
}

func TestOpenBaoCopyRejectsAnOverlappingSourceMount(t *testing.T) {
	for _, tc := range []struct {
		location string
		allowed  bool
	}{
		{location: "/openbao/config", allowed: true},
		{location: "/openbao/data-mirror", allowed: true},
		{location: "/"},
		{location: "/openbao"},
		{location: "/openbao/data"},
		{location: "/openbao/data/shared"},
		{location: "/openbao/audit/../data/shared"},
		{location: "/openbao/audit/logs"},
	} {
		t.Run(tc.location, func(t *testing.T) {
			stateful, pods, claims, pvs, attachments := openBaoProviderStorageFixture()
			container := value(pods[0], "spec", "containers").([]any)[0].(object)
			mounts := value(container, "volumeMounts").([]any)
			container["volumeMounts"] = append(mounts, object{"name": "extra", "mountPath": tc.location})
			_, err := bindOpenBaoProviderStorage("openbao-1", stateful, pods, claims, pvs, attachments)
			if (err == nil) != tc.allowed {
				t.Fatalf("source binding admitted=%t, want %t", err == nil, tc.allowed)
			}
		})
	}
}

func TestOpenBaoPairConfirmationBindsBothDisksAndProtectedRun(t *testing.T) {
	stateful, pods, claims, pvs, attachments := openBaoProviderStorageFixture()
	pair, err := bindOpenBaoProviderStorage("openbao-1", stateful, pods, claims, pvs, attachments)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := openBaoProviderStorageDigest(pair)
	if err != nil || !retentionDigest.MatchString(digest) {
		t.Fatalf("the pair receipt did not produce a valid confirmation: %v", err)
	}
	other := pair
	other.Disks[1].ProviderHandle = "307"
	changed, err := openBaoProviderStorageDigest(other)
	if err != nil || changed == digest {
		t.Fatal("the confirmation did not bind the second provider disk")
	}
	for _, fault := range []string{"", "unknown member", "malformed digest", "later attempt", "worker selected", "database selected", "feature branch"} {
		t.Run(fault, func(t *testing.T) {
			env := dispatchFixture()
			env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"] = openBaoPairOperation, ""
			env["LONGHORN_V2_CONFIRM"] = openBaoPairOperation + ":openbao-1:" + digest
			switch fault {
			case "unknown member":
				env["LONGHORN_V2_CONFIRM"] = openBaoPairOperation + ":openbao-5:" + digest
			case "malformed digest":
				env["LONGHORN_V2_CONFIRM"] = openBaoPairOperation + ":openbao-1:abc"
			case "later attempt":
				env["GITHUB_RUN_ATTEMPT"] = "2"
			case "worker selected":
				env["LONGHORN_V2_NODE"] = "prod-worker-1"
			case "database selected":
				env["LONGHORN_V2_DATABASE"] = "umami-db"
			case "feature branch":
				env["GITHUB_REF"] = "refs/heads/test"
			}
			got, err := authorizeOpenBaoPair(func(key string) string { return env[key] })
			if fault == "" {
				if err != nil || got.Member != "openbao-1" || got.Digest != digest {
					t.Fatalf("the protected pair request was not recognized: %v", err)
				}
			} else if err == nil {
				t.Fatal("an out-of-scope pair request was accepted")
			}
		})
	}
}

func TestOpenBaoOfflineCopyRequiresBothProviderSourcesDetached(t *testing.T) {
	for _, fault := range []string{"", "old pod remains", "volume referenced", "csi attachment remains", "provider still attached",
		"provider omits assignee", "provider has another assignee", "duplicate provider response", "provider owner differs",
		"source binding moved", "retain removed"} {
		t.Run(fault, func(t *testing.T) {
			stateful, livePods, claims, pvs, liveAttachments := openBaoProviderStorageFixture()
			bound, err := bindOpenBaoProviderStorage("openbao-1", stateful, livePods, claims, pvs, liveAttachments)
			if err != nil {
				t.Fatal(err)
			}
			provider := []object{
				{"id": float64(101), "size": float64(10), "status": "available", "server": nil, "assignee": nil},
				{"id": float64(102), "size": float64(40), "status": "available", "assignee": nil},
			}
			for i, disk := range bound.Disks {
				provider[i]["linux_device"] = "/dev/disk/by-id/scsi-0HC_Volume_" + disk.ProviderHandle
				provider[i]["labels"] = object{"managed-by": "csi-driver", "pv-name": disk.PV.Name,
					"pvc-name": disk.PVC.Name, "pvc-namespace": disk.PVC.Namespace}
			}
			pods, attachments := []object{}, []object{}
			switch fault {
			case "old pod remains":
				pods = livePods
			case "volume referenced":
				valueObject(livePods[0], "metadata")["name"] = "other-pod"
				pods = livePods
			case "csi attachment remains":
				attachments = liveAttachments
				valueObject(attachments[0], "status")["attached"] = false
			case "provider still attached":
				provider[1]["server"] = float64(12)
			case "provider omits assignee":
				provider[1]["server"] = nil
				delete(provider[1], "assignee")
			case "provider has another assignee":
				provider[1]["server"] = nil
				provider[1]["assignee"] = object{"type": "other", "id": float64(12)}
			case "duplicate provider response":
				provider = append(provider, provider[1])
			case "provider owner differs":
				valueObject(provider[0], "labels")["pvc-namespace"] = "other"
			case "source binding moved":
				valueObject(pvs[1], "metadata")["uid"] = "b286e4f7-51db-4bc1-aa4d-000000000888"
			case "retain removed":
				valueObject(pvs[1], "spec")["persistentVolumeReclaimPolicy"] = "Delete"
			}
			if err := verifyOpenBaoDetachedSources(bound, claims, pvs, pods, attachments, provider); (err != nil) != (fault != "") {
				t.Fatalf("detachment proof for %q: %v", fault, err)
			}
		})
	}
}

func openBaoProviderStorageFixture() (object, []object, []object, []object, []object) {
	const ns, member, node = "openbao", "openbao-1", "worker-1"
	document := func(api, kind, name, namespace string, suffix int) object {
		return object{"apiVersion": api, "kind": kind, "metadata": object{"name": name, "namespace": namespace,
			"resourceVersion": "11", "uid": fmt.Sprintf("b286e4f7-51db-4bc1-aa4d-%012d", suffix)}}
	}
	st := document("apps/v1", "StatefulSet", "openbao", ns, 1)
	valueObject(st, "metadata")["generation"] = float64(3)
	st["spec"] = object{"replicas": float64(3), "updateStrategy": object{"type": "OnDelete"}}
	st["status"] = object{"observedGeneration": float64(3), "readyReplicas": float64(3), "updatedReplicas": float64(3),
		"updateRevision": "openbao-current"}
	pod := document("v1", "Pod", member, ns, 2)
	valueObject(pod, "metadata")["ownerReferences"] = []any{object{"apiVersion": "apps/v1", "kind": "StatefulSet",
		"name": "openbao", "uid": text(st, "metadata", "uid"), "controller": true}}
	valueObject(pod, "metadata")["labels"] = object{"controller-revision-hash": "openbao-current"}
	pod["status"] = object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}}
	pod["spec"] = object{
		"nodeName":        node,
		"securityContext": object{"runAsUser": float64(100), "runAsGroup": float64(1000), "fsGroup": float64(1000)},
		"volumes": []any{
			object{"name": "data", "persistentVolumeClaim": object{"claimName": "data-" + member}},
			object{"name": "audit", "persistentVolumeClaim": object{"claimName": "audit-" + member}},
		},
		"containers": []any{object{"name": "openbao", "volumeMounts": []any{
			object{"name": "data", "mountPath": "/openbao/data"}, object{"name": "audit", "mountPath": "/openbao/audit"},
		}}},
	}
	var claims, pvs, attachments []object
	for index, disk := range []struct{ name, size string }{{"data", "10Gi"}, {"audit", "40Gi"}} {
		name := disk.name + "-" + member
		claim := document("v1", "PersistentVolumeClaim", name, ns, 10+index)
		pvName := "pvc-" + text(claim, "metadata", "uid")
		claim["spec"] = object{"volumeName": pvName, "volumeMode": "Filesystem", "accessModes": []any{"ReadWriteOnce"},
			"storageClassName": "hcloud", "resources": object{"requests": object{"storage": disk.size}}}
		claim["status"] = object{"phase": "Bound", "capacity": object{"storage": disk.size}}
		pv := document("v1", "PersistentVolume", pvName, "", 20+index)
		pv["spec"] = object{"claimRef": object{"name": name, "namespace": ns, "uid": text(claim, "metadata", "uid")},
			"volumeMode": "Filesystem", "accessModes": []any{"ReadWriteOnce"}, "storageClassName": "hcloud",
			"persistentVolumeReclaimPolicy": "Retain", "capacity": object{"storage": disk.size},
			"csi": object{"driver": hetznerCSIDriver, "volumeHandle": fmt.Sprint(101 + index)}}
		pv["status"] = object{"phase": "Bound"}
		attachment := document("storage.k8s.io/v1", "VolumeAttachment", "csi-"+disk.name, "", 30+index)
		attachment["spec"] = object{"attacher": hetznerCSIDriver, "nodeName": node, "source": object{"persistentVolumeName": pvName}}
		attachment["status"] = object{"attached": true}
		claims, pvs, attachments = append(claims, claim), append(pvs, pv), append(attachments, attachment)
	}
	return st, []object{pod}, claims, pvs, attachments
}
