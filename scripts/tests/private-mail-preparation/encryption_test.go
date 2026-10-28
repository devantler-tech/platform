package preparation_test

import (
	"reflect"
	"testing"
)

// TestEncryptedReplacementKeepsKeysOutsideTheWorkload checks the rendered CSI
// boundary rather than inferring encryption from a claim or class name.
func TestEncryptedReplacementKeepsKeysOutsideTheWorkload(t *testing.T) {
	objects := render(t, "k8s/bases/apps/email-encrypted-storage")
	if len(objects) != 3 {
		t.Fatal("storage preparation must contain only a key binding, class and replacement claim")
	}
	class := object(t, objects, "StorageClass", "email-encrypted")
	if class["provisioner"] != "driver.longhorn.io" || class["reclaimPolicy"] != "Retain" || class["volumeBindingMode"] != "WaitForFirstConsumer" || class["allowVolumeExpansion"] != true {
		t.Fatal("replacement storage must retain data and keep the existing CSI scheduling/expansion model")
	}
	annotations, _ := mapping(t, class["metadata"])["annotations"].(map[string]any)
	if annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
		t.Fatal("Email encryption must not change unrelated volumes' default class")
	}
	parameters := mapping(t, class["parameters"])
	for key, want := range map[string]any{"encrypted": "true", "dataEngine": "v1", "fsType": "ext4", "numberOfReplicas": "3"} {
		if parameters[key] != want {
			t.Fatalf("encrypted CSI parameter %s = %v, want %v", key, parameters[key], want)
		}
	}
	for _, phase := range []string{"node-stage", "node-publish", "node-expand"} {
		if parameters["csi.storage.k8s.io/"+phase+"-secret-name"] != "email-storage-key" || parameters["csi.storage.k8s.io/"+phase+"-secret-namespace"] != "private-mail" {
			t.Fatalf("%s must retain the exact operational key reference", phase)
		}
	}
	for _, phase := range []string{"provisioner", "controller-publish", "controller-expand"} {
		if parameters["csi.storage.k8s.io/"+phase+"-secret-name"] != nil {
			t.Fatal("this node-only encryption path must not add controller-side secret references")
		}
	}

	key := mapping(t, object(t, objects, "ExternalSecret", "email-storage-key")["spec"])
	if !reflect.DeepEqual(key["secretStoreRef"], map[string]any{"kind": "ClusterSecretStore", "name": "openbao"}) {
		t.Fatal("operational key must resolve from the established secrets service")
	}
	target := mapping(t, key["target"])
	if target["name"] != "email-storage-key" || target["creationPolicy"] != "Orphan" || target["deletionPolicy"] != "Retain" || target["immutable"] != true {
		t.Fatal("key must survive controller/source loss and refuse replacement by a changed source value")
	}
	data := sequence(t, key["data"])
	if len(data) != 1 || !reflect.DeepEqual(data[0], map[string]any{"secretKey": "CRYPTO_KEY_VALUE", "remoteRef": map[string]any{"key": "infrastructure/email/storage", "property": "passphrase"}}) {
		t.Fatal("key binding must read only the declared existing Email service key")
	}
	template := mapping(t, target["template"])
	values := mapping(t, template["data"])
	if values["CRYPTO_KEY_VALUE"] != "{{ .CRYPTO_KEY_VALUE }}" || values["CRYPTO_KEY_PROVIDER"] != "secret" {
		t.Fatal("CSI secret must receive the external key, never a committed value")
	}
	if key["dataFrom"] != nil {
		t.Fatal("storage preparation must never generate or silently rotate an encryption key")
	}

	pvc := object(t, objects, "PersistentVolumeClaim", "email-data-encrypted")
	metadata := mapping(t, pvc["metadata"])
	if metadata["namespace"] != "private-mail" || mapping(t, metadata["annotations"])["kustomize.toolkit.fluxcd.io/prune"] != "disabled" {
		t.Fatal("replacement data must stay in the retained Email namespace")
	}
	spec := mapping(t, pvc["spec"])
	if spec["storageClassName"] != "email-encrypted" || !reflect.DeepEqual(spec["accessModes"], []any{"ReadWriteOnce"}) || mapping(t, mapping(t, spec["resources"])["requests"])["storage"] != "2Gi" {
		t.Fatal("replacement must explicitly request the encrypted class with existing capacity and access mode")
	}
}

// TestEncryptedPreparationCannotSwitchTheExistingMailbox protects the incident
// boundary: preparation cannot rebind or replace the retained production claim.
func TestEncryptedPreparationCannotSwitchTheExistingMailbox(t *testing.T) {
	for _, path := range []string{"k8s/providers/hetzner/apps", "k8s/bases/apps", "k8s/providers/docker/apps"} {
		for _, o := range render(t, path) {
			metadata := mapping(t, o["metadata"])
			for _, name := range []string{"email-storage-key", "email-encrypted", "email-data-encrypted"} {
				if metadata["name"] == name {
					t.Fatalf("encrypted storage must remain unselected until recovery and migration proof: %s", path)
				}
			}
		}
	}
	production := render(t, "k8s/providers/hetzner/apps")
	assertRetainedMailStorage(t, production)
	pvc := object(t, production, "PersistentVolumeClaim", "private-mail-data")
	if mapping(t, pvc["spec"])["storageClassName"] == "email-encrypted" {
		t.Fatal("existing unencrypted claim cannot be encrypted by changing its class")
	}
}
