package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"regexp"
)

var nativeRestoreV2ProjectionName = regexp.MustCompile(`^kube-api-access-[a-z0-9]{5}$`)

type nativeRestoreV2ManagerMounts struct {
	Identity                   nativeRestoreV2ManagerIdentity
	ServiceAccount, LogSetting benchmarkResource
	Versions                   [4]string
	Digest, LogValue           string
}

// Bind only the producer's seven mounts and the standard API admission
// projection, using the canonical default log directory. File contents,
// image-pull configuration, env/security and full admission remain separate.
func qualifyNativeRestoreV2ManagerMounts(target nativeRestoreTarget, storage, manager, pod, account, log object) (nativeRestoreV2ManagerMounts, error) {
	fail := func() (nativeRestoreV2ManagerMounts, error) {
		return nativeRestoreV2ManagerMounts{}, errNativeRestoreEvidence
	}
	logValue, logString := log["value"].(string)
	identity, err := qualifyNativeRestoreV2ManagerIdentity(target, storage, manager, pod)
	if err != nil || !nativeRestoreV2ResourceIdentity(account, "v1", "ServiceAccount", "longhorn-system") || text(account, "metadata", "name") != "longhorn-service-account" ||
		!nativeRestoreV2ResourceIdentity(log, "longhorn.io/v1beta2", "Setting", "longhorn-system") || text(log, "metadata", "name") != "log-path" || !logString || logValue != "/var/lib/longhorn/logs" && logValue != "/var/lib/longhorn/logs/" {
		return fail()
	}
	uids := map[string]bool{target.UID: true, target.Preparation.LonghornUID: true, target.Preparation.Journal.UID: true, identity.ManagerUID: true, identity.PodUID: true}
	for _, input := range []object{account, log} {
		uid := text(input, "metadata", "uid")
		if uids[uid] {
			return fail()
		}
		uids[uid] = true
	}
	for _, raw := range []any{account["automountServiceAccountToken"], value(pod, "spec", "automountServiceAccountToken")} {
		if raw != nil && raw != true {
			return fail()
		}
	}
	expectedVolumes, expectedMounts := map[string]object{}, map[string]object{}
	for _, entry := range [][5]string{{"host", "/", "/host", "", "HostToContainer"}, {"engine-binaries", "/var/lib/longhorn/engine-binaries/", "/engine-binaries/", "", "HostToContainer"}, {"metadata", "/var/lib/longhorn/metadata/", "/metadata/", "DirectoryOrCreate", "HostToContainer"}, {"unix-domain-socket", "/var/lib/longhorn/unix-domain-socket/", "/host/var/lib/longhorn/unix-domain-socket/", "", ""}, {"log", "/var/lib/longhorn/logs", "/log", "DirectoryOrCreate", "HostToContainer"}} {
		host := object{"path": entry[1]}
		if entry[3] != "" {
			host["type"] = entry[3]
		}
		expectedVolumes[entry[0]] = object{"name": entry[0], "hostPath": host}
		mount := object{"name": entry[0], "mountPath": entry[2]}
		if entry[4] != "" {
			mount["mountPropagation"] = entry[4]
		}
		expectedMounts[entry[0]] = mount
	}
	expectedVolumes["longhorn-grpc-tls"] = object{"name": "longhorn-grpc-tls", "secret": object{"secretName": "longhorn-grpc-tls", "optional": true}}
	expectedVolumes["hugepage"] = object{"name": "hugepage", "emptyDir": object{"medium": "HugePages"}}
	expectedMounts["longhorn-grpc-tls"] = object{"name": "longhorn-grpc-tls", "mountPath": "/tls-files/"}
	expectedMounts["hugepage"] = object{"name": "hugepage", "mountPath": "/hugepages"}
	volumes, ok := value(pod, "spec", "volumes").([]any)
	if !ok || len(volumes) != 8 {
		return fail()
	}
	observedVolumes := map[string]object{}
	projection := ""
	for _, raw := range volumes {
		volume, ok := nativeRestoreV2MountObject(raw)
		name := text(volume, "name")
		if !ok || observedVolumes[name] != nil {
			return fail()
		}
		nativeRestoreV2NormalizeVolume(volume)
		expected := expectedVolumes[name]
		if expected == nil {
			if projection != "" || !nativeRestoreV2ProjectionName.MatchString(name) {
				return fail()
			}
			projection = name
			expected = nativeRestoreV2TokenVolume(name)
		}
		if !reflect.DeepEqual(volume, expected) {
			return fail()
		}
		observedVolumes[name] = volume
	}
	if projection == "" {
		return fail()
	}
	expectedMounts[projection] = object{"name": projection, "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount", "readOnly": true}
	mounts, ok := value(pod, "spec", "containers").([]any)[0].(object)["volumeMounts"].([]any)
	if !ok || len(mounts) != 8 {
		return fail()
	}
	observedMounts := map[string]object{}
	for _, raw := range mounts {
		mount, ok := nativeRestoreV2MountObject(raw)
		name := text(mount, "name")
		if !ok || observedMounts[name] != nil || observedVolumes[name] == nil {
			return fail()
		}
		if mount["readOnly"] == false {
			delete(mount, "readOnly")
		}
		if !reflect.DeepEqual(mount, expectedMounts[name]) {
			return fail()
		}
		observedMounts[name] = mount
	}
	encoded, err := json.Marshal(object{"volumes": observedVolumes, "mounts": observedMounts})
	if err != nil {
		return fail()
	}
	digest := sha256.Sum256(encoded)
	return nativeRestoreV2ManagerMounts{Identity: identity, ServiceAccount: benchmarkIdentity(account), LogSetting: benchmarkIdentity(log), Versions: [4]string{text(manager, "metadata", "resourceVersion"), text(pod, "metadata", "resourceVersion"), text(account, "metadata", "resourceVersion"), text(log, "metadata", "resourceVersion")}, Digest: hex.EncodeToString(digest[:]), LogValue: logValue}, nil
}

