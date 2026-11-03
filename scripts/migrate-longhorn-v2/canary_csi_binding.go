package main

import (
	"errors"
	"strings"
)

type csiScratchEvidence struct {
	Claim, PV, Volume          benchmarkResource
	OperationUID, NativeSource string
}

// Accepts observed watch objects, including deletion events. These identities
// are proof for later absence checks, never authority to delete by name.
func verifyCSIScratchBinding(claim, pv, volume, operation, sourceVolume object, class string) (csiScratchEvidence, error) {
	proof := csiScratchEvidence{Claim: benchmarkIdentity(claim), PV: benchmarkIdentity(pv), Volume: benchmarkIdentity(volume), OperationUID: text(operation, "metadata", "uid")}
	kind := text(operation, "kind")
	if proof.OperationUID == "" || (kind != "DataUpload" && kind != "DataDownload") || text(operation, "metadata", "namespace") != "velero" || proof.Claim.Namespace != "velero" ||
		proof.Claim.UID == "" || proof.PV.UID == "" || proof.Volume.UID == "" || proof.PV.Namespace != "" || proof.Volume.Namespace != "longhorn-system" || text(sourceVolume, "metadata", "uid") == "" {
		return proof, errors.New("CSI scratch resource identity or operation scope is incomplete")
	}
	owned := false
	refs, _ := value(claim, "metadata", "ownerReferences").([]any)
	for _, raw := range refs {
		r, _ := raw.(object)
		if text(r, "apiVersion") == "velero.io/v2alpha1" && text(r, "kind") == kind && text(r, "name") == text(operation, "metadata", "name") && text(r, "uid") == proof.OperationUID && value(r, "controller") == true {
			owned = true
		}
	}
	policy := text(pv, "spec", "persistentVolumeReclaimPolicy")
	if !owned || class == "" || text(claim, "spec", "storageClassName") != class || text(pv, "spec", "storageClassName") != class || (policy != "Retain" && policy != "Delete") ||
		text(claim, "spec", "volumeName") != proof.PV.Name || text(pv, "spec", "claimRef", "name") != proof.Claim.Name || text(pv, "spec", "claimRef", "namespace") != "velero" || text(pv, "spec", "claimRef", "uid") != proof.Claim.UID ||
		text(pv, "spec", "csi", "driver") != "driver.longhorn.io" || text(pv, "spec", "csi", "volumeHandle") != proof.Volume.Name || proof.Volume.UID == text(sourceVolume, "metadata", "uid") {
		return proof, errors.New("CSI scratch PVC, PV and independent Longhorn backend are not bound to this operation")
	}
	engine := "v1"
	if kind == "DataDownload" {
		engine = "v2"
	}
	if text(volume, "spec", "dataEngine") != engine {
		return proof, errors.New("CSI scratch backend uses the wrong data engine")
	}
	proof.NativeSource = text(volume, "spec", "dataSource")
	if kind == "DataUpload" {
		snapshot, ok := strings.CutPrefix(proof.NativeSource, "snap://"+text(sourceVolume, "metadata", "name")+"/")
		if !ok || !kubeName.MatchString(snapshot) {
			return proof, errors.New("temporary CSI backup clone is not a snapshot of the exact canary source")
		}
	} else if proof.NativeSource != "" {
		return proof, errors.New("CSI recovery target was pre-populated from another data source")
	}
	return proof, nil
}
