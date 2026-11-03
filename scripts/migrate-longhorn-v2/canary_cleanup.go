package main

import (
	"context"
	"errors"
)

// requireNativeRestoreGone observes Velero's own restore retirement and never
// deletes a replacement that reused the name.
func (p *canaryRun) requireNativeRestoreGone(ctx context.Context, restore object) error {
	if text(restore, "metadata", "uid") == "" {
		return errors.New("native restore retirement lacks its original identity")
	}
	// Velero 1.18.4's backup deletion controller retires its Restores too.
	// Verify that native result; an unconditional second delete fails after
	// successful cleanup and must never target a replacement with the same name.
	current, err := p.c.get(ctx, "restores.velero.io", "velero", text(restore, "metadata", "name"), true)
	if err != nil || current == nil {
		return err
	}
	if text(current, "metadata", "uid") != text(restore, "metadata", "uid") {
		return errors.New("canary restore was replaced; preserve the replacement")
	}
	return errors.New("native canary restore retirement has not finished")
}

// reattach moves only this run's restored consumer to a second client while
// requiring the same claim, PV, backend and original payload bytes.
func (p *canaryRun) reattach(ctx context.Context, created map[string]object, originalPV, originalVolume object) error {
	namespace := "longhorn-canary-v2-" + p.run
	originalClaim := created["PersistentVolumeClaim"]
	if err := removeScratch(ctx, p.c, p.source, "pod", namespace, "/api/v1/namespaces/"+namespace+"/pods/fio", created["Pod"]); err != nil {
		return err
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		volume, err := p.c.get(ctx, "volumes.longhorn.io", "longhorn-system", text(originalVolume, "metadata", "name"), false)
		if err != nil || text(volume, "metadata", "uid") != text(originalVolume, "metadata", "uid") || text(volume, "status", "state") != "detached" {
			return errors.New("canary volume has not detached before changing clients")
		}
		return nil
	}); err != nil {
		return err
	}
	assets, err := canaryProbeResources(namespace, "lh-canary-v2-"+p.run, p.run, "v2")
	if err != nil {
		return err
	}
	desired := assets[4]
	desired["spec"].(object)["nodeSelector"] = object{"kubernetes.io/hostname": "prod-worker-1"}
	desired["spec"].(object)["activeDeadlineSeconds"] = 1800
	pod, err := p.create(ctx, "reattached/Pod", desired)
	if err != nil {
		return err
	}
	bound, pv, volume, err := p.binding(ctx, namespace, "lh-canary-v2-"+p.run, "v2", "prod-worker-1", text(pod, "metadata", "uid"))
	if err != nil {
		return err
	}
	if text(pv, "metadata", "uid") != text(originalPV, "metadata", "uid") || text(volume, "metadata", "uid") != text(originalVolume, "metadata", "uid") || text(bound["PersistentVolumeClaim"], "metadata", "uid") != text(originalClaim, "metadata", "uid") {
		return errors.New("canary reattachment replaced its original restored storage")
	}
	if _, err = p.verifyBytes(ctx, bound["Pod"], "prod-worker-1", p.record.PayloadSHA256); err != nil {
		return err
	}
	created["Pod"] = bound["Pod"]
	return nil
}

// removeBackup delegates data retirement to Velero's UID-bound deletion request
// and verifies native absence before removing the owned modifier.
func (p *canaryRun) removeBackup(ctx context.Context, original, restore, modifier object) error {
	name := "longhorn-v2-canary-" + p.run
	backup, err := p.c.get(ctx, "backups.velero.io", "velero", name, false)
	if err != nil || text(backup, "metadata", "uid") != text(original, "metadata", "uid") {
		return errors.New("canary backup was replaced; refusing retirement")
	}
	if err = cleanVeleroResult(backup); err != nil {
		return err
	}
	// Velero's native deletion request removes Kopia snapshots and backup
	// objects together. Never delete only the Backup CR and strand its data.
	request, err := p.create(ctx, "DeleteBackupRequest", object{"apiVersion": "velero.io/v1", "kind": "DeleteBackupRequest",
		"metadata": object{"name": name + "-delete", "namespace": "velero", "labels": object{"velero.io/backup-name": name, "velero.io/backup-uid": text(backup, "metadata", "uid")}},
		"spec":     object{"backupName": name}})
	if err != nil {
		return err
	}
	if err = waitReady(ctx, func(ctx context.Context) error {
		current, err := p.c.get(ctx, "backups.velero.io", "velero", name, true)
		if err != nil {
			return err
		}
		if current != nil && text(current, "metadata", "uid") != text(original, "metadata", "uid") {
			return errors.New("backup identity changed during retirement")
		}
		dbr, err := p.c.get(ctx, "deletebackuprequests.velero.io", "velero", text(request, "metadata", "name"), true)
		if err != nil {
			return err
		}
		if dbr != nil {
			if text(dbr, "metadata", "uid") != text(request, "metadata", "uid") {
				return errors.New("backup deletion request was replaced")
			}
			if errs := value(dbr, "status", "errors"); errs != nil {
				if list, ok := errs.([]any); !ok || len(list) != 0 {
					return errors.New("native canary backup deletion reported errors")
				}
			}
		}
		// Successful native deletion removes its own request after deleting the
		// backup and all volume-data snapshots. Both absences are required.
		if current != nil || dbr != nil {
			return errors.New("native canary backup retirement has not finished")
		}
		return nil
	}); err != nil {
		return err
	}
	if err = p.requireNativeRestoreGone(ctx, restore); err != nil {
		return err
	}
	cm, err := p.c.get(ctx, "configmap", "velero", name, false)
	if err != nil || text(cm, "metadata", "uid") != p.record.Resources["Modifier"].UID || text(cm, "data", "rules.yaml") != text(modifier, "data", "rules.yaml") {
		return errors.New("canary restore modifier identity changed")
	}
	return removeScratch(ctx, p.c, p.source, "configmap", "velero", "/api/v1/namespaces/velero/configmaps/"+name, cm)
}
