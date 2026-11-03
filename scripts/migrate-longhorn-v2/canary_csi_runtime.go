package main

import (
	"context"
	"errors"
	"strings"
)

// canaryJournalName separates filesystem and CSI proof ownership at fixed names.
func canaryJournalName(transport string) (string, error) {
	switch transport {
	case "filesystem":
		return "longhorn-v2-canary", nil
	case "csi":
		return "longhorn-v2-csi-canary", nil
	default:
		return "", errors.New("canary transport is outside the reviewed procedure")
	}
}

// verifyCSIRestoredBinding requires the destination claim to adopt the exact
// independent backend downloaded by the native data mover.
func verifyCSIRestoredBinding(proof csiScratchEvidence, claim, pv, volume object) error {
	if proof.OperationUID == "" || proof.Claim.UID == "" || proof.PV.UID == "" || proof.Volume.UID == "" ||
		text(claim, "metadata", "namespace") == "velero" || text(claim, "metadata", "uid") == "" || text(claim, "metadata", "uid") == proof.Claim.UID ||
		text(pv, "metadata", "name") != proof.PV.Name || text(pv, "metadata", "uid") != proof.PV.UID ||
		text(volume, "metadata", "name") != proof.Volume.Name || text(volume, "metadata", "uid") != proof.Volume.UID {
		return errors.New("the restored claim did not adopt the exact downloaded PV and independent backend")
	}
	return nil
}

// transferCSI joins the completed native operation to its snapshot class,
// source claim, captured scratch storage and parent backup or restore.
func (p *canaryRun) transferCSI(ctx context.Context, desired object, expectedSnapshot string) (object, string, error) {
	kind := text(desired, "kind")
	query := "datauploads.velero.io"
	if kind == "Restore" {
		query = "datadownloads.velero.io"
	}
	created, err := p.create(ctx, kind, desired)
	if err != nil {
		return nil, "", err
	}
	var parent object
	var snapshot string
	err = waitReady(ctx, func(ctx context.Context) error {
		if err = p.csi.health(); err != nil {
			return err
		}
		parent, err = p.c.get(ctx, strings.ToLower(kind)+"s.velero.io", "velero", text(created, "metadata", "name"), false)
		if err != nil || text(parent, "metadata", "uid") != text(created, "metadata", "uid") {
			return errors.New("CSI backup or restore identity changed")
		}
		if err = cleanVeleroResult(parent); err != nil {
			return err
		}
		class, readErr := p.c.get(ctx, "volumesnapshotclasses.snapshot.storage.k8s.io", "", text(p.snapshotClass, "metadata", "name"), false)
		if readErr != nil || text(class, "metadata", "uid") != text(p.snapshotClass, "metadata", "uid") || text(class, "driver") != "driver.longhorn.io" || text(class, "deletionPolicy") != "Delete" || text(class, "parameters", "type") != "snap" {
			return errors.New("the disposable CSI snapshot class changed")
		}
		sourceClaim, readErr := p.c.get(ctx, "persistentvolumeclaim", text(p.sourceClaim, "metadata", "namespace"), text(p.sourceClaim, "metadata", "name"), false)
		if readErr != nil || text(sourceClaim, "metadata", "uid") != text(p.sourceClaim, "metadata", "uid") {
			return errors.New("the CSI source claim was replaced")
		}
		var targetClaim object
		if kind == "Restore" {
			targetClaim, readErr = p.c.get(ctx, "persistentvolumeclaim", "longhorn-canary-v2-"+p.run, "fio-data", false)
			if readErr != nil {
				return readErr
			}
		}
		operations, readErr := p.c.list(ctx, query, "velero")
		if readErr != nil {
			return readErr
		}
		op, snap, verifyErr := verifyCSITransfer(operations, parent, sourceClaim, targetClaim, expectedSnapshot, text(sourceClaim, "spec", "storageClassName"), text(class, "metadata", "name"))
		if verifyErr != nil {
			return verifyErr
		}
		storageClass := text(sourceClaim, "spec", "storageClassName")
		if kind == "Restore" {
			storageClass = text(targetClaim, "spec", "storageClassName")
		}
		proof, verifyErr := p.csi.proof(op, p.sourceVolume, storageClass)
		if verifyErr != nil {
			return verifyErr
		}
		opKind := text(op, "kind")
		if old := p.record.CSIScratch[opKind]; old.OperationUID != "" && old != proof {
			return errors.New("the observed CSI operation storage graph changed")
		}
		p.record.CSIScratch[opKind] = proof
		if verifyErr = p.remember(ctx, "csi/"+opKind, op); verifyErr != nil {
			return verifyErr
		}
		snapshot = snap
		return nil
	})
	return parent, snapshot, err
}

