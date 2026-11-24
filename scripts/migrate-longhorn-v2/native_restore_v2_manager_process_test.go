package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
)

func nativeRestoreV2ManagerProcessFixture() (nativeRestoreTarget, object, object, object, object, []object, []object) {
	target, node, storage, manager, pod, _, limits := nativeRestoreV2ManagerResourcesFixture()
	_, _, argvManager, argvPod, _ := nativeRestoreV2ManagerArgvFixture()
	_, _, _, mountPod, _, _ := nativeRestoreV2ManagerMountsFixture()
	container := value(pod, "spec", "containers").([]any)[0].(object)
	container["args"] = value(value(argvPod, "spec", "containers").([]any)[0].(object), "args")
	container["volumeMounts"] = value(value(mountPod, "spec", "containers").([]any)[0].(object), "volumeMounts")
	valueObject(pod, "spec")["volumes"] = value(mountPod, "spec", "volumes")
	valueObject(manager, "status")["dataEngineStatus"] = value(argvManager, "status", "dataEngineStatus")
	// Independent literal from pinned20d9c079 producer2264-2325, with native defaults.
	container["env"] = []any{object{"name": "TLS_DIR", "value": "/tls-files/"}, object{"name": "POD_IP", "valueFrom": object{"fieldRef": object{"apiVersion": "v1", "fieldPath": "status.podIP"}}}, object{"name": "DATA_ENGINE", "value": "v2"}}
	container["lifecycle"] = object{"preStop": object{"exec": object{"command": []any{"/bin/sh", "-c", "instance-manager-v2-prestop >> /log/prestop-v2.log 2>&1"}}}}
	container["livenessProbe"] = object{"exec": object{"command": []any{"/bin/sh", "-c", "/usr/local/bin/instance-manager-liveness-probe --timeout 11 --data-engine v2"}}, "initialDelaySeconds": float64(3), "timeoutSeconds": float64(10), "periodSeconds": float64(11), "failureThreshold": float64(6), "successThreshold": float64(1)}
	container["imagePullPolicy"], container["terminationMessagePath"], container["terminationMessagePolicy"] = "IfNotPresent", "/dev/termination-log", "File"
	settings := []object{}
	for _, entry := range [][2]string{{"instance-manager-pod-liveness-probe-timeout", "10"}, {"system-managed-pods-image-pull-policy", "if-not-present"}} {
		settings = append(settings, object{"apiVersion": "longhorn.io/v1beta2", "kind": "Setting", "metadata": object{"name": entry[0], "namespace": "longhorn-system", "uid": "setting-" + entry[0], "resourceVersion": "5"}, "value": entry[1]})
	}
	return target, node, storage, manager, pod, settings, limits
}

func TestNativeRestoreV2ManagerQualifiersShareOneUnchangedNativePod(t *testing.T) {
	target, node, storage, manager, pod, processSettings, limits := nativeRestoreV2ManagerProcessFixture()
	_, _, _, _, _, resourceSettings, _ := nativeRestoreV2ManagerResourcesFixture()
	_, _, _, _, argvSettings := nativeRestoreV2ManagerArgvFixture()
	_, _, _, _, account, log := nativeRestoreV2ManagerMountsFixture()
	before, _ := json.Marshal(pod)
	if _, err := qualifyNativeRestoreV2ManagerIdentity(target, storage, manager, pod); err != nil {
		t.Fatal("combined native identity rejected:", err)
	}
	if _, err := qualifyNativeRestoreV2ManagerResources(target, node, storage, manager, pod, resourceSettings, limits, nil); err != nil {
		t.Fatal("combined native resources rejected:", err)
	}
	if _, err := qualifyNativeRestoreV2ManagerArgv(target, storage, manager, pod, argvSettings); err != nil {
		t.Fatal("combined native argv rejected:", err)
	}
	if _, err := qualifyNativeRestoreV2ManagerMounts(target, storage, manager, pod, account, log); err != nil {
		t.Fatal("combined native mounts rejected:", err)
	}
	if _, err := qualifyNativeRestoreV2ManagerProcess(target, storage, manager, pod, processSettings); err != nil {
		t.Fatal("combined native process rejected:", err)
	}
	after, _ := json.Marshal(pod)
	if !bytes.Equal(before, after) {
		t.Fatal("qualification mutated the observed Pod")
	}
}

