package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// servingFixture is the production shape since the encrypted-storage cutover:
// the data volume is the encrypted claim and the original claim stays mounted
// read-only as the rollback.
func servingFixture() (object, object, object) {
	pod := object{"metadata": object{"name": "private-mail-0", "namespace": "private-mail", "uid": "serving-pod-uid", "labels": object{"statefulset.kubernetes.io/pod-name": "private-mail-0"}, "ownerReferences": []any{object{"kind": "StatefulSet", "name": "private-mail", "uid": "statefulset-uid", "controller": true}}}, "spec": object{"containers": []any{object{"name": "private-mail", "image": fixtureImage}}, "volumes": []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": "email-data-encrypted"}}, object{"name": "legacy-data", "persistentVolumeClaim": object{"claimName": "private-mail-data", "readOnly": true}}}}, "status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}}}
	child(pod, "status")["containerStatuses"] = []any{object{"name": "private-mail", "imageID": "runtime-mail", "ready": true, "restartCount": float64(0)}}
	pvc := object{"metadata": object{"name": "email-data-encrypted", "namespace": "private-mail", "uid": "serving-claim-uid"}, "spec": object{"volumeName": "serving-volume", "storageClassName": "email-encrypted", "resources": object{"requests": object{"storage": "2Gi"}}}, "status": object{"phase": "Bound"}}
	pv := object{"metadata": object{"name": "serving-volume", "uid": "serving-pv-uid"}, "spec": object{"claimRef": object{"namespace": "private-mail", "name": "email-data-encrypted", "uid": "serving-claim-uid"}, "persistentVolumeReclaimPolicy": "Retain", "csi": object{"driver": "driver.longhorn.io", "volumeHandle": "serving-volume"}}, "status": object{"phase": "Bound"}}
	return pod, pvc, pv
}

// The store moved to the encrypted claim on the cutover. The pre-cutover shape,
// a read-only data mount and the rollback claim are not the serving store.
func TestTheEncryptedClaimIsTheServingSource(t *testing.T) {
	pod, pvc, pv := servingFixture()
	if _, err := proveServingSource(pod, pvc, pv, fixtureImage); err != nil {
		t.Fatal("the encrypted serving store was refused:", err)
	}
	mutations := map[string]func(object, object, object){
		"pre-cutover-claim": func(p, c, v object) {
			child(items(child(p, "spec"), "volumes")[0], "persistentVolumeClaim")["claimName"] = "private-mail-data"
			child(c, "metadata")["name"] = "private-mail-data"
			child(c, "spec")["storageClassName"] = "longhorn-wffc"
			child(child(v, "spec"), "claimRef")["name"] = "private-mail-data"
		},
		"unencrypted-class": func(_, c, _ object) { child(c, "spec")["storageClassName"] = "longhorn-wffc" },
		"rollback-as-data": func(p, _, _ object) {
			volumes := child(p, "spec")["volumes"].([]any)
			volumes[0], volumes[1] = object{"name": "data", "persistentVolumeClaim": object{"claimName": "private-mail-data", "readOnly": true}}, object{"name": "legacy-data", "persistentVolumeClaim": object{"claimName": "email-data-encrypted"}}
		},
		"read-only-data": func(p, _, _ object) {
			child(items(child(p, "spec"), "volumes")[0], "persistentVolumeClaim")["readOnly"] = true
		},
		"volume-of-another-claim": func(_, _, v object) { child(child(v, "spec"), "claimRef")["name"] = "private-mail-data" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p, c, v := servingFixture()
			mutate(p, c, v)
			if _, err := proveServingSource(p, c, v, fixtureImage); err == nil {
				t.Fatal("a store other than the encrypted serving claim was accepted")
			}
		})
	}
}

func proxyServingFixture() (object, object, object) {
	p, c, v := servingFixture()
	child(p, "spec")["containers"] = append(child(p, "spec")["containers"].([]any), object{"name": "client-proxy", "image": fixtureImage})
	child(p, "status")["containerStatuses"] = append(child(p, "status")["containerStatuses"].([]any), object{"name": "client-proxy", "imageID": "runtime-proxy", "ready": true, "restartCount": float64(0)})
	return p, c, v
}

func TestReviewedProxyProfilePreservesServingIdentity(t *testing.T) {
	p, c, v := proxyServingFixture()
	original, err := proveServingSource(p, c, v, fixtureImage)
	if err != nil {
		t.Fatal("reviewed proxy profile refused:", err)
	}
	for _, index := range []int{0, 1} {
		container := items(child(p, "spec"), "containers")[index]
		container["image"] = "unreviewed:latest"
		if _, err := proveServingSource(p, c, v, fixtureImage); err == nil {
			t.Fatal("unreviewed serving component accepted")
		}
		container["image"] = fixtureImage
	}
	for _, key := range []string{"containers", "containerStatuses"} {
		parent := child(p, "spec")
		if key == "containerStatuses" {
			parent = child(p, "status")
		}
		values := parent[key].([]any)
		values[0], values[1] = values[1], values[0]
	}
	reordered, err := proveServingSource(p, c, v, fixtureImage)
	if err != nil || reordered != original {
		t.Fatal("API list order changed serving identity")
	}
	for _, key := range []string{"imageID", "restartCount"} {
		status := items(child(p, "status"), "containerStatuses")[0]
		before := status[key]
		if key == "imageID" {
			status[key] = "replaced-runtime"
		} else {
			status[key] = float64(1)
		}
		changed, err := proveServingSource(p, c, v, fixtureImage)
		if err != nil || changed == original {
			t.Fatalf("%s drift was not bound to the source", key)
		}
		status[key] = before
	}
}

