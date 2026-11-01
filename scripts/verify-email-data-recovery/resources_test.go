package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestReaderCapabilitiesPermitThePinnedServerExecutable(t *testing.T) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	container := items(child(recoveryReader(i, fixtureImage), "spec"), "containers")[0]
	security := child(container, "securityContext")
	capabilities := child(security, "capabilities")
	if !reflect.DeepEqual(capabilities["drop"], []any{"ALL"}) || !reflect.DeepEqual(capabilities["add"], []any{"NET_BIND_SERVICE"}) || security["allowPrivilegeEscalation"] != false {
		t.Fatal("the reader cannot execute the pinned file-capability binary with the minimal bounded capability")
	}
}

func TestRecoveryGraphNeverReplaysBackedUpWorkloadOrCredentials(t *testing.T) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	graph := recoveryResources(i, "test-volume-key", "restore:temporary-diagnostic-only")
	if len(graph) != 7 {
		t.Fatal("recovery graph is incomplete")
	}
	seen := map[string]bool{}
	for _, o := range append(graph, recoveryReader(i, fixtureImage)) {
		m := child(o, "metadata")
		kind, name, ns := text(o, "kind"), text(m, "name"), text(m, "namespace")
		if !i.permitsMutation(kind, ns, name) {
			t.Fatal("recovery graph escaped its mutation scope")
		}
		seen[kind+"/"+name] = true
		switch kind {
		case "CiliumNetworkPolicy":
			spec := child(o, "spec")
			if len(items(spec, "ingressDeny")) != 1 || len(items(spec, "egressDeny")) != 1 || len(child(spec, "endpointSelector")) != 0 {
				t.Fatal("restored server has an unbounded network path")
			}
		case "PersistentVolumeClaim":
			spec := child(o, "spec")
			if len(child(spec, "selector")) == 0 || text(spec, "volumeName") != "" || text(spec, "storageClassName") != i.namespace {
				t.Fatal("target could provision early or bind serving storage")
			}
		case "Pod":
			spec := child(o, "spec")
			if spec["automountServiceAccountToken"] != false || spec["hostNetwork"] == true || spec["hostPID"] == true || len(items(spec, "initContainers")) != 0 {
				t.Fatal("restored server can obtain credentials or execute restored hooks")
			}
			containers := items(spec, "containers")
			if len(containers) != 1 || text(containers[0], "image") != fixtureImage {
				t.Fatal("reader is not the reviewed server")
			}
			data, _ := json.Marshal(o)
			if strings.Contains(string(data), "secretKeyRef\":{\"name\":\"private-mail") || strings.Contains(string(data), "hostPath") || strings.Contains(string(data), "serviceAccountName") {
				t.Fatal("reader inherited source credentials or permissions")
			}
		}
	}
	for _, required := range []string{"Namespace/" + i.namespace, "StorageClass/" + i.namespace, "CiliumNetworkPolicy/isolation", "Secret/storage-key", "Secret/test-authentication", "ConfigMap/datastore", "PersistentVolumeClaim/data"} {
		if !seen[required] {
			t.Fatal("recovery graph omitted a safety prerequisite")
		}
	}
	pod := recoveryReader(i, fixtureImage)
	if !i.permitsMutation(text(pod, "kind"), text(child(pod, "metadata"), "namespace"), text(child(pod, "metadata"), "name")) {
		t.Fatal("reader is not separately owned")
	}
}

func TestReaderAcceptsObservedSafeSecurityDefaultsButRejectsInjectedWork(t *testing.T) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	want := recoveryReader(i, fixtureImage)
	data, _ := json.Marshal(want)
	var actual object
	_ = json.Unmarshal(data, &actual)
	container := items(child(actual, "spec"), "containers")[0]
	security := child(container, "securityContext")
	security["runAsNonRoot"] = true
	security["seLinuxOptions"] = object{}
	security["seccompProfile"] = object{"type": "RuntimeDefault"}
	if !admittedRecoveryObject(want, actual) {
		t.Fatal("observed platform security defaults made the reader unevaluable")
	}
	container["command"] = []any{"/bin/sh", "-c", "unreviewed-command"}
	if admittedRecoveryObject(want, actual) {
		t.Fatal("admission changed the executable but kept recovery clearance")
	}
}

func TestAdmissionRefusesInjectedExecutionAndCredentialSources(t *testing.T) {
	env := confirmedInvocation()
	i, _ := authorizeDataRecovery(func(k string) string { return env[k] })
	mutations := map[string]any{
		"lifecycle":      object{"postStart": object{"exec": object{"command": []any{"/bin/sh", "-c", "echo unexpected"}}}},
		"envFrom":        []any{object{"secretRef": object{"name": "foreign-secret"}}},
		"startupProbe":   object{"exec": object{"command": []any{"/bin/sh", "-c", "echo unexpected"}}},
		"readinessProbe": object{"exec": object{"command": []any{"/bin/sh", "-c", "echo unexpected"}}},
		"livenessProbe":  object{"exec": object{"command": []any{"/bin/sh", "-c", "echo unexpected"}}},
		"volumeDevices":  []any{object{"name": "data", "devicePath": "/dev/extra"}},
	}
	for key, value := range mutations {
		t.Run(key, func(t *testing.T) {
			want := recoveryReader(i, fixtureImage)
			data, _ := json.Marshal(want)
			var actual object
			_ = json.Unmarshal(data, &actual)
			items(child(actual, "spec"), "containers")[0][key] = value
			if admittedRecoveryObject(want, actual) {
				t.Fatal("unrequested execution or credential access was admitted")
			}
		})
	}
}
