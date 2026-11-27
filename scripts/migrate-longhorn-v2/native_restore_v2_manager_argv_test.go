package main

import (
	"reflect"
	"testing"
)

func nativeRestoreV2ManagerArgvFixture() (nativeRestoreTarget, object, object, object, []object) {
	target, storage, manager, pod := nativeRestoreV2ManagerIdentityFixture()
	valueObject(manager, "status")["dataEngineStatus"] = object{"v2": object{"cpuMask": "0x3", "cpuCoreNumber": float64(0), "interruptModeEnabled": "false"}}
	// Independent literal from the pinned 20d9c079 producer, including the
	// isolation flags after the daemon arguments and omitted default pools.
	value(pod, "spec", "containers").([]any)[0].(object)["args"] = []any{"start-spdk-tgt", "--spdk-log", "all", "--spdk-cpumask", "0x3", "--spdk-core-number", "0", "--spdk-memory-size", "2048", "--longhorn-control-path", "/var/lib/longhorn", "--enable-spdk", "--debug", "daemon", "--spdk-enabled", "--listen", "0.0.0.0:8500", "--enable-irq-affinity", "--enable-workqueue-affinity", "--enable-rps"}
	settings := []object{}
	for _, entry := range [][2]string{{"data-engine-cpu-mask", `{"v2":"0x3"}`}, {"data-engine-number-of-cpu-cores", `{"v2":"0"}`}, {"data-engine-memory-size", `{"v2":"2048"}`}, {"data-engine-hugepage-enabled", `{"v2":"true"}`}, {"data-engine-iobuf-large-pool-size", `{"v2":"1024"}`}, {"data-engine-iobuf-small-pool-size", `{"v2":"8192"}`}, {"data-engine-log-flags", `{"v2":""}`}, {"data-engine-interrupt-mode-enabled", `{"v2":"false"}`}, {"data-engine-cpu-isolation-enabled", `{"v2":"true"}`}} {
		settings = append(settings, object{"apiVersion": "longhorn.io/v1beta2", "kind": "Setting", "metadata": object{"name": entry[0], "namespace": "longhorn-system", "uid": "setting-" + entry[0], "resourceVersion": "3"}, "value": entry[1]})
	}
	return target, storage, manager, pod, settings
}

func TestNativeRestoreV2ManagerArgvBindProducerProfile(t *testing.T) {
	target, storage, manager, pod, settings := nativeRestoreV2ManagerArgvFixture()
	before, err := qualifyNativeRestoreV2ManagerArgv(target, storage, manager, pod, settings)
	if err != nil || before.Identity.PodUID != "v2-pod-uid" || before.ArgvSHA256 == "" || before.Settings[6].Name != "data-engine-log-flags" {
		t.Fatal("the pinned V2 argv was not bound:", before, err)
	}
	valueObject(manager, "spec")["dataEngineSpec"] = object{"v2": object{"cpuMask": "", "cpuIsolationEnabled": ""}}
	after, err := qualifyNativeRestoreV2ManagerArgv(target, storage, manager, pod, settings)
	if err != nil || after.ArgvSHA256 != before.ArgvSHA256 || reflect.DeepEqual(before, after) {
		t.Fatal("native inherited overrides must preserve argv and remain visible in the receipt:", after, err)
	}
	settings[6]["value"] = `{"v1":"","v2":""}`
	rebound, err := qualifyNativeRestoreV2ManagerArgv(target, storage, manager, pod, settings)
	if err != nil || reflect.DeepEqual(after, rebound) {
		t.Fatal("a changed observed Setting must change the receipt:", rebound, err)
	}
}

func TestNativeRestoreV2ManagerArgvRejectEveryAlteredArgument(t *testing.T) {
	_, _, _, fixture, _ := nativeRestoreV2ManagerArgvFixture()
	count := len(value(fixture, "spec", "containers").([]any)[0].(object)["args"].([]any))
	for index := 0; index < count; index++ {
		for _, action := range []string{"replace", "remove", "duplicate", "reorder"} {
			target, storage, manager, pod, settings := nativeRestoreV2ManagerArgvFixture()
			container := value(pod, "spec", "containers").([]any)[0].(object)
			args := container["args"].([]any)
			switch action {
			case "replace":
				args[index] = "unreviewed"
			case "remove":
				args = append(args[:index], args[index+1:]...)
			case "duplicate":
				args = append(args, args[index])
			case "reorder":
				args[index], args[(index+1)%count] = args[(index+1)%count], args[index]
			}
			container["args"] = args
			if got, err := qualifyNativeRestoreV2ManagerArgv(target, storage, manager, pod, settings); err == nil || got != (nativeRestoreV2ManagerArgv{}) {
				t.Fatal("altered V2 arguments were accepted:", index, action, got, err)
			}
		}
	}
}