func TestUnreviewedOrUnreadyServingProfilesRefuseRecovery(t *testing.T) {
	mutations := map[string]func(object){
		"unknown-container": func(p object) {
			items(child(p, "spec"), "containers")[0]["name"] = "unknown"
			items(child(p, "status"), "containerStatuses")[0]["name"] = "unknown"
		},
		"duplicate-container": func(p object) {
			child(p, "spec")["containers"].([]any)[1] = items(child(p, "spec"), "containers")[0]
		},
		"missing-main": func(p object) {
			child(p, "spec")["containers"] = child(p, "spec")["containers"].([]any)[1:]
			child(p, "status")["containerStatuses"] = child(p, "status")["containerStatuses"].([]any)[1:]
		},
		"duplicate-status": func(p object) {
			child(p, "status")["containerStatuses"].([]any)[1] = items(child(p, "status"), "containerStatuses")[0]
		},
		"missing-status":     func(p object) { delete(child(p, "status"), "containerStatuses") },
		"unknown-status":     func(p object) { items(child(p, "status"), "containerStatuses")[0]["name"] = "unknown" },
		"unready-status":     func(p object) { items(child(p, "status"), "containerStatuses")[0]["ready"] = false },
		"missing-runtime":    func(p object) { delete(items(child(p, "status"), "containerStatuses")[0], "imageID") },
		"negative-restart":   func(p object) { items(child(p, "status"), "containerStatuses")[0]["restartCount"] = float64(-1) },
		"malformed-restart":  func(p object) { items(child(p, "status"), "containerStatuses")[0]["restartCount"] = "0" },
		"fractional-restart": func(p object) { items(child(p, "status"), "containerStatuses")[0]["restartCount"] = 0.5 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p, c, v := proxyServingFixture()
			mutate(p)
			if _, err := proveServingSource(p, c, v, fixtureImage); err == nil {
				t.Fatal("unsafe serving profile accepted")
			}
		})
	}
}

// This opt-in evaluates only fixed source reads, without invoking the recovery
// lifecycle or obtaining workflow mutation authority.
func TestServingSourceLiveReadOnly(t *testing.T) {
	contextName := os.Getenv("EMAIL_RECOVERY_READONLY_SOURCE_CONTEXT")
	if contextName == "" {
		t.Skip("requires an explicitly selected existing read-only context")
	}
	t.Chdir("../..")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	d := recoveryDriver{k: kubectl{context: contextName}}
	first, err := d.readSource(ctx)
	if err != nil {
		t.Fatal("live source refused:", err)
	}
	d.source = first
	if err := d.sourceRetained(ctx); err != nil {
		t.Fatal("live source changed:", err)
	}
}
func TestServingSourceIsBoundAcrossPodClaimAndBackend(t *testing.T) {
	pod, pvc, pv := servingFixture()
	source, err := proveServingSource(pod, pvc, pv, fixtureImage)
	if err != nil || source.podUID != "serving-pod-uid" || source.claimUID != "serving-claim-uid" || source.pv != "serving-volume" || source.pvUID != "serving-pv-uid" || source.reclaim != "Retain" {
		t.Fatal("serving source identity was not preserved")
	}
}
func TestSourceReplacementHookOrWrongClaimRefusesBackup(t *testing.T) {
	mutations := map[string]func(object, object, object){
		"claim-ref": func(_, _, v object) { child(child(v, "spec"), "claimRef")["uid"] = "another-claim" },
		"wrong-pod-volume": func(p, _, _ object) {
			child(items(child(p, "spec"), "volumes")[0], "persistentVolumeClaim")["claimName"] = "another-claim"
		},
		"unready":     func(p, _, _ object) { items(child(p, "status"), "conditions")[0]["status"] = "False" },
		"wrong-image": func(p, _, _ object) { items(child(p, "spec"), "containers")[0]["image"] = "unreviewed:latest" },
		"hook": func(p, _, _ object) {
			child(p, "metadata")["annotations"] = object{"pre.hook.backup.velero.io/command": "unsafe-command"}
		},
		"deleting": func(_, c, _ object) { child(c, "metadata")["deletionTimestamp"] = "2026-10-29T10:00:00Z" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p, c, v := servingFixture()
			mutate(p, c, v)
			if _, err := proveServingSource(p, c, v, fixtureImage); err == nil {
				t.Fatal("unsafe or mismatched source was accepted")
			}
		})
	}
}

func TestLiveSourceMustMatchTheReviewedRuntimeImage(t *testing.T) {
	pod, pvc, pv := servingFixture()
	if _, err := proveServingSource(pod, pvc, pv, ""); err == nil {
		t.Fatal("a missing deployment image fact was accepted")
	}
	if _, err := proveServingSource(pod, pvc, pv, "ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:2222222222222222222222222222222222222222222222222222222222222222"); err == nil {
		t.Fatal("a live image different from the reviewed declaration was accepted")
	}
}
