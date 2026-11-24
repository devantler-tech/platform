package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"reflect"
	"strconv"
)

type nativeRestoreV2ManagerProcess struct {
	Identity                                   nativeRestoreV2ManagerIdentity
	Settings                                   [2]benchmarkResource
	Versions                                   [2]string
	PodVersion, ManagerVersion, EnvelopeSHA256 string
}

// Bind the remaining container execution inputs from the pinned V2 producer.
// Pod placement, priority, image credentials and cohort admission remain separate.
func qualifyNativeRestoreV2ManagerProcess(target nativeRestoreTarget, storage, manager, pod object, settings []object) (nativeRestoreV2ManagerProcess, error) {
	fail := func() (nativeRestoreV2ManagerProcess, error) {
		return nativeRestoreV2ManagerProcess{}, errNativeRestoreEvidence
	}
	identity, err := qualifyNativeRestoreV2ManagerIdentity(target, storage, manager, pod)
	if err != nil || len(settings) != 2 {
		return fail()
	}
	result := nativeRestoreV2ManagerProcess{Identity: identity, PodVersion: text(pod, "metadata", "resourceVersion"), ManagerVersion: text(manager, "metadata", "resourceVersion")}
	seen := map[string]bool{target.UID: true, target.Preparation.LonghornUID: true, target.Preparation.Journal.UID: true, identity.ManagerUID: true, identity.PodUID: true}
	values := [2]string{}
	for _, setting := range settings {
		if !nativeRestoreV2ResourceIdentity(setting, "longhorn.io/v1beta2", "Setting", "longhorn-system") || seen[text(setting, "metadata", "uid")] {
			return fail()
		}
		index := -1
		switch text(setting, "metadata", "name") {
		case "instance-manager-pod-liveness-probe-timeout":
			index = 0
		case "system-managed-pods-image-pull-policy":
			index = 1
		}
		if index < 0 || result.Settings[index].UID != "" {
			return fail()
		}
		result.Settings[index], result.Versions[index], values[index] = benchmarkIdentity(setting), text(setting, "metadata", "resourceVersion"), text(setting, "value")
		seen[text(setting, "metadata", "uid")] = true
	}
	timeout, err := strconv.Atoi(values[0])
	if err != nil || timeout < 1 || timeout > 60 || strconv.Itoa(timeout) != values[0] {
		return fail()
	}
	pull := map[string]string{"if-not-present": "IfNotPresent", "never": "Never", "always": "Always"}[values[1]]
	if pull == "" {
		return fail()
	}
	if raw, present := valueObject(pod, "spec")["securityContext"]; present {
		security, valid := raw.(object)
		if !valid || security == nil || len(security) != 0 && !reflect.DeepEqual(security, object{"fsGroupChangePolicy": "OnRootMismatch"}) {
			return fail()
		}
	}
	container := value(pod, "spec", "containers").([]any)[0].(object)
	privileged := object{"privileged": true}
	admitted := object{"privileged": true, "seLinuxOptions": object{}}
	security := value(container, "securityContext")
	if !runtimeOnlyKeys(container, "name", "image", "imagePullPolicy", "command", "args", "resources", "securityContext", "env", "lifecycle", "livenessProbe", "volumeMounts", "terminationMessagePath", "terminationMessagePolicy", "stdin", "stdinOnce", "tty") ||
		!reflect.DeepEqual(security, privileged) && !reflect.DeepEqual(security, admitted) {
		return fail()
	}
	for _, field := range []string{"stdin", "stdinOnce", "tty"} {
		if v, present := container[field]; present && v != false {
			return fail()
		}
	}
	if value(container, "imagePullPolicy") != pull {
		return fail()
	}
	for key, expected := range map[string]string{"terminationMessagePath": "/dev/termination-log", "terminationMessagePolicy": "File"} {
		if actual, present := container[key]; present && actual != expected {
			return fail()
		}
	}
	environment := []any{object{"name": "TLS_DIR", "value": "/tls-files/"}, object{"name": "POD_IP", "valueFrom": object{"fieldRef": object{"apiVersion": "v1", "fieldPath": "status.podIP"}}}, object{"name": "DATA_ENGINE", "value": "v2"}}
	if !reflect.DeepEqual(container["env"], environment) {
		// Kubernetes defaults only this omitted ObjectFieldSelector version.
		beforeDefault := []any{environment[0], object{"name": "POD_IP", "valueFrom": object{"fieldRef": object{"fieldPath": "status.podIP"}}}, environment[2]}
		if !reflect.DeepEqual(container["env"], beforeDefault) {
			return fail()
		}
	}
	lifecycle := object{"preStop": object{"exec": object{"command": []any{"/bin/sh", "-c", "instance-manager-v2-prestop >> /log/prestop-v2.log 2>&1"}}}}
	if !reflect.DeepEqual(container["lifecycle"], lifecycle) {
		return fail()
	}
	probe := object{"exec": object{"command": []any{"/bin/sh", "-c", "/usr/local/bin/instance-manager-liveness-probe --timeout " + strconv.Itoa(timeout+1) + " --data-engine v2"}},
		"initialDelaySeconds": float64(3), "timeoutSeconds": float64(timeout), "periodSeconds": float64(timeout + 1), "failureThreshold": float64(6), "successThreshold": float64(1)}
	if !reflect.DeepEqual(container["livenessProbe"], probe) {
		beforeDefault := maps.Clone(probe)
		delete(beforeDefault, "successThreshold")
		if !reflect.DeepEqual(container["livenessProbe"], beforeDefault) {
			return fail()
		}
	}
	normalized := maps.Clone(container)
	normalized["securityContext"] = privileged
	normalized["env"], normalized["livenessProbe"] = environment, probe
	normalized["terminationMessagePath"], normalized["terminationMessagePolicy"] = "/dev/termination-log", "File"
	for _, field := range []string{"stdin", "stdinOnce", "tty"} {
		delete(normalized, field)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil || len(encoded) > 1<<20 {
		return fail()
	}
	digest := sha256.Sum256(encoded)
	result.EnvelopeSHA256 = hex.EncodeToString(digest[:])
	return result, nil
}
