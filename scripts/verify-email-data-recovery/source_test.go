package main

import "testing"

func servingFixture() (object, object, object) {
	pod := object{"metadata": object{"name": "private-mail-0", "namespace": "private-mail", "uid": "serving-pod-uid", "labels": object{"statefulset.kubernetes.io/pod-name": "private-mail-0"}, "ownerReferences": []any{object{"kind": "StatefulSet", "name": "private-mail", "uid": "statefulset-uid", "controller": true}}}, "spec": object{"containers": []any{object{"name": "private-mail", "image": recoveryImage}}, "volumes": []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": "private-mail-data"}}}}, "status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}}}
	pvc := object{"metadata": object{"name": "private-mail-data", "namespace": "private-mail", "uid": "serving-claim-uid"}, "spec": object{"volumeName": "serving-volume", "storageClassName": "longhorn-wffc", "resources": object{"requests": object{"storage": "2Gi"}}}, "status": object{"phase": "Bound"}}
	pv := object{"metadata": object{"name": "serving-volume", "uid": "serving-pv-uid"}, "spec": object{"claimRef": object{"namespace": "private-mail", "name": "private-mail-data", "uid": "serving-claim-uid"}, "persistentVolumeReclaimPolicy": "Delete", "csi": object{"driver": "driver.longhorn.io", "volumeHandle": "serving-volume"}}, "status": object{"phase": "Bound"}}
	return pod, pvc, pv
}
func TestServingSourceIsBoundAcrossPodClaimAndBackend(t *testing.T) {
	pod, pvc, pv := servingFixture()
	source, err := proveServingSource(pod, pvc, pv)
	if err != nil || source.podUID != "serving-pod-uid" || source.claimUID != "serving-claim-uid" || source.pv != "serving-volume" || source.pvUID != "serving-pv-uid" || source.reclaim != "Delete" {
		t.Fatal("serving source identity was not preserved")
	}
}
func TestSourceReplacementHookOrWrongClaimRefusesBackup(t *testing.T) {
	mutations := map[string]func(object, object, object){
		"claim-ref": func(p, c, v object) { child(child(v, "spec"), "claimRef")["uid"] = "another-claim" },
		"wrong-pod-volume": func(p, c, v object) {
			child(items(child(p, "spec"), "volumes")[0], "persistentVolumeClaim")["claimName"] = "another-claim"
		},
		"unready":     func(p, c, v object) { items(child(p, "status"), "conditions")[0]["status"] = "False" },
		"wrong-image": func(p, c, v object) { items(child(p, "spec"), "containers")[0]["image"] = "unreviewed:latest" },
		"hook": func(p, c, v object) {
			child(p, "metadata")["annotations"] = object{"pre.hook.backup.velero.io/command": "unsafe-command"}
		},
		"deleting": func(p, c, v object) { child(c, "metadata")["deletionTimestamp"] = "2026-10-29T10:00:00Z" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p, c, v := servingFixture()
			mutate(p, c, v)
			if _, err := proveServingSource(p, c, v); err == nil {
				t.Fatal("unsafe or mismatched source was accepted")
			}
		})
	}
}
