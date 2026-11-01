package main

import (
	"context"
	"errors"
	"time"
)

// Failed or uncertain runs retain the diagnostic namespace and its storage key.
// This path runs only after the recovered message and source cleanup both pass.
func (d *recoveryDriver) cleanupRestore(ctx context.Context) error {
	if d.uncertain || d.volumeName == "" || d.volumeUID == "" || d.pvUID == "" {
		return errors.New("cleanup-retained")
	}
	if err := d.observeRestoredStorage(ctx); err != nil {
		return err
	}
	reader, err := d.ref("Pod", "reader", d.i.namespace)
	if err != nil {
		return err
	}
	if err = d.remove(ctx, reader); err != nil {
		return err
	}
	if err = d.observeRestoredStorage(ctx); err != nil {
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
	if err != nil || text(child(live, "status"), "phase") != "Completed" {
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
	if err != nil || len(d.refs) != 1 {
		return errors.New("cleanup-residue")
	}
	live, err = d.owned(ctx, backup)
	if err != nil || text(child(live, "status"), "phase") != "Completed" || text(child(live, "spec"), "ttl") != "24h0m0s" {
		return errors.New("retained-backup")
	}
	expires, e := time.Parse(time.RFC3339, text(child(live, "status"), "expiration"))
	if e != nil || !expires.After(time.Now()) {
		return errors.New("backup-retention")
	}
	return d.sourceRetained(ctx)
}

func (d *recoveryDriver) namespaceInventory(ctx context.Context) error {
	namespace, err := d.ref("Namespace", d.i.namespace, "")
	if err != nil || len(d.namespacePolicies) != 4 {
		return errors.New("namespace-policy-source")
	}
	if _, err = d.owned(ctx, namespace); err != nil {
		return err
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
