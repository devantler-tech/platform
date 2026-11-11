package main

import (
	"context"
	"errors"
	"time"
)

// Failed or uncertain runs retain the diagnostic namespace and its storage key.
// Callers must first verify the copy's diagnostic and preserve the source.
func (d *recoveryDriver) cleanupRestore(ctx context.Context) error {
	return d.cleanupRestoreAfterVerification(ctx, nil)
}

// cleanupRestoreAfterVerification repeats a caller's live readback after the
// non-destructive preflight, immediately before authorizing the first removal.
// A failed readback preserves the entire diagnostic graph and its storage key.
func (d *recoveryDriver) cleanupRestoreAfterVerification(ctx context.Context, verify func(context.Context) error) error {
	if d.uncertain || d.volumeName == "" || d.volumeUID == "" || d.pvUID == "" {
		return errors.New("cleanup-retained")
	}
	if err := d.observeRestoredStorage(ctx); err != nil {
		return err
	}
	if err := d.cleanupPreflight(ctx); err != nil {
		return err
	}
	reader, err := d.ref("Pod", "reader", d.i.namespace)
	if err != nil {
		return err
	}
	if verify != nil {
		if err = verify(ctx); err != nil {
			return err
		}
	}
	if err = d.remove(ctx, reader); err != nil {
		return err
	}
	if err = d.observeRestoredStorage(ctx); err != nil {
		return err
	}
	if err = d.sourceRetained(ctx); err != nil {
		return err
	}
	claim, err := d.ref("PersistentVolumeClaim", "data", d.i.namespace)
	if err != nil {
		return err
	}
	if err = d.remove(ctx, claim); err != nil {
		return err
	}
	err = waitRecovery(ctx, 5*time.Minute, func() (bool, error) {
		pv, presentPV, e := d.lookup(ctx, "PersistentVolume", d.volumeName, "")
		if e != nil {
			return false, e
		}
		volume, presentVolume, e := d.lookup(ctx, "volumes.longhorn.io", d.volumeName, "longhorn-system")
		if e != nil {
			return false, e
		}
		if (presentPV && text(child(pv, "metadata"), "uid") != d.pvUID) || (presentVolume && text(child(volume, "metadata"), "uid") != d.volumeUID) {
			d.uncertain = true
			return false, errors.New("cleanup-backend-replaced")
		}
		return !presentPV && !presentVolume, nil
	})
	if err != nil {
		return err
	}
	transfer, err := d.ref("DataDownload", d.i.namespace+"-download", "velero")
	if err != nil {
		return err
	}
	live, err := d.owned(ctx, transfer)
	if err != nil {
		return err
	}
	if text(child(live, "status"), "phase") != "Completed" {
		return errors.New("cleanup-transfer-not-complete")
	}
	err = waitRecovery(ctx, 2*time.Minute, func() (bool, error) {
		for _, ref := range []ownedReference{{kind: "Pod", name: transfer.name, namespace: "velero"}, {kind: "PersistentVolumeClaim", name: transfer.name, namespace: "velero"}, {kind: "PersistentVolumeClaim", name: transfer.name + "-cache", namespace: "velero"}} {
			if _, found, e := d.lookup(ctx, ref.kind, ref.name, ref.namespace); e != nil {
				return false, e
			} else if found {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if err = d.remove(ctx, transfer); err != nil {
		return err
	}
	if err = d.namespaceInventory(ctx); err != nil {
		return err
	}
	for _, ref := range []ownedReference{{kind: "Secret", name: "test-authentication", namespace: d.i.namespace}, {kind: "Secret", name: "storage-key", namespace: d.i.namespace}, {kind: "ConfigMap", name: "datastore", namespace: d.i.namespace}, {kind: "CiliumNetworkPolicy", name: "isolation", namespace: d.i.namespace}, {kind: "StorageClass", name: d.i.namespace}, {kind: "Namespace", name: d.i.namespace}} {
		if ref.kind == "Namespace" {
			if err = d.namespaceInventory(ctx); err != nil {
				return err
			}
		}
		if ref.kind == "Secret" && ref.name == "storage-key" {
			for _, backend := range []ownedReference{{kind: "PersistentVolume", name: d.volumeName}, {kind: "volumes.longhorn.io", name: d.volumeName, namespace: "longhorn-system"}} {
				if _, found, err := d.lookup(ctx, backend.kind, backend.name, backend.namespace); err != nil || found {
					return errors.New("cleanup-backend-reappeared")
				}
			}
		}
		actual, e := d.ref(ref.kind, ref.name, ref.namespace)
		if e != nil {
			return e
		}
		if e = d.remove(ctx, actual); e != nil {
			return e
		}
	}
	// This fresh recovery point deliberately remains under Velero's 24-hour TTL.
	// Only the disposable restored storage is removed by this invocation.
	backup, err := d.ref("Backup", d.i.namespace+"-backup", "velero")
	if err != nil {
		return err
	}
	if len(d.refs) != 1 {
		return errors.New("cleanup-residue")
	}
	live, err = d.owned(ctx, backup)
	if err != nil {
		return err
	}
	if text(child(live, "status"), "phase") != "Completed" || text(child(live, "spec"), "ttl") != "24h0m0s" {
		return errors.New("retained-backup")
	}
	expires, e := time.Parse(time.RFC3339, text(child(live, "status"), "expiration"))
	if e != nil || !expires.After(time.Now()) {
		return errors.New("backup-retention")
	}
	return d.sourceRetained(ctx)
}

// Check the entire disposal graph before deleting even the reader. Recheck the
// inventory and backup later as well; neither first-read evidence nor labels
// permit adopting a replacement after disposal has begun.
func (d *recoveryDriver) cleanupPreflight(ctx context.Context) error {
	if err := d.sourceRetained(ctx); err != nil {
		return err
	}
	for _, ref := range d.refs {
		if _, err := d.owned(ctx, ref); err != nil {
			return err
		}
	}
	for _, descriptor := range []ownedReference{
		{kind: "DataDownload", name: d.i.namespace + "-download", namespace: "velero"},
		{kind: "Backup", name: d.i.namespace + "-backup", namespace: "velero"},
	} {
		ref, err := d.ref(descriptor.kind, descriptor.name, descriptor.namespace)
		if err != nil {
			return err
		}
		live, err := d.owned(ctx, ref)
		if err != nil {
			return err
		}
		if text(child(live, "status"), "phase") != "Completed" || child(live, "metadata")["deletionTimestamp"] != nil {
			return errors.New("cleanup-controller-unverified")
		}
		if ref.kind == "Backup" {
			expires, err := time.Parse(time.RFC3339, text(child(live, "status"), "expiration"))
			if err != nil || !expires.After(time.Now().Add(15*time.Minute)) || text(child(live, "spec"), "ttl") != "24h0m0s" {
				return errors.New("cleanup-backup-retention")
			}
		} else {
			// Velero removes the transfer helpers shortly after the DataDownload
			// completes, so a fresh recovery can still see them here. Wait for them
			// as the transfer step does; a lookup error or a helper that outlives
			// the bound still refuses.
			err = waitRecovery(ctx, 2*time.Minute, func() (bool, error) {
				for _, helper := range []ownedReference{{kind: "Pod", name: ref.name, namespace: "velero"}, {kind: "PersistentVolumeClaim", name: ref.name, namespace: "velero"}, {kind: "PersistentVolumeClaim", name: ref.name + "-cache", namespace: "velero"}} {
					if _, found, e := d.lookup(ctx, helper.kind, helper.name, helper.namespace); e != nil {
						return false, e
					} else if found {
						return false, nil
					}
				}
				return true, nil
			})
			if err != nil {
				return errors.New("cleanup-transfer-helper")
			}
		}
	}
	return d.namespaceInventory(ctx)
}

func (d *recoveryDriver) namespaceInventory(ctx context.Context) error {
	namespace, err := d.ref("Namespace", d.i.namespace, "")
	if err != nil || len(d.namespacePolicies) != 4 {
		return errors.New("namespace-policy-source")
	}
	if _, err = d.owned(ctx, namespace); err != nil {
		return err
	}
	if d.completeNamespaceInventory != nil {
		if err = d.completeNamespaceInventory(ctx); err != nil {
			return err
		}
	}
	resources, err := d.list(ctx, "pods,persistentvolumeclaims,secrets,configmaps,services,serviceaccounts,deployments.apps,statefulsets.apps,daemonsets.apps,jobs.batch,cronjobs.batch,ciliumnetworkpolicies.cilium.io,networkpolicies.networking.k8s.io", d.i.namespace, "")
	if err != nil {
		return err
	}
	for _, o := range resources {
		m := child(o, "metadata")
		kind, name := text(o, "kind"), text(m, "name")
		if text(m, "namespace") != d.i.namespace {
			return errors.New("namespace-inventory")
		}
		if (kind == "ServiceAccount" && name == "default") || (kind == "ConfigMap" && name == "kube-root-ca.crt") {
			continue
		}
		if policy, ok := d.namespacePolicies[kind+"/"+name]; ok && policy.matches(o, namespace.uid) {
			continue
		}
		ref, e := d.ref(kind, name, d.i.namespace)
		if e != nil || text(m, "uid") != ref.uid || text(child(m, "labels"), recoveryLabel) != d.i.run {
			return errors.New("foreign-namespace-resource")
		}
	}
	return nil
}
