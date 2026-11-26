package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func nativeRestoreV2ManagerMountsFixture() (nativeRestoreTarget, object, object, object, object, object) {
	target, storage, manager, pod := nativeRestoreV2ManagerIdentityFixture()
	volumes, mounts := []any{}, []any{}
	for _, entry := range [][5]string{{"host", "/", "/host", "", "HostToContainer"}, {"engine-binaries", "/var/lib/longhorn/engine-binaries/", "/engine-binaries/", "", "HostToContainer"}, {"metadata", "/var/lib/longhorn/metadata/", "/metadata/", "DirectoryOrCreate", "HostToContainer"}, {"unix-domain-socket", "/var/lib/longhorn/unix-domain-socket/", "/host/var/lib/longhorn/unix-domain-socket/", "", ""}, {"log", "/var/lib/longhorn/logs", "/log", "DirectoryOrCreate", "HostToContainer"}} {
		volumes = append(volumes, object{"name": entry[0], "hostPath": object{"path": entry[1], "type": entry[3]}})
		mount := object{"name": entry[0], "mountPath": entry[2], "readOnly": false}
		if entry[4] != "" {
			mount["mountPropagation"] = entry[4]
		}
		mounts = append(mounts, mount)
	}
	volumes = append(volumes, object{"name": "longhorn-grpc-tls", "secret": object{"secretName": "longhorn-grpc-tls", "optional": true, "defaultMode": float64(420)}}, object{"name": "hugepage", "emptyDir": object{"medium": "HugePages"}},
		object{"name": "kube-api-access-abcde", "projected": object{"defaultMode": float64(420), "sources": []any{object{"serviceAccountToken": object{"expirationSeconds": float64(3607), "path": "token"}}, object{"configMap": object{"name": "kube-root-ca.crt", "items": []any{object{"key": "ca.crt", "path": "ca.crt"}}}}, object{"downwardAPI": object{"items": []any{object{"path": "namespace", "fieldRef": object{"apiVersion": "v1", "fieldPath": "metadata.namespace"}}}}}}}})
	mounts = append(mounts, object{"name": "longhorn-grpc-tls", "mountPath": "/tls-files/"}, object{"name": "hugepage", "mountPath": "/hugepages"}, object{"name": "kube-api-access-abcde", "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount", "readOnly": true})
	valueObject(pod, "spec")["volumes"] = volumes
	value(pod, "spec", "containers").([]any)[0].(object)["volumeMounts"] = mounts
	account := object{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": object{"name": "longhorn-service-account", "namespace": "longhorn-system", "uid": "service-account-uid", "resourceVersion": "5"}}
	log := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Setting", "metadata": object{"name": "log-path", "namespace": "longhorn-system", "uid": "log-setting-uid", "resourceVersion": "6"}, "value": "/var/lib/longhorn/logs/"}
	return target, storage, manager, pod, account, log
}

func TestNativeRestoreV2ManagerMountsBindNativeProjection(t *testing.T) {
	target, storage, manager, pod, account, log := nativeRestoreV2ManagerMountsFixture()
	original, _ := json.Marshal(pod)
	before, err := qualifyNativeRestoreV2ManagerMounts(target, storage, manager, pod, account, log)
	if err != nil || before.Identity.PodUID != "v2-pod-uid" || before.ServiceAccount.UID != "service-account-uid" || before.Digest == "" {
		t.Fatal("native V2 mount profile was not bound:", before, err)
	}
	unchanged, _ := json.Marshal(pod)
	if string(original) != string(unchanged) {
		t.Fatal("qualification mutated the observation it must later rebind")
	}
	for _, raw := range value(pod, "spec", "volumes").([]any) {
		volume := raw.(object)
		if host, ok := volume["hostPath"].(object); ok && host["type"] == "" {
			delete(host, "type")
		}
		for _, kind := range []string{"secret", "projected"} {
			if source, ok := volume[kind].(object); ok {
				delete(source, "defaultMode")
			}
		}
	}
	after, err := qualifyNativeRestoreV2ManagerMounts(target, storage, manager, pod, account, log)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("documented native defaults changed the mount profile:", after, err)
	}
	valueObject(account, "metadata")["resourceVersion"] = "7"
	rebound, err := qualifyNativeRestoreV2ManagerMounts(target, storage, manager, pod, account, log)
	if err != nil || reflect.DeepEqual(after, rebound) {
		t.Fatal("a changed admission account was invisible:", rebound, err)
	}
}

