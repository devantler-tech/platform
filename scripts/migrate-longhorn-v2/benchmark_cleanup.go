package main

import (
	"context"
	"encoding/json"
	"errors"
)

// Native kubectl raw DELETE accepts DeleteOptions. Unlike name-only deletion,
// the API atomically refuses changed or replaced objects through preconditions.
func (c client) deleteObserved(ctx context.Context, source, path string, o object) error {
	uid, version := text(o, "metadata", "uid"), text(o, "metadata", "resourceVersion")
	if uid == "" || version == "" {
		return errors.New("deletion requires the observed resource identity")
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	body, err := json.Marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground",
		"preconditions": object{"uid": uid, "resourceVersion": version}})
	if err != nil {
		return err
	}
	_, err = c.kubectl(ctx, body, "delete", "--raw="+path, "--filename=/dev/stdin")
	return err
}

func removeScratch(ctx context.Context, c client, source, kind, namespace, path string, original object) error {
	current, err := c.get(ctx, kind, namespace, text(original, "metadata", "name"), false)
	if err != nil {
		return err
	}
	if text(current, "metadata", "uid") != text(original, "metadata", "uid") {
		return errors.New("scratch resource was replaced; refusing deletion")
	}
	if err = c.deleteObserved(ctx, source, path, current); err != nil {
		return err
	}
	return waitReady(ctx, func() error {
		o, err := c.get(ctx, kind, namespace, text(original, "metadata", "name"), true)
		if err != nil {
			return err
		}
		if o != nil {
			return errors.New("scratch resource deletion has not completed")
		}
		return nil
	})
}

func verifyScratchBinding(pv, volume, claim, class object) error {
	if text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" ||
		text(pv, "spec", "claimRef", "uid") != text(claim, "metadata", "uid") || text(claim, "metadata", "uid") == "" ||
		text(pv, "spec", "claimRef", "namespace") != text(claim, "metadata", "namespace") ||
		text(pv, "spec", "claimRef", "name") != "fio-data" || text(pv, "spec", "storageClassName") != text(class, "metadata", "name") ||
		text(pv, "spec", "csi", "driver") != "driver.longhorn.io" || text(pv, "spec", "csi", "volumeHandle") != text(volume, "metadata", "name") ||
		text(volume, "metadata", "uid") == "" || text(volume, "status", "kubernetesStatus", "pvName") != text(pv, "metadata", "name") {
		return errors.New("scratch volume ownership is incomplete; no volume may be retired")
	}
	return nil
}

func cleanupBenchmark(ctx context.Context, c client, source, namespace string, created map[string]object, pv, volume object) error {
	claim, class := created["PersistentVolumeClaim"], created["StorageClass"]
	if err := verifyScratchBinding(pv, volume, claim, class); err != nil {
		return err
	}
	if err := removeScratch(ctx, c, source, "pod", namespace, "/api/v1/namespaces/"+namespace+"/pods/fio", created["Pod"]); err != nil {
		return err
	}
	if err := waitReady(ctx, func() error {
		v, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", text(volume, "metadata", "name"), false)
		if err != nil {
			return err
		}
		if text(v, "metadata", "uid") != text(volume, "metadata", "uid") || text(v, "status", "state") != "detached" {
			return errors.New("scratch backend has not detached")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := removeScratch(ctx, c, source, "persistentvolumeclaim", namespace, "/api/v1/namespaces/"+namespace+"/persistentvolumeclaims/fio-data", claim); err != nil {
		return err
	}
	if err := waitReady(ctx, func() error {
		current, err := c.get(ctx, "persistentvolume", "", text(pv, "metadata", "name"), false)
		if err != nil {
			return err
		}
		if text(current, "metadata", "uid") != text(pv, "metadata", "uid") || text(current, "status", "phase") != "Released" {
			return errors.New("scratch retained PV has not released")
		}
		if err = verifyScratchBinding(current, volume, claim, class); err != nil {
			return err
		}
		attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
		if err != nil {
			return err
		}
		for _, a := range attachments {
			if text(a, "spec", "source", "persistentVolumeName") == text(pv, "metadata", "name") && value(a, "status", "attached") == true {
				return errors.New("scratch PV still has an attached consumer")
			}
		}
		engines, err := c.list(ctx, "engines.longhorn.io", "longhorn-system")
		if err != nil {
			return err
		}
		for _, e := range engines {
			if text(e, "spec", "volumeName") == text(volume, "metadata", "name") && text(e, "status", "currentState") != "stopped" {
				return errors.New("scratch volume engine is still active")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := removeScratch(ctx, c, source, "persistentvolume", "", "/api/v1/persistentvolumes/"+text(pv, "metadata", "name"), pv); err != nil {
		return err
	}
	if err := removeScratch(ctx, c, source, "volumes.longhorn.io", "longhorn-system", "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes/"+text(volume, "metadata", "name"), volume); err != nil {
		return err
	}
	if err := waitReady(ctx, func() error {
		replicas, err := c.list(ctx, "replicas.longhorn.io", "longhorn-system")
		if err != nil {
			return err
		}
		for _, r := range replicas {
			if text(r, "spec", "volumeName") == text(volume, "metadata", "name") {
				return errors.New("scratch backend replicas remain")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	claims, err := c.list(ctx, "persistentvolumeclaims", "*")
	if err != nil {
		return err
	}
	for _, remaining := range claims {
		if text(remaining, "spec", "storageClassName") == text(class, "metadata", "name") || text(remaining, "metadata", "namespace") == namespace {
			return errors.New("scratch class or namespace has another claim")
		}
	}
	if err = removeScratch(ctx, c, source, "storageclass", "", "/apis/storage.k8s.io/v1/storageclasses/"+text(class, "metadata", "name"), class); err != nil {
		return err
	}
	return removeScratch(ctx, c, source, "namespace", "", "/api/v1/namespaces/"+namespace, created["Namespace"])
}