func TestNativeRestoreV2ManagerProcessNormalizesOnlyDocumentedDefaults(t *testing.T) {
	target, _, storage, manager, pod, settings, _ := nativeRestoreV2ManagerProcessFixture()
	before, err := qualifyNativeRestoreV2ManagerProcess(target, storage, manager, pod, settings)
	if err != nil {
		t.Fatal(err)
	}
	container := value(pod, "spec", "containers").([]any)[0].(object)
	delete(valueObject(container["env"].([]any)[1].(object), "valueFrom", "fieldRef"), "apiVersion")
	delete(valueObject(container, "livenessProbe"), "successThreshold")
	delete(container, "terminationMessagePath")
	delete(container, "terminationMessagePolicy")
	container["stdin"], container["stdinOnce"], container["tty"] = false, false, false
	valueObject(pod, "spec")["securityContext"] = object{"fsGroupChangePolicy": "OnRootMismatch"}
	valueObject(container, "securityContext")["seLinuxOptions"] = object{}
	settings[0], settings[1] = settings[1], settings[0]
	input, _ := json.Marshal(pod)
	after, err := qualifyNativeRestoreV2ManagerProcess(target, storage, manager, pod, settings)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("documented admission defaults changed process identity:", after, err)
	}
	unchanged, _ := json.Marshal(pod)
	if !bytes.Equal(input, unchanged) {
		t.Fatal("normalization mutated the source")
	}
}

func TestNativeRestoreV2ManagerProcessBindsSupportedProbeAndPullSettings(t *testing.T) {
	for _, timeout := range []int{1, 60} {
		for setting, policy := range map[string]string{"if-not-present": "IfNotPresent", "never": "Never", "always": "Always"} {
			target, _, storage, manager, pod, settings, _ := nativeRestoreV2ManagerProcessFixture()
			settings[0]["value"], settings[1]["value"] = strconv.Itoa(timeout), setting
			container := value(pod, "spec", "containers").([]any)[0].(object)
			container["imagePullPolicy"] = policy
			probe := valueObject(container, "livenessProbe")
			probe["timeoutSeconds"], probe["periodSeconds"] = float64(timeout), float64(timeout+1)
			valueObject(probe, "exec")["command"] = []any{"/bin/sh", "-c", "/usr/local/bin/instance-manager-liveness-probe --timeout " + strconv.Itoa(timeout+1) + " --data-engine v2"}
			if _, err := qualifyNativeRestoreV2ManagerProcess(target, storage, manager, pod, settings); err != nil {
				t.Fatal("native supported setting rejected:", timeout, setting, err)
			}
		}
	}
}

func TestNativeRestoreV2ManagerProcessBindsNativeEnvironmentAndLifecycle(t *testing.T) {
	target, _, storage, manager, pod, settings, _ := nativeRestoreV2ManagerProcessFixture()
	before, err := qualifyNativeRestoreV2ManagerProcess(target, storage, manager, pod, settings)
	if err != nil || before.Identity.PodUID != "v2-pod-uid" || before.EnvelopeSHA256 == "" || before.Settings[0].Name != "instance-manager-pod-liveness-probe-timeout" {
		t.Fatal("native process envelope was not bound:", before, err)
	}
	valueObject(settings[0], "metadata")["resourceVersion"] = "6"
	after, err := qualifyNativeRestoreV2ManagerProcess(target, storage, manager, pod, settings)
	if err != nil || reflect.DeepEqual(before, after) {
		t.Fatal("process policy revision was invisible:", after, err)
	}
}

