package main

import "fmt"

const labelKey = "email.devantler.tech/storage-canary"
const probeImage = "ghcr.io/stalwartlabs/stalwart:v0.16.27@sha256:c429b311c93db5606598abc1c68225a85a6953183848ae01bee9c6a98885836c"

// All names and key references belong to this invocation, never the Email namespace.
func manifest(kind, name, namespace, run string) object {
	meta := object{"name": name, "labels": object{labelKey: run, "app.kubernetes.io/part-of": "email-storage-canary"}}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	return object{"apiVersion": "v1", "kind": kind, "metadata": meta}
}
func namespaceManifest(name, run string) object {
	o := manifest("Namespace", name, "", run)
	labels := child(child(o, "metadata"), "labels")
	labels["pod-security.kubernetes.io/enforce"] = "restricted"
	labels["pod-security.kubernetes.io/enforce-version"] = "v1.36"
	return o
}
func policyManifest(ns, run string) object {
	o := manifest("NetworkPolicy", "deny-all", ns, run)
	o["apiVersion"] = "networking.k8s.io/v1"
	o["spec"] = object{"podSelector": object{}, "policyTypes": []any{"Ingress", "Egress"}}
	return o
}
func storageClassManifest(name, ns, run string) object {
	o := manifest("StorageClass", name, "", run)
	o["apiVersion"] = "storage.k8s.io/v1"
	o["provisioner"] = "driver.longhorn.io"
	o["reclaimPolicy"] = "Delete"
	o["volumeBindingMode"] = "WaitForFirstConsumer"
	o["allowVolumeExpansion"] = true
	p := object{"encrypted": "true", "numberOfReplicas": "3", "staleReplicaTimeout": "30", "fromBackup": "", "fsType": "ext4", "dataLocality": "disabled", "unmapMarkSnapChainRemoved": "ignored", "disableRevisionCounter": "true", "dataEngine": "v1", "backupTargetName": "default"}
	for _, stage := range []string{"stage", "publish", "expand"} {
		p["csi.storage.k8s.io/node-"+stage+"-secret-name"] = "encryption-key"
		p["csi.storage.k8s.io/node-"+stage+"-secret-namespace"] = ns
	}
	o["parameters"] = p
	return o
}
func keyManifest(ns, run, key string) object {
	o := manifest("Secret", "encryption-key", ns, run)
	o["type"] = "Opaque"
	o["stringData"] = object{"CRYPTO_KEY_VALUE": key, "CRYPTO_KEY_PROVIDER": "secret", "CRYPTO_KEY_CIPHER": "aes-xts-plain64", "CRYPTO_KEY_HASH": "sha256", "CRYPTO_KEY_SIZE": "256", "CRYPTO_PBKDF": "argon2i"}
	return o
}
func claimManifest(ns, sc, run string) object {
	o := manifest("PersistentVolumeClaim", "probe", ns, run)
	o["spec"] = object{"storageClassName": sc, "accessModes": []any{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": "1Gi"}}}
	return o
}
func podManifest(ns, phase, run string) object {
	o := manifest("Pod", "probe-"+phase, ns, run)
	operation := `test "$(cat /data/probe)" = email-storage-canary-v1`
	if phase == "write" {
		operation = `umask 077; printf '%s' email-storage-canary-v1 > /data/probe; sync; ` + operation
	}
	operation += fmt.Sprintf("; printf 'EMAIL_VOLUME_PROBE=PASS phase=%s\\n'", phase)
	o["spec"] = object{
		"restartPolicy": "Never", "automountServiceAccountToken": false, "terminationGracePeriodSeconds": float64(30),
		"securityContext": object{"runAsNonRoot": true, "runAsUser": float64(2000), "runAsGroup": float64(2000), "fsGroup": float64(2000), "seccompProfile": object{"type": "RuntimeDefault"}},
		"containers":      []any{object{"name": "probe", "image": probeImage, "command": []any{"/bin/sh", "-eu", "-c", operation}, "securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}}}, "resources": object{"requests": object{"cpu": "10m", "memory": "32Mi"}, "limits": object{"cpu": "100m", "memory": "64Mi"}}, "volumeMounts": []any{object{"name": "data", "mountPath": "/data"}}}},
		"volumes":         []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": "probe"}}},
	}
	return o
}
