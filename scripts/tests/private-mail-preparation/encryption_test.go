package preparation_test

import (
	"reflect"
	"testing"
)

// TestEncryptedReplacementKeepsKeysOutsideTheWorkload checks the rendered CSI
// boundary rather than inferring encryption from a claim or class name.
func TestEncryptedReplacementKeepsKeysOutsideTheWorkload(t *testing.T) {
	objects := render(t, "k8s/bases/apps/email-encrypted-storage")
	if len(objects) != 5 {
		t.Fatal("storage preparation must contain its scoped store, key binding, two classes and one replacement claim")
	}
	for name, engine := range map[string]string{"email-encrypted": "v1", "email-encrypted-v2": "v2"} {
		class := object(t, objects, "StorageClass", name)
		if class["provisioner"] != "driver.longhorn.io" || class["reclaimPolicy"] != "Retain" || class["volumeBindingMode"] != "WaitForFirstConsumer" || class["allowVolumeExpansion"] != true {
			t.Fatalf("%s must retain data and keep the CSI scheduling and expansion model", name)
		}
		annotations, _ := mapping(t, class["metadata"])["annotations"].(map[string]any)
		if annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			t.Fatalf("%s must remain opt-in", name)
		}
		parameters := mapping(t, class["parameters"])
		for key, want := range map[string]any{"encrypted": "true", "dataEngine": engine, "fsType": "ext4", "numberOfReplicas": "3"} {
			if parameters[key] != want {
				t.Fatalf("%s CSI parameter %s = %v, want %v", name, key, parameters[key], want)
			}
		}
		for _, phase := range []string{"node-stage", "node-publish", "node-expand"} {
			if parameters["csi.storage.k8s.io/"+phase+"-secret-name"] != "email-storage-key" || parameters["csi.storage.k8s.io/"+phase+"-secret-namespace"] != "private-mail" {
				t.Fatalf("%s %s must retain the exact operational key reference", name, phase)
			}
		}
		for _, phase := range []string{"provisioner", "controller-publish", "controller-expand"} {
			if parameters["csi.storage.k8s.io/"+phase+"-secret-name"] != nil || parameters["csi.storage.k8s.io/"+phase+"-secret-namespace"] != nil {
				t.Fatalf("%s must keep secret references on the node path", name)
			}
		}
	}

	key := mapping(t, object(t, objects, "ExternalSecret", "email-storage-key")["spec"])
	if !reflect.DeepEqual(key["secretStoreRef"], map[string]any{"kind": "SecretStore", "name": "email-storage"}) {
		t.Fatal("operational key must resolve through its dedicated namespaced store")
	}
	store := mapping(t, object(t, objects, "SecretStore", "email-storage")["spec"])
	vault := mapping(t, mapping(t, store["provider"])["vault"])
	auth := mapping(t, mapping(t, vault["auth"])["kubernetes"])
	if auth["role"] != "email-storage" || !reflect.DeepEqual(auth["serviceAccountRef"], map[string]any{"name": "private-mail"}) {
		t.Fatal("key store must authenticate using only the existing namespaced Email identity")
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
	if labels, _ := metadata["labels"].(map[string]any); labels["platform.devantler.tech/storage-stage"] != "prepared" {
		t.Fatal("an unconsumed replacement claim must carry the prepared-storage health marker")
	}
	spec := mapping(t, pvc["spec"])
	if spec["storageClassName"] != "email-encrypted" || !reflect.DeepEqual(spec["accessModes"], []any{"ReadWriteOnce"}) || mapping(t, mapping(t, spec["resources"])["requests"])["storage"] != "2Gi" {
		t.Fatal("replacement must explicitly request the encrypted class with existing capacity and access mode")
	}
}