func TestNativeRestoreV2ManagerProcessRejectsUnexpectedExecutionInputs(t *testing.T) {
	for _, fault := range []string{"env extra", "env value", "env source", "env from", "security extra", "explicit selinux", "pod groups", "pod user", "pod group", "pod seccomp", "pod security null", "group ownership override", "lifecycle extra", "prestop", "liveness command", "probe timeout", "readiness", "startup", "ports", "working dir", "stdin", "resize policy", "pull mismatch", "message path", "unknown setting", "duplicate setting", "setting alias", "setting revision", "setting deletion", "timeout zero", "timeout high", "timeout noncanonical"} {
		t.Run(fault, func(t *testing.T) {
			target, _, storage, manager, pod, settings, _ := nativeRestoreV2ManagerProcessFixture()
			container := value(pod, "spec", "containers").([]any)[0].(object)
			switch fault {
			case "env extra":
				container["env"] = append(container["env"].([]any), object{"name": "LD_PRELOAD", "value": "/host/library"})
			case "env value":
				container["env"].([]any)[0].(object)["value"] = "/elsewhere"
			case "env source":
				container["env"].([]any)[1].(object)["valueFrom"] = object{"secretKeyRef": object{"name": "foreign", "key": "ip"}}
			case "env from":
				container["envFrom"] = []any{object{"secretRef": object{"name": "foreign"}}}
			case "security extra":
				valueObject(container, "securityContext")["runAsUser"] = float64(1000)
			case "explicit selinux":
				valueObject(container, "securityContext")["seLinuxOptions"] = object{"type": "spc_t"}
			case "pod groups":
				valueObject(pod, "spec")["securityContext"] = object{"supplementalGroups": []any{float64(4242)}}
			case "pod user":
				valueObject(pod, "spec")["securityContext"] = object{"runAsUser": float64(1000)}
			case "pod group":
				valueObject(pod, "spec")["securityContext"] = object{"runAsGroup": float64(1000)}
			case "pod seccomp":
				valueObject(pod, "spec")["securityContext"] = object{"seccompProfile": object{"type": "Localhost", "localhostProfile": "foreign.json"}}
			case "pod security null":
				valueObject(pod, "spec")["securityContext"] = nil
			case "group ownership override":
				valueObject(pod, "spec")["securityContext"] = object{"fsGroupChangePolicy": "Always"}
			case "lifecycle extra":
				valueObject(container, "lifecycle")["postStart"] = object{"exec": object{"command": []any{"touch", "/host/file"}}}
			case "prestop":
				valueObject(container, "lifecycle", "preStop", "exec")["command"] = []any{"/bin/true"}
			case "liveness command":
				valueObject(container, "livenessProbe", "exec")["command"] = []any{"/bin/true"}
			case "probe timeout":
				valueObject(container, "livenessProbe")["timeoutSeconds"] = float64(9)
			case "readiness":
				container["readinessProbe"] = container["livenessProbe"]
			case "startup":
				container["startupProbe"] = container["livenessProbe"]
			case "ports":
				container["ports"] = []any{object{"containerPort": float64(8500)}}
			case "working dir":
				container["workingDir"] = "/host"
			case "stdin":
				container["stdin"] = true
			case "resize policy":
				container["resizePolicy"] = []any{object{"resourceName": "cpu", "restartPolicy": "NotRequired"}}
			case "pull mismatch":
				container["imagePullPolicy"] = "Always"
			case "message path":
				container["terminationMessagePath"] = "/host/data"
			case "unknown setting":
				valueObject(settings[0], "metadata")["name"] = "another-setting"
			case "duplicate setting":
				settings = append(settings, settings[0])
			case "setting alias":
				valueObject(settings[0], "metadata")["uid"] = target.UID
			case "setting revision":
				delete(valueObject(settings[0], "metadata"), "resourceVersion")
			case "setting deletion":
				valueObject(settings[0], "metadata")["deletionTimestamp"] = "2026-11-16T12:00:00Z"
			case "timeout zero":
				settings[0]["value"] = "0"
			case "timeout high":
				settings[0]["value"] = "61"
			case "timeout noncanonical":
				settings[0]["value"] = "010"
			}
			if got, err := qualifyNativeRestoreV2ManagerProcess(target, storage, manager, pod, settings); err == nil || got != (nativeRestoreV2ManagerProcess{}) {
				t.Fatal("altered native execution inputs accepted:", fault, got, err)
			}
		})
	}
}
