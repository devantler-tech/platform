package main

import "testing"

func csiScratchFixture(kind string) (object, object, object, object, object, string) {
	_, _, _, op := csiTransferFixture("Backup")
	op["kind"] = kind
	claim, pv, backend, source := resource("transfer"), resource("temporary-pv"), resource("temporary-backend"), resource("source-backend")
	claim["metadata"].(object)["namespace"] = "velero"
	claim["metadata"].(object)["ownerReferences"] = []any{object{"apiVersion": "velero.io/v2alpha1", "kind": kind, "name": text(op, "metadata", "name"), "uid": text(op, "metadata", "uid"), "controller": true}}
	class, engine := "lh-canary-v1-1234", "v1"
	if kind == "DataDownload" {
		class, engine = "lh-canary-v2-1234", "v2"
	}
	claim["spec"] = object{"volumeName": text(pv, "metadata", "name"), "storageClassName": class}
	pv["spec"] = object{"claimRef": object{"name": text(claim, "metadata", "name"), "namespace": "velero", "uid": text(claim, "metadata", "uid")}, "storageClassName": class, "persistentVolumeReclaimPolicy": "Retain", "csi": object{"driver": "driver.longhorn.io", "volumeHandle": text(backend, "metadata", "name")}}
	backend["metadata"].(object)["namespace"] = "longhorn-system"
	backend["spec"] = object{"dataEngine": engine}
	if kind == "DataUpload" {
		backend["spec"].(object)["dataSource"] = "snap://source-backend/native-snapshot"
	}
	return claim, pv, backend, op, source, class
}

func TestCSIScratchProofRequiresTheObservedOwnedClaimAndExactBackend(t *testing.T) {
	for _, kind := range []string{"DataUpload", "DataDownload"} {
		claim, pv, volume, op, source, class := csiScratchFixture(kind)
		proof, err := verifyCSIScratchBinding(claim, pv, volume, op, source, class)
		if err != nil || proof.Claim.UID == "" || proof.PV.UID == "" || proof.Volume.UID == "" {
			t.Fatalf("valid owned scratch rejected: %v", err)
		}
		for name, change := range map[string]func(object, object, object){
			"claim belongs to another operation": func(c, _, _ object) { value(c, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "foreign" },
			"PV rebound to another claim":        func(_, p, _ object) { value(p, "spec", "claimRef").(object)["uid"] = "replacement" },
			"backend replaced":                   func(_, _, v object) { v["metadata"].(object)["name"] = "other-backend" },
			"another storage driver":             func(_, p, _ object) { value(p, "spec", "csi").(object)["driver"] = "another-driver" },
			"wrong data engine":                  func(_, _, v object) { v["spec"].(object)["dataEngine"] = "other" },
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				c, p, v, o, s, sc := csiScratchFixture(kind)
				change(c, p, v)
				if _, err := verifyCSIScratchBinding(c, p, v, o, s, sc); err == nil {
					t.Fatal("unowned temporary storage accepted")
				}
			})
		}
	}
	c, p, v, o, s, sc := csiScratchFixture("DataUpload")
	v["spec"].(object)["dataSource"] = "snap://original-historical-volume/do-not-delete"
	if _, err := verifyCSIScratchBinding(c, p, v, o, s, sc); err == nil {
		t.Fatal("a snapshot of another source volume was accepted")
	}
}