func nativeRestoreV2MountObject(raw any) (object, bool) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var copy object
	err = json.Unmarshal(encoded, &copy)
	return copy, err == nil && copy != nil
}

func nativeRestoreV2NormalizeVolume(volume object) {
	// The producer cleans an explicit default path, but its host-directory
	// fallback retains the trailing slash in DefaultLogDirectoryOnHost.
	if host := valueObject(volume, "hostPath"); text(volume, "name") == "log" && host["path"] == "/var/lib/longhorn/logs/" {
		host["path"] = "/var/lib/longhorn/logs"
	}
	if host := valueObject(volume, "hostPath"); host["type"] == "" {
		delete(host, "type")
	}
	for _, kind := range []string{"secret", "projected"} {
		if source := valueObject(volume, kind); source["defaultMode"] == float64(420) {
			delete(source, "defaultMode")
		}
	}
	sources, _ := value(volume, "projected", "sources").([]any)
	for _, raw := range sources {
		source, _ := raw.(object)
		items, _ := value(source, "downwardAPI", "items").([]any)
		for _, raw := range items {
			item, _ := raw.(object)
			if ref := valueObject(item, "fieldRef"); ref["apiVersion"] == "v1" {
				delete(ref, "apiVersion")
			}
		}
	}
}

func nativeRestoreV2TokenVolume(name string) object {
	return object{
		"name": name,
		"projected": object{
			"sources": []any{
				object{"serviceAccountToken": object{"expirationSeconds": float64(3607), "path": "token"}},
				object{"configMap": object{"name": "kube-root-ca.crt", "items": []any{object{"key": "ca.crt", "path": "ca.crt"}}}},
				object{"downwardAPI": object{"items": []any{object{"path": "namespace", "fieldRef": object{"fieldPath": "metadata.namespace"}}}}},
			},
		},
	}
}