func TestNativeRestoreV2ManagerArgvRejectNullEngineValues(t *testing.T) {
	for _, raw := range []string{`{"v2":null}`, `{"v1":null,"v2":""}`} {
		target, storage, manager, pod, settings := nativeRestoreV2ManagerArgvFixture()
		settings[6]["value"] = raw
		if got, err := qualifyNativeRestoreV2ManagerArgv(target, storage, manager, pod, settings); err == nil || got != (nativeRestoreV2ManagerArgv{}) {
			t.Fatal("null is not a native engine string setting:", got, err)
		}
	}
}

func TestNativeRestoreV2ManagerArgvRejectUnknownSettingsAndOverrides(t *testing.T) {
	for _, fault := range []string{"missing setting", "duplicate setting", "unknown setting", "unknown engine", "duplicate json", "numeric setting", "missing log flags", "nonempty log flags", "setting uid alias", "setting revision", "setting deleting", "mask", "dynamic cores", "memory", "no hugepages", "large pool", "small pool", "interrupt", "isolation", "override mask", "override isolation", "override unknown", "override type", "engine spec unknown", "engine spec type", "missing status", "missing zero", "string zero", "status mask", "status interrupt", "status unknown", "args type"} {
		t.Run(fault, func(t *testing.T) {
			target, storage, manager, pod, settings := nativeRestoreV2ManagerArgvFixture()
			status := valueObject(manager, "status", "dataEngineStatus", "v2")
			switch fault {
			case "missing setting":
				settings = settings[1:]
			case "duplicate setting":
				settings[1] = settings[0]
			case "unknown setting":
				valueObject(settings[0], "metadata")["name"] = "unknown-setting"
			case "unknown engine":
				settings[0]["value"] = `{"v2":"0x3","other":"0x3"}`
			case "duplicate json":
				settings[0]["value"] = `{"v2":"0x7","v2":"0x3"}`
			case "numeric setting":
				settings[1]["value"] = `{"v2":0}`
			case "missing log flags":
				settings[6]["value"] = `{"v1":""}`
			case "nonempty log flags":
				settings[6]["value"] = `{"v2":"all"}`
			case "setting uid alias":
				valueObject(settings[0], "metadata")["uid"] = target.UID
			case "setting revision":
				delete(valueObject(settings[0], "metadata"), "resourceVersion")
			case "setting deleting":
				valueObject(settings[0], "metadata")["deletionTimestamp"] = "2026-11-16T12:00:00Z"
			case "mask":
				settings[0]["value"] = `{"v2":"0x7"}`
			case "dynamic cores":
				settings[1]["value"] = `{"v2":"2"}`
			case "memory":
				settings[2]["value"] = `{"v2":"1024"}`
			case "no hugepages":
				settings[3]["value"] = `{"v2":"false"}`
			case "large pool":
				settings[4]["value"] = `{"v2":"2048"}`
			case "small pool":
				settings[5]["value"] = `{"v2":"16384"}`
			case "interrupt":
				settings[7]["value"] = `{"v2":"true"}`
			case "isolation":
				settings[8]["value"] = `{"v2":"false"}`
			case "override mask":
				valueObject(manager, "spec")["dataEngineSpec"] = object{"v2": object{"cpuMask": "0x3"}}
			case "override isolation":
				valueObject(manager, "spec")["dataEngineSpec"] = object{"v2": object{"cpuIsolationEnabled": "true"}}
			case "override unknown":
				valueObject(manager, "spec")["dataEngineSpec"] = object{"v2": object{"unknown": ""}}
			case "override type":
				valueObject(manager, "spec")["dataEngineSpec"] = object{"v2": "unknown"}
			case "engine spec unknown":
				valueObject(manager, "spec")["dataEngineSpec"] = object{"v1": object{}}
			case "engine spec type":
				valueObject(manager, "spec")["dataEngineSpec"] = "unknown"
			case "missing status":
				delete(valueObject(manager, "status"), "dataEngineStatus")
			case "missing zero":
				delete(status, "cpuCoreNumber")
			case "string zero":
				status["cpuCoreNumber"] = "0"
			case "status mask":
				status["cpuMask"] = "0x7"
			case "status interrupt":
				status["interruptModeEnabled"] = "true"
			case "status unknown":
				status["cpuIsolationEnabled"] = "true"
			case "args type":
				value(pod, "spec", "containers").([]any)[0].(object)["args"] = "start-spdk-tgt"
			}
			if got, err := qualifyNativeRestoreV2ManagerArgv(target, storage, manager, pod, settings); err == nil || got != (nativeRestoreV2ManagerArgv{}) {
				t.Fatal("unqualified V2 argv evidence was accepted:", fault, got, err)
			}
		})
	}
}
