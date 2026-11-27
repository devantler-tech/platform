package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
)

type nativeRestoreV2ManagerArgv struct {
	Identity                 nativeRestoreV2ManagerIdentity
	Settings                 [9]benchmarkResource
	Versions                 [11]string
	SettingValues            [9]string
	ArgvSHA256, OverrideJSON string
}

// This qualifies the pinned, inherited, non-dynamic launch profile only. The
// producer can support explicit overrides, but this initial profile does not.
// It proves no env/mount/security, host, cohort or capacity admission.
func qualifyNativeRestoreV2ManagerArgv(target nativeRestoreTarget, storage, manager, pod object, settings []object) (nativeRestoreV2ManagerArgv, error) {
	fail := func() (nativeRestoreV2ManagerArgv, error) {
		return nativeRestoreV2ManagerArgv{}, errNativeRestoreEvidence
	}
	identity, err := qualifyNativeRestoreV2ManagerIdentity(target, storage, manager, pod)
	if err != nil || len(settings) != 9 {
		return fail()
	}
	result := nativeRestoreV2ManagerArgv{Identity: identity}
	result.Versions[9], result.Versions[10] = text(manager, "metadata", "resourceVersion"), text(pod, "metadata", "resourceVersion")
	profile := [9][2]string{{"data-engine-cpu-mask", "0x3"}, {"data-engine-number-of-cpu-cores", "0"}, {"data-engine-memory-size", "2048"}, {"data-engine-hugepage-enabled", "true"}, {"data-engine-iobuf-large-pool-size", "1024"}, {"data-engine-iobuf-small-pool-size", "8192"}, {"data-engine-log-flags", ""}, {"data-engine-interrupt-mode-enabled", "false"}, {"data-engine-cpu-isolation-enabled", "true"}}
	uids := map[string]bool{target.UID: true, target.Preparation.LonghornUID: true, target.Preparation.Journal.UID: true, identity.ManagerUID: true, identity.PodUID: true}
	for _, setting := range settings {
		uid := text(setting, "metadata", "uid")
		if !nativeRestoreV2ResourceIdentity(setting, "longhorn.io/v1beta2", "Setting", "longhorn-system") || uids[uid] {
			return fail()
		}
		index := -1
		for i, entry := range profile {
			if text(setting, "metadata", "name") == entry[0] {
				index = i
			}
		}
		if index < 0 || result.Settings[index].UID != "" {
			return fail()
		}
		raw := text(setting, "value")
		actual, err := nativeRestoreV2SettingValue(raw)
		if err != nil || actual != profile[index][1] {
			return fail()
		}
		result.Settings[index], result.Versions[index], result.SettingValues[index] = benchmarkIdentity(setting), text(setting, "metadata", "resourceVersion"), raw
		uids[uid] = true
	}
	engineSpec, present := valueObject(manager, "spec")["dataEngineSpec"]
	if present {
		engines, ok := engineSpec.(object)
		if !ok || !runtimeOnlyKeys(engines, "v2") {
			return fail()
		}
		if raw, found := engines["v2"]; found {
			overrides, ok := raw.(object)
			if !ok || !runtimeOnlyKeys(overrides, "cpuMask", "cpuIsolationEnabled") {
				return fail()
			}
			for _, raw := range overrides {
				if text, ok := raw.(string); !ok || text != "" {
					return fail()
				}
			}
		}
	}
	encodedOverrides, err := json.Marshal(engineSpec)
	if err != nil {
		return fail()
	}
	result.OverrideJSON = string(encodedOverrides)
	status, ok := value(manager, "status", "dataEngineStatus").(object)
	if !ok || !runtimeOnlyKeys(status, "v2") || !reflect.DeepEqual(status["v2"], object{"cpuMask": "0x3", "cpuCoreNumber": float64(0), "interruptModeEnabled": "false"}) {
		return fail()
	}
	args := value(pod, "spec", "containers").([]any)[0].(object)["args"]
	expected := []any{"start-spdk-tgt", "--spdk-log", "all", "--spdk-cpumask", "0x3", "--spdk-core-number", "0", "--spdk-memory-size", "2048", "--longhorn-control-path", "/var/lib/longhorn", "--enable-spdk", "--debug", "daemon", "--spdk-enabled", "--listen", "0.0.0.0:8500", "--enable-irq-affinity", "--enable-workqueue-affinity", "--enable-rps"}
	if !reflect.DeepEqual(args, expected) {
		return fail()
	}
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return fail()
	}
	digest := sha256.Sum256(encodedArgs)
	result.ArgvSHA256 = hex.EncodeToString(digest[:])
	return result, nil
}