// requireCSIGone observes native scratch retirement without adopting or deleting
// a replacement that reused a recorded resource name.
func (p *canaryRun) requireCSIGone(ctx context.Context, kind string, identity benchmarkResource) error {
	if identity.Name == "" || identity.UID == "" {
		return errors.New("CSI cleanup lacks a captured resource identity")
	}
	current, err := p.c.get(ctx, kind, identity.Namespace, identity.Name, true)
	if err != nil {
		return err
	}
	if current != nil {
		if text(current, "metadata", "uid") != identity.UID {
			return errors.New("a CSI scratch name was reused; do not adopt or delete its replacement")
		}
		return errors.New("native CSI scratch cleanup has not completed")
	}
	return nil
}

// waitCSIUploadGone requires native retirement of all captured upload storage
// before beginning download, while keeping the identity streams healthy.
func (p *canaryRun) waitCSIUploadGone(ctx context.Context) error {
	proof := p.record.CSIScratch["DataUpload"]
	return waitReady(ctx, func(ctx context.Context) error {
		if err := p.csi.health(); err != nil {
			return err
		}
		for _, item := range []struct {
			kind     string
			identity benchmarkResource
		}{
			{"persistentvolumeclaim", proof.Claim}, {"persistentvolume", proof.PV}, {"volumes.longhorn.io", proof.Volume},
		} {
			if err := p.requireCSIGone(ctx, item.kind, item.identity); err != nil {
				return err
			}
		}
		return nil
	})
}

// waitCSIArtifactsGone verifies native operation and snapshot cleanup while
// retaining the downloaded backend for its separate owned cleanup.
func (p *canaryRun) waitCSIArtifactsGone(ctx context.Context) error {
	return waitReady(ctx, func(ctx context.Context) error {
		if err := p.csi.health(); err != nil {
			return err
		}
		for _, kind := range []string{"DataUpload", "DataDownload"} {
			if err := p.requireCSIGone(ctx, strings.ToLower(kind)+"s.velero.io", p.record.Resources["csi/"+kind]); err != nil {
				return err
			}
		}
		if err := p.requireCSIGone(ctx, "persistentvolumeclaim", p.record.CSIScratch["DataDownload"].Claim); err != nil {
			return err
		}
		for _, scope := range []struct{ kind, namespace string }{
			{"volumesnapshots.snapshot.storage.k8s.io", "*"}, {"volumesnapshotcontents.snapshot.storage.k8s.io", ""},
		} {
			items, err := p.c.list(ctx, scope.kind, scope.namespace)
			if err != nil {
				return err
			}
			for _, item := range items {
				if text(item, "spec", "volumeSnapshotClassName") == text(p.snapshotClass, "metadata", "name") {
					return errors.New("native CSI snapshot cleanup has not completed")
				}
			}
		}
		return nil
	})
}

// removeCSISnapshotClass conditionally removes only this run's original class.
func (p *canaryRun) removeCSISnapshotClass(ctx context.Context) error {
	current, err := p.c.get(ctx, "volumesnapshotclasses.snapshot.storage.k8s.io", "", text(p.snapshotClass, "metadata", "name"), false)
	if err != nil || text(current, "metadata", "uid") != text(p.snapshotClass, "metadata", "uid") {
		return errors.New("the CSI snapshot class was replaced; preserve it for diagnosis")
	}
	return removeScratch(ctx, p.c, p.source, "volumesnapshotclasses.snapshot.storage.k8s.io", "", "/apis/snapshot.storage.k8s.io/v1/volumesnapshotclasses/"+text(current, "metadata", "name"), current)
}
