package main

import (
	"maps"
	"strings"
	"testing"
)

func nativeRestoreConsumerFixture(t *testing.T, reattached bool) (benchmarkRecord, canarySeedState, nativeRestoreState, object, string) {
	t.Helper()
	phase := "restore-verified"
	if reattached {
		phase = "reattach-verified"
	}
	r, seed, state := nativeRestoreStateFixture(t, phase)
	pod := seedPodFixture(t)
	node, uid, script := state.FirstNode, state.Resources["FirstPod"].UID, nativeRestoreReadScript
	if reattached {
		node, uid = state.SecondNode, state.Resources["SecondPod"].UID
		var err error
		script, err = nativeRestoreVerifyScript(seed.PayloadSHA256, state.Token)
		if err != nil {
			t.Fatal(err)
		}
		script += nativeReattachWait
	}
	valueObject(pod, "metadata")["uid"] = uid
	valueObject(pod, "metadata")["labels"] = object{"storage.platform.devantler.tech/benchmark-run": state.Run}
	if !reattached {
		valueObject(pod, "metadata", "labels")["velero.io/restore-name"] = state.Resources["Restore"].Name
	}
	valueObject(pod, "spec")["nodeName"] = node
	valueObject(pod, "spec")["nodeSelector"] = object{"kubernetes.io/hostname": node}
	valueObject(value(pod, "spec", "containers").([]any)[0].(object))["args"] = []any{script}
	valueObject(value(pod, "spec", "containers").([]any)[0].(object))["env"] = restorePodUIDEnvironmentFixture()
	digest := strings.Repeat("a", 64)
	if !reattached {
		security := maps.Clone(valueObject(value(pod, "spec", "containers").([]any)[0].(object), "securityContext"))
		valueObject(pod, "spec")["initContainers"] = []any{object{"name": "restore-wait", "image": "velero/velero:v1.18.4", "command": []any{"/velero-restore-helper"}, "args": []any{state.Resources["Restore"].UID}, "securityContext": security,
			"env":          []any{object{"name": "POD_NAMESPACE", "valueFrom": object{"fieldRef": object{"apiVersion": "v1", "fieldPath": "metadata.namespace"}}}, object{"name": "POD_NAME", "valueFrom": object{"fieldRef": object{"apiVersion": "v1", "fieldPath": "metadata.name"}}}},
			"resources":    object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"cpu": "100m", "memory": "128Mi"}},
			"volumeMounts": []any{object{"name": "data", "mountPath": "/restores/data"}}}}
		valueObject(pod, "status")["initContainerStatuses"] = []any{object{"name": "restore-wait", "imageID": "docker.io/velero/velero@sha256:" + digest, "containerID": "containerd://restore-helper-process", "restartCount": float64(0), "state": object{"terminated": object{"exitCode": float64(0)}}}}
	}
	return r, seed, state, pod, digest
}

func TestNativeRestoreWaitMatchesPinnedVeleroProducer(t *testing.T) {
	// Velero4ee1e79a's native helper carries two Downward API fields and a
	// positive128Mi envelope; neither is an arbitrary application environment.
	record, seed, state, pod, digest := nativeRestoreConsumerFixture(t, false)
	if err := verifyNativeRestoreConsumer(record, seed, state, pod, digest); err != nil {
		t.Fatal("pinned native restore helper rejected:", err)
	}
}

func TestNativeRestoreWaitRejectsLargerRetainedAllocations(t *testing.T) {
	for _, fault := range []string{"allocation", "retained request", "retained limit", "zero limit", "malformed allocation"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, pod, digest := nativeRestoreConsumerFixture(t, false)
			status := value(pod, "status", "initContainerStatuses").([]any)[0].(object)
			switch fault {
			case "allocation":
				status["allocatedResources"] = object{"memory": "1Gi"}
			case "retained request":
				status["resources"] = object{"requests": object{"memory": "1Gi"}}
			case "retained limit":
				status["resources"] = object{"limits": object{"memory": "1Gi"}}
			case "zero limit":
				status["resources"] = object{"limits": object{"memory": "0"}}
			case "malformed allocation":
				status["allocatedResources"] = nil
			}
			if verifyNativeRestoreConsumer(r, seed, state, pod, digest) == nil {
				t.Fatal("unbounded or larger native helper allocation accepted:", fault)
			}
		})
	}
}