func TestNativeRestoreV2ManagerMountsRejectEveryChangedPair(t *testing.T) {
	for index := 0; index < 8; index++ {
		for _, fault := range []string{"source", "path", "propagation", "subpath", "duplicate volume", "duplicate mount"} {
			target, storage, manager, pod, account, log := nativeRestoreV2ManagerMountsFixture()
			volumes := value(pod, "spec", "volumes").([]any)
			mounts := value(pod, "spec", "containers").([]any)[0].(object)["volumeMounts"].([]any)
			switch fault {
			case "source":
				volumes[index].(object)["emptyDir"] = object{}
			case "path":
				mounts[index].(object)["mountPath"] = "/unreviewed"
			case "propagation":
				mounts[index].(object)["mountPropagation"] = "Bidirectional"
			case "subpath":
				mounts[index].(object)["subPath"] = "unreviewed"
			case "duplicate volume":
				volumes[(index+1)%8] = volumes[index]
			case "duplicate mount":
				mounts[(index+1)%8] = mounts[index]
			}
			if got, err := qualifyNativeRestoreV2ManagerMounts(target, storage, manager, pod, account, log); err == nil || got != (nativeRestoreV2ManagerMounts{}) {
				t.Fatal("a changed volume/mount pair was accepted:", index, fault, got, err)
			}
		}
	}
}

func TestNativeRestoreV2ManagerMountsAcceptOnlyKnownDefaultLogSpellings(t *testing.T) {
	for _, setting := range []string{"/var/lib/longhorn/logs/", "/var/lib/longhorn/logs"} {
		for _, path := range []string{"/var/lib/longhorn/logs/", "/var/lib/longhorn/logs"} {
			target, storage, manager, pod, account, log := nativeRestoreV2ManagerMountsFixture()
			log["value"] = setting
			valueObject(value(pod, "spec", "volumes").([]any)[4].(object), "hostPath")["path"] = path
			if got, err := qualifyNativeRestoreV2ManagerMounts(target, storage, manager, pod, account, log); err != nil || got.Digest == "" {
				t.Fatal("a known native default log spelling was rejected:", setting, path, got, err)
			}
		}
	}
}

func TestNativeRestoreV2ManagerMountsRejectChangedCredentialSources(t *testing.T) {
	for _, fault := range []string{"tls name", "tls required", "tls mode", "token audience", "token expiration", "root ca", "namespace", "account disabled", "pod disabled", "account alias", "account revision", "log setting", "coverage mount"} {
		t.Run(fault, func(t *testing.T) {
			target, storage, manager, pod, account, log := nativeRestoreV2ManagerMountsFixture()
			volumes := value(pod, "spec", "volumes").([]any)
			sources := value(volumes[7].(object), "projected", "sources").([]any)
			switch fault {
			case "tls name":
				valueObject(volumes[5].(object), "secret")["secretName"] = "other-secret"
			case "tls required":
				valueObject(volumes[5].(object), "secret")["optional"] = false
			case "tls mode":
				valueObject(volumes[5].(object), "secret")["defaultMode"] = float64(256)
			case "token audience":
				valueObject(sources[0].(object), "serviceAccountToken")["audience"] = "other-service"
			case "token expiration":
				valueObject(sources[0].(object), "serviceAccountToken")["expirationSeconds"] = float64(3600)
			case "root ca":
				valueObject(sources[1].(object), "configMap")["name"] = "other-ca"
			case "namespace":
				valueObject(value(sources[2].(object), "downwardAPI", "items").([]any)[0].(object), "fieldRef")["fieldPath"] = "metadata.name"
			case "account disabled":
				account["automountServiceAccountToken"] = false
			case "pod disabled":
				valueObject(pod, "spec")["automountServiceAccountToken"] = false
			case "account alias":
				valueObject(account, "metadata")["uid"] = target.UID
			case "account revision":
				delete(valueObject(account, "metadata"), "resourceVersion")
			case "log setting":
				log["value"] = "/unreviewed/logs"
			case "coverage mount":
				valueObject(pod, "spec")["volumes"] = append(volumes, object{"name": "coverage", "hostPath": object{"path": "/coverage"}})
			}
			if got, err := qualifyNativeRestoreV2ManagerMounts(target, storage, manager, pod, account, log); err == nil || got != (nativeRestoreV2ManagerMounts{}) {
				t.Fatal("a changed credential or producer volume source was accepted:", got, err)
			}
		})
	}
}