// TestProductionServesTheMailStoreFromEncryptedStorage pins the maintainer's
// copy-based cutover (#5387): production serves from the encrypted claim after
// a verified one-time copy, and the original claim stays unchanged, declared
// and mounted read-only only by that copy step, as the rollback.
func TestProductionServesTheMailStoreFromEncryptedStorage(t *testing.T) {
	for _, path := range []string{"k8s/bases/apps", "k8s/providers/docker/apps"} {
		for _, o := range render(t, path) {
			metadata := mapping(t, o["metadata"])
			for _, name := range []string{"email-storage", "email-storage-key", "email-encrypted", "email-encrypted-v2", "email-data-encrypted"} {
				if metadata["name"] == name {
					t.Fatalf("only production holds the storage key, so only production selects encrypted storage: %s", path)
				}
			}
		}
	}
	production := render(t, "k8s/providers/hetzner/apps")
	assertRetainedMailStorage(t, production)
	original := object(t, production, "PersistentVolumeClaim", "private-mail-data")
	if mapping(t, original["spec"])["storageClassName"] == "email-encrypted" {
		t.Fatal("the original claim cannot be encrypted by changing its class; it stays the rollback")
	}
	encrypted := object(t, production, "PersistentVolumeClaim", "email-data-encrypted")
	if mapping(t, encrypted["spec"])["storageClassName"] != "email-encrypted" || mapping(t, mapping(t, encrypted["metadata"])["annotations"])["kustomize.toolkit.fluxcd.io/prune"] != "disabled" {
		t.Fatal("the serving claim must use the encrypted class and survive a failed deployment")
	}
	if labels, _ := mapping(t, encrypted["metadata"])["labels"].(map[string]any); labels["platform.devantler.tech/storage-stage"] != nil {
		t.Fatal("the serving claim must leave the prepared-storage health exception and wait for binding")
	}
	if mapping(t, object(t, production, "StorageClass", "email-encrypted")["parameters"])["encrypted"] != "true" {
		t.Fatal("the serving class must encrypt at rest")
	}
	if mapping(t, object(t, production, "StorageClass", "email-encrypted-v2")["parameters"])["encrypted"] != "true" {
		t.Fatal("the V2 migration destination must encrypt at rest")
	}
	pod := mapping(t, mapping(t, mapping(t, object(t, production, "StatefulSet", "private-mail")["spec"])["template"])["spec"])
	volumes := map[string]map[string]any{}
	for _, item := range sequence(t, pod["volumes"]) {
		volume := mapping(t, item)
		if claim, ok := volume["persistentVolumeClaim"].(map[string]any); ok {
			volumes[stringField(t, volume, "name")] = claim
		}
	}
	if len(volumes) != 2 || volumes["data"]["claimName"] != "email-data-encrypted" || volumes["data"]["readOnly"] == true ||
		volumes["legacy-data"]["claimName"] != "private-mail-data" || volumes["legacy-data"]["readOnly"] != true {
		t.Fatal("the server must store data on the encrypted claim and see the original only read-only")
	}
	server := ""
	for _, item := range sequence(t, pod["containers"]) {
		container := mapping(t, item)
		if container["name"] == "private-mail" {
			server = stringField(t, container, "image")
		}
	}
	copySteps, sourceMounts := 0, 0
	for index, group := range []string{"initContainers", "containers"} {
		for position, item := range sequence(t, pod[group]) {
			container := mapping(t, item)
			name := stringField(t, container, "name")
			for _, mountItem := range sequence(t, container["volumeMounts"]) {
				mount := mapping(t, mountItem)
				if mount["name"] != "legacy-data" {
					continue
				}
				if index != 0 || position != 0 || name != "migrate-storage" || mount["readOnly"] != true {
					t.Fatal("only the first initializer, the copy step, may mount the original claim, and only read-only")
				}
				sourceMounts++
			}
			if name == "migrate-storage" {
				copySteps++
				if index != 0 || position != 0 || container["image"] != server {
					t.Fatal("the copy must run first, before setup, with the server's signed image")
				}
			}
		}
	}
	if copySteps != 1 || sourceMounts != 1 {
		t.Fatal("exactly one copy step must mount the original claim exactly once")
	}
}
