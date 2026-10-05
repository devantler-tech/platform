package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPodReadbackBindsAllRuntimeFields(t *testing.T) {
	image := "ghcr.io/devantler-tech/ksail-analysis-runner@sha256:" + strings.Repeat("a", 64)
	runtimeImage := "ghcr.io/devantler-tech/ksail-analysis-runner@sha256:" + strings.Repeat("b", 64)
	var desired map[string]any
	if err := json.Unmarshal([]byte(`{"spec":{
		"automountServiceAccountToken":false,"serviceAccountName":"no-permission",
		"securityContext":{"runAsUser":1001,"runAsGroup":1001,"fsGroup":1001,"seccompProfile":{"type":"RuntimeDefault"}},
		"nodeSelector":{"analysis":"enabled"},"tolerations":[{"key":"analysis","operator":"Equal","value":"enabled","effect":"NoSchedule"}],
		"volumes":[{"name":"scratch","emptyDir":{"sizeLimit":"40Gi"}}],
		"restartPolicy":"Never","activeDeadlineSeconds":1200,
		"containers":[{"name":"runner","image":"IMAGE","command":["/bin/sh","-ec","proof"],
			"securityContext":{"privileged":false,"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]}},
			"resources":{"requests":{"cpu":"3","memory":"12Gi"},"limits":{"cpu":"3500m","memory":"14Gi"}},
			"volumeMounts":[{"name":"scratch","mountPath":"/home/runner"}],
			"readinessProbe":{"exec":{"command":["true"]},"periodSeconds":2}}],
		"initContainers":[{"name":"init","image":"IMAGE","command":["cp"],"securityContext":{"readOnlyRootFilesystem":true}}]
	}}`), &desired); err != nil {
		t.Fatal(err)
	}
	spec := desired["spec"].(map[string]any)
	for _, key := range []string{"containers", "initContainers"} {
		spec[key].([]any)[0].(map[string]any)["image"] = image
	}
	encode := func(v any) []byte {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	desiredJSON := encode(desired)
	actual := func() map[string]any {
		var pod map[string]any
		if err := json.Unmarshal(desiredJSON, &pod); err != nil {
			t.Fatal(err)
		}
		s := pod["spec"].(map[string]any)
		s["nodeName"] = "assigned-node"
		s["dnsPolicy"] = "ClusterFirst"
		s["schedulerName"] = "default-scheduler"
		s["terminationGracePeriodSeconds"] = float64(30)
		s["enableServiceLinks"] = true
		s["serviceAccount"] = s["serviceAccountName"]
		s["tolerations"] = append(s["tolerations"].([]any), map[string]any{
			"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": float64(300)})
		for _, key := range []string{"containers", "initContainers"} {
			c := s[key].([]any)[0].(map[string]any)
			c["imagePullPolicy"] = "IfNotPresent"
			c["terminationMessagePath"] = "/dev/termination-log"
			c["terminationMessagePolicy"] = "File"
		}
		s["containers"].([]any)[0].(map[string]any)["readinessProbe"].(map[string]any)["timeoutSeconds"] = float64(1)
		pod["status"] = map[string]any{
			"containerStatuses":     []any{map[string]any{"name": "runner", "ready": true, "restartCount": float64(0), "imageID": runtimeImage}},
			"initContainerStatuses": []any{map[string]any{"name": "init", "restartCount": float64(0), "imageID": runtimeImage, "state": map[string]any{"terminated": map[string]any{"exitCode": float64(0)}}}},
		}
		return pod
	}
	t.Run("known-api-defaults-and-verified-runtime", func(t *testing.T) {
		if err := verifyPod(desiredJSON, encode(actual()), runtimeImage); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("containerd-reports-requested-signed-index", func(t *testing.T) {
		pod := actual()
		for _, field := range []string{"containerStatuses", "initContainerStatuses"} {
			pod["status"].(map[string]any)[field].([]any)[0].(map[string]any)["imageID"] = image
		}
		if err := verifyPod(desiredJSON, encode(pod), runtimeImage); err != nil {
			t.Fatal(err)
		}
	})
	mutations := map[string]func(map[string]any){
		"host-network":    func(s map[string]any) { s["hostNetwork"] = true },
		"host-pid":        func(s map[string]any) { s["hostPID"] = true },
		"runtime-class":   func(s map[string]any) { s["runtimeClassName"] = "unexpected" },
		"service-account": func(s map[string]any) { s["serviceAccountName"] = "privileged" },
		"token-mount":     func(s map[string]any) { s["automountServiceAccountToken"] = true },
		"pod-security":    func(s map[string]any) { s["securityContext"].(map[string]any)["runAsUser"] = float64(0) },
		"node-selector":   func(s map[string]any) { s["nodeSelector"] = map[string]any{"other": "enabled"} },
		"extra-toleration": func(s map[string]any) {
			s["tolerations"] = append(s["tolerations"].([]any), map[string]any{"operator": "Exists"})
		},
		"host-path": func(s map[string]any) {
			s["volumes"] = append(s["volumes"].([]any), map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}})
		},
		"sidecar": func(s map[string]any) {
			s["containers"] = append(s["containers"].([]any), map[string]any{"name": "sidecar", "image": "other"})
		},
		"init-image": func(s map[string]any) { s["initContainers"].([]any)[0].(map[string]any)["image"] = "other" },
		"init-security": func(s map[string]any) {
			s["initContainers"].([]any)[0].(map[string]any)["securityContext"] = map[string]any{"privileged": true}
		},
		"runner-image": func(s map[string]any) { s["containers"].([]any)[0].(map[string]any)["image"] = "other" },
		"runner-env": func(s map[string]any) {
			s["containers"].([]any)[0].(map[string]any)["env"] = []any{map[string]any{"name": "INJECTED", "value": "unsafe"}}
		},
		"runner-envfrom": func(s map[string]any) {
			s["containers"].([]any)[0].(map[string]any)["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "unsafe"}}}
		},
		"runner-mount": func(s map[string]any) { s["containers"].([]any)[0].(map[string]any)["volumeMounts"] = []any{} },
		"runner-resources": func(s map[string]any) {
			s["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)["limits"] = map[string]any{"memory": "1Gi"}
		},
		"runner-lifecycle": func(s map[string]any) {
			s["containers"].([]any)[0].(map[string]any)["lifecycle"] = map[string]any{"postStart": map[string]any{"exec": map[string]any{"command": []any{"unsafe"}}}}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			pod := actual()
			mutate(pod["spec"].(map[string]any))
			if verifyPod(desiredJSON, encode(pod), runtimeImage) == nil {
				t.Fatal("accepted changed runtime")
			}
		})
	}
	for _, field := range []string{"containerStatuses", "initContainerStatuses"} {
		t.Run(field+"-unverified-image", func(t *testing.T) {
			pod := actual()
			pod["status"].(map[string]any)[field].([]any)[0].(map[string]any)["imageID"] =
				"ghcr.io/devantler-tech/ksail-analysis-runner@sha256:" + strings.Repeat("c", 64)
			if verifyPod(desiredJSON, encode(pod), runtimeImage) == nil {
				t.Fatal("accepted unrelated runtime digest")
			}
		})
	}
}
