package main

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"regexp"
)

func (d *recoveryDriver) adoptRetainedGraph(ctx context.Context, readerUID, claimUID string) error {
	fail := func() error { return errors.New("retained-graph") }
	if readerUID == "" || claimUID == "" || readerUID == claimUID || len(d.refs) != 0 || d.image == "" {
		return fail()
	}
	graph := append(recoveryResources(d.i, "", ""), recoveryReader(d.i, d.image), downloadManifest(d.i, ""))
	// A protected disposal is bound to the fixed protection backup instead:
	// Velero has already collected the copy's own 24-hour recovery point.
	if d.protection.backup == "" {
		graph = append(graph, backupManifest(d.i))
	}
	d.retainedGraph = map[string]object{}
	seen := map[string]bool{}
	for _, want := range graph {
		m := child(want, "metadata")
		ref := ownedReference{kind: text(want, "kind"), name: text(m, "name"), namespace: text(m, "namespace")}
		actual, found, err := d.lookup(ctx, ref.kind, ref.name, ref.namespace)
		if err != nil || !found || text(actual, "kind") != ref.kind {
			return fail()
		}
		am := child(actual, "metadata")
		ref.uid = text(am, "uid")
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(ref.uid) || seen[ref.uid] || text(am, "name") != ref.name || text(am, "namespace") != ref.namespace || am["deletionTimestamp"] != nil || !d.i.permitsMutation(ref.kind, ref.namespace, ref.name) {
			return fail()
		}
		for key, value := range child(m, "labels") {
			if child(am, "labels")[key] != value {
				return fail()
			}
		}
		seen[ref.uid] = true
		switch ref.kind {
		case "Pod":
			if ref.uid != readerUID || text(child(actual, "status"), "phase") != "Running" || !admittedRecoveryObject(want, actual) {
				return fail()
			}
		case "PersistentVolumeClaim":
			spec := child(actual, "spec")
			if ref.uid != claimUID || !reflect.DeepEqual(spec["accessModes"], []any{"ReadWriteOnce"}) || text(child(child(spec, "resources"), "requests"), "storage") != "2Gi" || text(spec, "storageClassName") != d.i.namespace {
				return fail()
			}
		case "Secret":
			if !retainedSecret(actual, ref.name) {
				return fail()
			}
		case "ConfigMap":
			if !reflect.DeepEqual(actual["data"], want["data"]) || len(child(actual, "binaryData")) != 0 {
				return fail()
			}
		case "StorageClass":
			for _, key := range []string{"provisioner", "reclaimPolicy", "volumeBindingMode", "parameters"} {
				if !reflect.DeepEqual(actual[key], want[key]) {
					return fail()
				}
			}
		case "CiliumNetworkPolicy":
			if !reflect.DeepEqual(actual["spec"], want["spec"]) {
				return fail()
			}
		case "Backup":
			if !retainedBackupSpec(child(want, "spec"), child(actual, "spec")) {
				return fail()
			}
		case "DataDownload":
			spec, status := child(actual, "spec"), child(actual, "status")
			snapshot := text(spec, "snapshotID")
			if !regexp.MustCompile(`^[0-9a-f]{32,64}$`).MatchString(snapshot) || text(status, "phase") != "Completed" {
				return fail()
			}
			for _, key := range []string{"backupStorageLocation", "datamover", "sourceNamespace", "operationTimeout", "nodeOS"} {
				if spec[key] != child(want, "spec")[key] {
					return fail()
				}
			}
			target := child(spec, "targetVolume")
			if !reflect.DeepEqual(target, child(child(want, "spec"), "targetVolume")) {
				return fail()
			}
			progress := child(status, "progress")
			total, ok := progress["totalBytes"].(float64)
			if !ok || total <= 0 || progress["bytesDone"] != total {
				return fail()
			}
		}
		d.refs[ref.key()] = ref
		d.retainedGraph[ref.key()] = retainedMutableFields(actual)
	}
	return nil
}

func retainedMutableFields(o object) object {
	frozen := object{}
	for _, key := range []string{"apiVersion", "kind", "spec", "data", "binaryData", "type", "provisioner", "reclaimPolicy", "parameters", "volumeBindingMode", "allowVolumeExpansion", "mountOptions", "allowedTopologies"} {
		if value, present := o[key]; present {
			frozen[key] = value
		}
	}
	frozen["labels"] = child(child(o, "metadata"), "labels")
	frozen["owners"] = child(o, "metadata")["ownerReferences"]
	return frozen
}

func retainedBackupSpec(want, actual object) bool {
	defaults := object{"csiSnapshotTimeout": "10m0s", "itemOperationTimeout": "4h0m0s", "snapshotMoveData": false, "volumeGroupSnapshotLabelKey": "velero.io/volume-group", "excludedResources": []any{"volumesnapshots.snapshot.storage.k8s.io", "volumesnapshotcontents.snapshot.storage.k8s.io"}}
	for key, value := range want {
		if !reflect.DeepEqual(actual[key], value) {
			return false
		}
	}
	for key, value := range actual {
		if _, present := want[key]; !present {
			if expected, known := defaults[key]; !known || !reflect.DeepEqual(value, expected) {
				return false
			}
		}
	}
	return true
}

func retainedSecret(o object, name string) bool {
	if text(o, "type") != "Opaque" {
		return false
	}
	data := child(o, "data")
	values := object{}
	for key, value := range data {
		encoded, ok := value.(string)
		if !ok || len(encoded) > 8192 {
			return false
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return false
		}
		values[key] = string(decoded)
	}
	hex := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if name == "test-authentication" {
		credential := text(values, "credential")
		return len(values) == 1 && len(credential) == 72 && credential[:8] == "restore:" && hex.MatchString(credential[8:])
	}
	if name != "storage-key" || !hex.MatchString(text(values, "CRYPTO_KEY_VALUE")) {
		return false
	}
	expected := object{"CRYPTO_KEY_VALUE": values["CRYPTO_KEY_VALUE"], "CRYPTO_KEY_PROVIDER": "secret", "CRYPTO_KEY_CIPHER": "aes-xts-plain64", "CRYPTO_KEY_HASH": "sha256", "CRYPTO_KEY_SIZE": "256", "CRYPTO_PBKDF": "argon2i"}
	return reflect.DeepEqual(values, expected)
}
