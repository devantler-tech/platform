package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

func randomRecoveryValue() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("random")
	}
	return hex.EncodeToString(raw[:]), nil
}

func approvedBackupControllerImage(image string) bool {
	return regexp.MustCompile(`^(docker\.io/)?velero/velero:v1\.18\.4(@sha256:[0-9a-f]{64})?$`).MatchString(image)
}
func (d *recoveryDriver) list(ctx context.Context, kind, ns, selector string) ([]object, error) {
	args := []string{"get", kind, "--namespace=" + ns, "-o=json"}
	if selector != "" {
		args = append(args, "--selector="+selector)
	}
	data, err := d.k.call(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var o object
	if json.Unmarshal(data, &o) != nil {
		return nil, errors.New("list-response")
	}
	raw, ok := o["items"].([]any)
	if !ok {
		return nil, errors.New("list-items")
	}
	entries := items(o, "items")
	if len(raw) != len(entries) {
		return nil, errors.New("list-shape")
	}
	return entries, nil
}
func (d *recoveryDriver) preflight(ctx context.Context) (string, error) {
	var err error
	d.source, err = d.readSource(ctx)
	if err != nil {
		return "", err
	}
	velero, found, err := d.lookup(ctx, "Deployment", "velero", "velero")
	if err != nil || !found {
		return "", errors.New("backup-controller")
	}
	versionOK := false
	for _, container := range items(child(child(child(velero, "spec"), "template"), "spec"), "containers") {
		if text(container, "name") == "velero" && approvedBackupControllerImage(text(container, "image")) {
			versionOK = true
		}
	}
	if !versionOK {
		return "", errors.New("backup-controller-version")
	}
	location, found, err := d.lookup(ctx, "BackupStorageLocation", "default", "velero")
	if err != nil || !found || text(child(location, "status"), "phase") != "Available" || (text(child(location, "spec"), "accessMode") != "" && text(child(location, "spec"), "accessMode") != "ReadWrite") {
		return "", errors.New("backup-location")
	}
	repo, found, err := d.lookup(ctx, "BackupRepository", "private-mail-default-kopia", "velero")
	if err != nil || !found || text(child(repo, "spec"), "volumeNamespace") != "private-mail" || text(child(repo, "spec"), "backupStorageLocation") != "default" || text(child(repo, "spec"), "repositoryType") != "kopia" || text(child(repo, "status"), "phase") != "Ready" {
		return "", errors.New("backup-repository")
	}
	for _, ref := range []ownedReference{{kind: "Namespace", name: d.i.namespace}, {kind: "StorageClass", name: d.i.namespace}, {kind: "Backup", name: d.i.namespace + "-backup", namespace: "velero"}, {kind: "DataDownload", name: d.i.namespace + "-download", namespace: "velero"}} {
		if _, exists, err := d.lookup(ctx, ref.kind, ref.name, ref.namespace); err != nil || exists {
			return "", errors.New("invocation-collision")
		}
	}
	secret, found, err := d.lookup(ctx, "Secret", "private-mail-maintainer", "private-mail")
	if err != nil || !found {
		return "", errors.New("mailbox-credential")
	}
	password, err := base64.StdEncoding.DecodeString(text(child(secret, "data"), "password"))
	if err != nil || len(password) < 16 || len(password) > 4096 {
		return "", errors.New("mailbox-credential")
	}
	return string(password), nil
}
func (d *recoveryDriver) ref(kind, name, ns string) (ownedReference, error) {
	ref, ok := d.refs[(ownedReference{kind: kind, name: name, namespace: ns}).key()]
	if !ok {
		return ref, errors.New("untracked-resource")
	}
	return ref, nil
}
func (d *recoveryDriver) execute(ctx context.Context) error {
	password, err := d.preflight(ctx)
	if err != nil {
		return err
	}
	endpoint, stopSource, err := d.forward(ctx, "private-mail", "private-mail-0", d.source.podUID)
	if err != nil {
		return err
	}
	defer stopSource()
	original, err := newMailboxClient(endpoint, password)
	if err != nil {
		return err
	}
	if err = original.session(ctx); err != nil {
		return err
	}
	d.stage = "source-marker"
	nonce, err := randomRecoveryValue()
	if err != nil {
		return err
	}
	after := time.Now().UTC().Truncate(time.Second)
	d.started = true
	marker, err := original.createMarker(ctx, nonce)
	if err != nil {
		return err
	}
	d.stage = "backup"
	backup, err := d.create(ctx, backupManifest(d.i))
	if err != nil {
		return err
	}
	var completed object
	err = waitRecovery(ctx, 8*time.Minute, func() (bool, error) {
		live, err := d.owned(ctx, backup)
		if err != nil {
			return false, err
		}
		phase := text(child(live, "status"), "phase")
		switch phase {
		case "Completed":
			completed = live
			return true, nil
		case "Failed", "FailedValidation", "PartiallyFailed", "Deleting":
			return false, errors.New("backup-incomplete")
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	volumes, err := d.list(ctx, "PodVolumeBackup", "velero", "velero.io/backup-uid="+backup.uid)
	if err != nil {
		return errors.New("backup-volume-count")
	}
	volume, err := sourceVolumeBackup(volumes, d.source.volume)
	if err != nil {
		return err
	}
	snapshot, err := proveBackup(completed, volume, d.source, d.i, backup.uid, after)
	if err != nil {
		return err
	}
	if err = d.sourceRetained(ctx); err != nil {
		return err
	}
	d.stage = "isolated-storage"
	key, err := randomRecoveryValue()
	if err != nil {
		return err
	}
	admin, err := randomRecoveryValue()
	if err != nil {
		return err
	}
	for _, resource := range recoveryResources(d.i, key, "restore:"+admin) {
		if _, err = d.create(ctx, resource); err != nil {
			return err
		}
	}
	reader, err := d.create(ctx, recoveryReader(d.i, d.image))
	if err != nil {
		return err
	}
	d.stage = "restore"
	transfer, err := d.create(ctx, downloadManifest(d.i, snapshot))
	if err != nil {
		return err
	}
	err = waitRecovery(ctx, 12*time.Minute, func() (bool, error) {
		live, err := d.owned(ctx, transfer)
		if err != nil {
			return false, err
		}
		spec, status := child(live, "spec"), child(live, "status")
		target := child(spec, "targetVolume")
		if text(spec, "snapshotID") != snapshot || text(spec, "sourceNamespace") != "private-mail" || text(spec, "backupStorageLocation") != "default" || text(target, "namespace") != d.i.namespace || text(target, "pvc") != "data" {
			return false, errors.New("transfer-identity")
		}
		switch text(status, "phase") {
		case "Completed":
			progress := child(status, "progress")
			total, ok := progress["totalBytes"].(float64)
			if !ok || total <= 0 || progress["bytesDone"] != total {
				return false, errors.New("transfer-progress")
			}
			return true, nil
		case "Failed", "Canceled", "Canceling":
			return false, errors.New("transfer-incomplete")
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	d.stage = "storage-proof"
	if err = d.observeRestoredStorage(ctx); err != nil {
		return err
	}
	err = waitRecovery(ctx, 2*time.Minute, func() (bool, error) {
		live, err := d.owned(ctx, reader)
		if err != nil {
			return false, err
		}
		phase := text(child(live, "status"), "phase")
		if phase == "Failed" || phase == "Succeeded" {
			return false, errors.New("reader-stopped")
		}
		return phase == "Running", nil
	})
	if err != nil {
		return err
	}
	d.stage = "mailbox-proof"
	endpoint, stopReader, err := d.forward(ctx, d.i.namespace, "reader", reader.uid)
	if err != nil {
		return err
	}
	defer stopReader()
	recovered, err := newMailboxClient(endpoint, password)
	if err != nil {
		return err
	}
	recovered.account = original.account
	err = waitRecovery(ctx, 90*time.Second, func() (bool, error) {
		_, e := recovered.request(ctx, "GET", "/healthz/ready", "", nil)
		return e == nil, nil
	})
	if err != nil {
		return err
	}
	if err = recovered.session(ctx); err != nil {
		return err
	}
	if _, err = recovered.verifyMessage(ctx, marker.identity); err != nil {
		return err
	}
	d.stage = "source-retention"
	if err = d.sourceRetained(ctx); err != nil {
		return err
	}
	if _, err = original.verifyMessage(ctx, marker.identity); err != nil {
		return err
	}
	d.stage = "marker-cleanup"
	if err = original.cleanupMarker(ctx, marker); err != nil {
		return err
	}
	return d.sourceRetained(ctx)
}
func (d *recoveryDriver) observeRestoredStorage(ctx context.Context) error {
	claim, err := d.ref("PersistentVolumeClaim", "data", d.i.namespace)
	if err != nil {
		return err
	}
	pvc, err := d.owned(ctx, claim)
	if err != nil {
		return err
	}
	name := text(child(pvc, "spec"), "volumeName")
	if !kubernetesName.MatchString(name) || name == d.source.pv || claim.uid == d.source.claimUID {
		return errors.New("restored-volume-name")
	}
	pv, found, err := d.lookup(ctx, "PersistentVolume", name, "")
	if err != nil || !found {
		return errors.New("restored-pv")
	}
	volume, found, err := d.lookup(ctx, "volumes.longhorn.io", name, "longhorn-system")
	if err != nil || !found {
		return errors.New("restored-volume")
	}
	if err = proveRestoredStorage(pvc, pv, volume, d.i, claim.uid, d.source.pv); err != nil {
		return err
	}
	pvUID, volumeUID := text(child(pv, "metadata"), "uid"), text(child(volume, "metadata"), "uid")
	if d.volumeName != "" && (d.volumeName != name || d.pvUID != pvUID || d.volumeUID != volumeUID) {
		d.uncertain = true
		return errors.New("restored-backend-replaced")
	}
	d.volumeName = name
	d.pvUID = pvUID
	d.volumeUID = volumeUID
	return nil
}