func TestNativeRestoreWaitAcceptsNativeDefaultsAndEquivalentQuantities(t *testing.T) {
	r, seed, state, pod, digest := nativeRestoreConsumerFixture(t, false)
	init := value(pod, "spec", "initContainers").([]any)[0].(object)
	init["imagePullPolicy"], init["terminationMessagePath"], init["terminationMessagePolicy"] = "IfNotPresent", "/dev/termination-log", "File"
	env := init["env"].([]any)
	env[0], env[1] = env[1], env[0]
	for _, raw := range env {
		delete(valueObject(raw.(object), "valueFrom", "fieldRef"), "apiVersion")
	}
	for _, kind := range []string{"requests", "limits"} {
		init["resources"].(object)[kind] = object{"cpu": "0.1", "memory": "134217728"}
	}
	status := value(pod, "status", "initContainerStatuses").([]any)[0].(object)
	status["allocatedResources"] = object{"cpu": "100m", "memory": "128Mi"}
	status["resources"] = object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"cpu": "100m", "memory": "128Mi"}}
	if err := verifyNativeRestoreConsumer(r, seed, state, pod, digest); err != nil {
		t.Fatal("equivalent native helper envelope rejected:", err)
	}
}

func TestNativeRestoreWaitRemainsAnOrdinaryInit(t *testing.T) {
	for _, fault := range []string{"restart policy", "lifecycle", "probe", "port"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, pod, digest := nativeRestoreConsumerFixture(t, false)
			init := value(pod, "spec", "initContainers").([]any)[0].(object)
			switch fault {
			case "restart policy":
				init["restartPolicy"] = "Always"
			case "lifecycle":
				init["lifecycle"] = object{"postStart": object{"exec": object{"command": []any{"/bin/true"}}}}
			case "probe":
				init["livenessProbe"] = object{"exec": object{"command": []any{"/bin/true"}}}
			case "port":
				init["ports"] = []any{object{"containerPort": float64(8080)}}
			}
			if verifyNativeRestoreConsumer(r, seed, state, pod, digest) == nil {
				t.Fatal("unreviewed native helper execution accepted:", fault)
			}
		})
	}
}

func TestNativeRestoreConsumersKeepTheFixedExecutableAndIsolation(t *testing.T) {
	for _, reattached := range []bool{false, true} {
		for _, fault := range []string{"", "wrong consumer", "wrong node", "wrong script", "wrong image", "secret environment", "token mount", "host network", "host process", "root user", "privileged", "extra init", "ephemeral container", "not running", "restarted", "borrowed consumer", "same second node"} {
			t.Run(map[bool]string{false: "restored", true: "reattached"}[reattached]+"/"+fault, func(t *testing.T) {
				r, seed, state, pod, digest := nativeRestoreConsumerFixture(t, reattached)
				container := value(pod, "spec", "containers").([]any)[0].(object)
				switch fault {
				case "wrong consumer":
					valueObject(pod, "metadata")["uid"] = "replacement-pod"
				case "wrong node":
					valueObject(pod, "spec")["nodeName"] = "unqualified-worker"
				case "wrong script":
					container["args"] = []any{"true"}
				case "wrong image":
					container["image"] = "unreviewed-image"
				case "secret environment":
					container["envFrom"] = []any{object{"secretRef": object{"name": "application-credentials"}}}
				case "token mount":
					valueObject(pod, "spec")["automountServiceAccountToken"] = true
				case "host network":
					valueObject(pod, "spec")["hostNetwork"] = true
				case "host process":
					valueObject(pod, "spec")["hostPID"] = true
				case "root user":
					valueObject(container, "securityContext")["runAsUser"] = float64(0)
				case "privileged":
					valueObject(container, "securityContext")["privileged"] = true
				case "extra init":
					inits, _ := value(pod, "spec", "initContainers").([]any)
					valueObject(pod, "spec")["initContainers"] = append(inits, object{"name": "unreviewed-init"})
				case "ephemeral container":
					valueObject(pod, "spec")["ephemeralContainers"] = []any{object{"name": "debug"}}
				case "not running":
					valueObject(pod, "status")["phase"] = "Pending"
				case "restarted":
					value(pod, "status", "containerStatuses").([]any)[0].(object)["restartCount"] = float64(1)
				case "borrowed consumer":
					valueObject(pod, "metadata")["uid"] = r.Resources["Pod"].UID
				case "same second node":
					if reattached {
						valueObject(pod, "spec")["nodeName"] = state.FirstNode
					} else {
						valueObject(pod, "spec")["nodeName"] = state.Targets[1].Name
					}
				}
				err := verifyNativeRestoreConsumer(r, seed, state, pod, digest)
				if (err != nil) != (fault != "") {
					t.Fatalf("consumer %q accepted unsafe execution: %v", fault, err)
				}
			})
		}
	}
}

