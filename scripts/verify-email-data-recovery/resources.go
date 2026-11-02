package main

func recoveryManifest(i invocation, kind, name, ns, api string) object {
	meta := object{"name": name, "labels": object{recoveryLabel: i.run, "app.kubernetes.io/part-of": "email-data-recovery"}}
	if ns != "" {
		meta["namespace"] = ns
	}
	return object{"apiVersion": api, "kind": kind, "metadata": meta}
}

// The backup supplies bytes only. No backed-up Pod, hook, account credential,
// route, Service or permission object is restored into the test namespace.
func recoveryResources(i invocation, key, auth string) []object {
	ns := recoveryManifest(i, "Namespace", i.namespace, "", "v1")
	labels := child(child(ns, "metadata"), "labels")
	labels["pod-security.kubernetes.io/enforce"] = "restricted"
	labels["pod-security.kubernetes.io/enforce-version"] = "v1.36"
	policy := recoveryManifest(i, "CiliumNetworkPolicy", "isolation", i.namespace, "cilium.io/v2")
	policy["spec"] = object{"endpointSelector": object{}, "ingressDeny": []any{object{"fromEntities": []any{"all"}}}, "egressDeny": []any{object{"toEntities": []any{"all"}}}}
	sc := recoveryManifest(i, "StorageClass", i.namespace, "", "storage.k8s.io/v1")
	sc["provisioner"] = "driver.longhorn.io"
	sc["reclaimPolicy"] = "Delete"
	sc["volumeBindingMode"] = "WaitForFirstConsumer"
	params := object{"encrypted": "true", "numberOfReplicas": "3", "staleReplicaTimeout": "30", "fromBackup": "", "fsType": "ext4", "dataLocality": "disabled", "unmapMarkSnapChainRemoved": "ignored", "disableRevisionCounter": "true", "dataEngine": "v1", "backupTargetName": "default"}
	for _, stage := range []string{"stage", "publish", "expand"} {
		params["csi.storage.k8s.io/node-"+stage+"-secret-name"] = "storage-key"
		params["csi.storage.k8s.io/node-"+stage+"-secret-namespace"] = i.namespace
	}
	sc["parameters"] = params
	secret := recoveryManifest(i, "Secret", "storage-key", i.namespace, "v1")
	secret["type"] = "Opaque"
	secret["stringData"] = object{"CRYPTO_KEY_VALUE": key, "CRYPTO_KEY_PROVIDER": "secret", "CRYPTO_KEY_CIPHER": "aes-xts-plain64", "CRYPTO_KEY_HASH": "sha256", "CRYPTO_KEY_SIZE": "256", "CRYPTO_PBKDF": "argon2i"}
	admin := recoveryManifest(i, "Secret", "test-authentication", i.namespace, "v1")
	admin["type"] = "Opaque"
	admin["stringData"] = object{"credential": auth}
	config := recoveryManifest(i, "ConfigMap", "datastore", i.namespace, "v1")
	config["data"] = object{"config.json": `{"@type":"RocksDb","path":"/var/lib/stalwart/data","bufferSize":33554432,"cacheSize":33554432}`}
	pvc := recoveryManifest(i, "PersistentVolumeClaim", "data", i.namespace, "v1")
	// Velero 1.18.4's CSI restore action uses this impossible selector until its
	// data mover restores and rebinds the helper volume to the original claim.
	pvc["spec"] = object{"storageClassName": i.namespace, "selector": object{"matchLabels": object{"velero.io/dynamic-pv-restore": i.namespace}}, "accessModes": []any{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": "2Gi"}}}
	return []object{ns, policy, sc, secret, admin, config, pvc}
}

func recoveryReader(i invocation, image string) object {
	pod := recoveryManifest(i, "Pod", "reader", i.namespace, "v1")
	pod["spec"] = object{
		"restartPolicy": "Never", "automountServiceAccountToken": false, "terminationGracePeriodSeconds": float64(30),
		"securityContext": object{"runAsNonRoot": true, "runAsUser": float64(2000), "runAsGroup": float64(2000), "fsGroup": float64(2000), "fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": object{"type": "RuntimeDefault"}},
		"containers": []any{object{
			"name": "reader", "image": image, "command": []any{"/usr/local/bin/stalwart"}, "args": []any{"--config", "/etc/stalwart/config.json"},
			"env": []any{object{"name": "STALWART_RECOVERY_MODE", "value": "1"}, object{"name": "STALWART_RECOVERY_MODE_PORT", "value": "8080"}, object{"name": "STALWART_RECOVERY_ADMIN", "valueFrom": object{"secretKeyRef": object{"name": "test-authentication", "key": "credential"}}}},
			// The pinned executable retains an effective NET_BIND_SERVICE file
			// capability, including when recovery binds the unprivileged port.
			"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "runAsNonRoot": true, "seLinuxOptions": object{}, "seccompProfile": object{"type": "RuntimeDefault"}, "capabilities": object{"drop": []any{"ALL"}, "add": []any{"NET_BIND_SERVICE"}}},
			"resources":       object{"requests": object{"cpu": "25m", "memory": "128Mi"}, "limits": object{"cpu": "1", "memory": "512Mi"}},
			"volumeMounts":    []any{object{"name": "data", "mountPath": "/var/lib/stalwart"}, object{"name": "config", "mountPath": "/etc/stalwart", "readOnly": true}, object{"name": "tmp", "mountPath": "/tmp"}},
		}},
		"volumes": []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": "data"}}, object{"name": "config", "configMap": object{"name": "datastore", "defaultMode": float64(292)}}, object{"name": "tmp", "emptyDir": object{"medium": "Memory", "sizeLimit": "64Mi"}}},
	}
	return pod
}
