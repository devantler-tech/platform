package main

import "testing"

func TestNativeRestoreReclaimRequiresTheOriginalBoundClaimAndCSIFinalizer(t *testing.T) {
	for _, fault := range []string{"", "wrong PV UID", "changed claim", "wrong policy", "no CSI finalizer", "unbound", "changed capacity"} {
		t.Run(fault, func(t *testing.T) {
			_, _, _, before := nativeRestoreDetachedFixture(t)
			_, _, _, after := nativeRestoreDetachedFixture(t)
			after.PV["status"] = object{"phase": "Bound"}
			valueObject(after.PV, "spec")["persistentVolumeReclaimPolicy"] = "Delete"
			valueObject(after.PV, "metadata")["finalizers"] = []any{csiReclaimFinalizer}
			switch fault {
			case "wrong PV UID":
				valueObject(after.PV, "metadata")["uid"] = "different"
			case "changed claim":
				valueObject(after.Claim, "metadata")["resourceVersion"] = "different"
			case "wrong policy":
				valueObject(after.PV, "spec")["persistentVolumeReclaimPolicy"] = "Retain"
			case "no CSI finalizer":
				valueObject(after.PV, "metadata")["finalizers"] = []any{"kubernetes.io/pv-protection"}
			case "unbound":
				valueObject(after.PV, "status")["phase"] = "Released"
			case "changed capacity":
				valueObject(after.PV, "spec", "capacity")["storage"] = "8Gi"
			}
			if got := nativeRestoreCSIReclaimReady(before, after); got != (fault == "") {
				t.Fatalf("reclaim readiness for %q: %v", fault, got)
			}
		})
	}
}

func TestNativeRestoreReclaimBindsTheDetachedClaimAndExclusiveBackend(t *testing.T) {
	for _, fault := range []string{
		"", "unfinished", "live pod", "backend still attached", "missing PV inventory",
		"missing claim inventory", "PV changed", "claim changed", "PV already released",
		"foreign PV references backend", "foreign claim references PV", "another scratch claim",
	} {
		t.Run(fault, func(t *testing.T) {
			record, seed, _, detached := nativeRestoreDetachedFixture(t)
			_, _, state := nativeRestoreStateFixture(t, "completed")
			detached.PV["status"] = object{"phase": "Bound"}
			pvs := []object{detached.PV}
			claims := []object{detached.Claim}
			switch fault {
			case "unfinished":
				state.Phase, state.CompletedAt = "reattach-verified", ""
			case "live pod":
				pod := state.Resources["SecondPod"]
				detached.Inventory.pods = []object{{"metadata": object{"name": pod.Name, "namespace": pod.Namespace, "uid": pod.UID}}}
			case "backend still attached":
				valueObject(detached.Inventory.volumes[0], "status")["state"] = "attached"
			case "missing PV inventory":
				pvs = nil
			case "missing claim inventory":
				claims = []object{}
			case "PV changed":
				pvs[0] = object{"metadata": object{"name": state.Resources["PersistentVolume"].Name, "uid": "replacement-pv", "resourceVersion": "10"}}
			case "claim changed":
				claims[0] = object{"metadata": object{"name": "fio-data", "namespace": benchmarkNamespace, "uid": "replacement", "resourceVersion": "10"}}
			case "PV already released":
				detached.PV["status"] = object{"phase": "Released"}
			case "foreign PV references backend":
				pvs = append(pvs, object{"metadata": object{"name": "other-pv", "uid": "other-pv-id", "resourceVersion": "10"},
					"spec": object{"csi": object{"driver": "driver.longhorn.io", "volumeHandle": state.Resources["LonghornVolume"].Name}}})
			case "foreign claim references PV", "another scratch claim":
				claim := object{"metadata": object{"name": "other-claim", "namespace": "elsewhere", "uid": "other-claim-id", "resourceVersion": "10"}}
				if fault == "foreign claim references PV" {
					claim["spec"] = object{"volumeName": state.Resources["PersistentVolume"].Name}
				} else {
					valueObject(claim, "metadata")["namespace"] = benchmarkNamespace
				}
				claims = append(claims, claim)
			}
			err := verifyNativeRestoreReclaim(record, seed, state, detached, pvs, claims)
			if (err != nil) != (fault != "") {
				t.Fatalf("restore reclaim verdict for %q: %v", fault, err)
			}
		})
	}
}