func TestNativeRestoreOnlyAcceptsItsCompletedOriginalVeleroWait(t *testing.T) {
	for _, fault := range []string{"missing wait", "wrong image", "wrong digest", "missing process", "wrong restore UID", "wrong command", "wrong mount", "helper environment", "extra env", "wrong field", "static env", "duplicate env", "missing env", "env api version", "env null", "resources missing", "memory unbounded", "memory larger", "cpu larger", "extra resource", "helper privileged", "helper failed", "helper restarted", "missing completion"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, state, pod, digest := nativeRestoreConsumerFixture(t, false)
			init := value(pod, "spec", "initContainers").([]any)[0].(object)
			status := value(pod, "status", "initContainerStatuses").([]any)[0].(object)
			switch fault {
			case "missing wait":
				delete(valueObject(pod, "spec"), "initContainers")
			case "wrong image":
				init["image"] = "velero/velero:latest"
			case "wrong digest":
				status["imageID"] = "docker.io/velero/velero@sha256:" + strings.Repeat("b", 64)
			case "missing process":
				delete(status, "containerID")
			case "wrong restore UID":
				init["args"] = []any{"another-restore"}
			case "wrong command":
				init["command"] = []any{"/bin/sh", "-ec", "true"}
			case "wrong mount":
				init["volumeMounts"] = []any{object{"name": "scratch", "mountPath": "/restores/data"}}
			case "helper environment":
				init["envFrom"] = []any{object{"secretRef": object{"name": "application-credentials"}}}
			case "extra env":
				init["env"] = append(init["env"].([]any), object{"name": "LD_PRELOAD", "value": "unreviewed"})
			case "wrong field":
				valueObject(init["env"].([]any)[0].(object), "valueFrom", "fieldRef")["fieldPath"] = "metadata.uid"
			case "static env":
				init["env"].([]any)[0].(object)["value"] = "another-namespace"
			case "duplicate env":
				init["env"].([]any)[1] = init["env"].([]any)[0]
			case "missing env":
				delete(init, "env")
			case "env api version":
				valueObject(init["env"].([]any)[0].(object), "valueFrom", "fieldRef")["apiVersion"] = "v2"
			case "env null":
				init["env"] = nil
			case "resources missing":
				delete(init, "resources")
			case "memory unbounded":
				valueObject(init, "resources", "limits")["memory"] = "0"
			case "memory larger":
				valueObject(init, "resources", "limits")["memory"] = "1Gi"
			case "cpu larger":
				valueObject(init, "resources", "limits")["cpu"] = "1"
			case "extra resource":
				valueObject(init, "resources", "requests")["hugepages-2Mi"] = "2Gi"
			case "helper privileged":
				valueObject(init, "securityContext")["privileged"] = true
			case "helper failed":
				valueObject(status, "state", "terminated")["exitCode"] = float64(1)
			case "helper restarted":
				status["restartCount"] = float64(1)
			case "missing completion":
				status["state"] = object{"running": object{}}
			}
			if verifyNativeRestoreConsumer(r, seed, state, pod, digest) == nil {
				t.Fatal("an unverified restore init process was accepted")
			}
		})
	}
}
