package main

import (
	"strings"
	"testing"
)

// barmanProcessFixture is synthetic; its image pins come from verified public OCI manifests.
func barmanProcessFixture() (object, object, object, object) {
	pod, dep, rs, node := operatorProcessFixture()
	dep["metadata"].(object)["name"] = "plugin-barman-cloud"
	dep["metadata"].(object)["uid"] = "barman-deployment-uid"
	rs["metadata"].(object)["name"] = "barman-fixture"
	rs["metadata"].(object)["uid"] = "barman-replicaset-uid"
	pod["metadata"].(object)["name"] = "barman-fixture-one"
	pod["metadata"].(object)["uid"] = "barman-pod-uid"
	value(pod, "metadata", "ownerReferences").([]any)[0].(object)["name"] = "barman-fixture"
	value(pod, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "barman-replicaset-uid"
	for _, r := range []object{dep, rs} {
		r["spec"].(object)["replicas"] = float64(1)
		for _, field := range []string{"replicas", "readyReplicas", "availableReplicas"} {
			r["status"].(object)[field] = float64(1)
		}
	}
	dep["status"].(object)["updatedReplicas"] = float64(1)
	dep["spec"].(object)["strategy"] = object{"type": "Recreate"}
	value(rs, "metadata", "ownerReferences").([]any)[0].(object)["name"] = "plugin-barman-cloud"
	value(rs, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "barman-deployment-uid"
	container := object{"name": "barman-cloud", "image": "ghcr.io/cloudnative-pg/plugin-barman-cloud:v0.16.0@sha256:6404a416718bd7aad943c67a25a7bb20d0558b43edfeab0bf167f013ca435882", "args": []any{"operator", "--server-cert=/server/tls.crt", "--server-key=/server/tls.key", "--client-cert=/client/tls.crt", "--server-address=:9090", "--leader-elect", "--log-level=info"}, "env": []any{object{"name": "SIDECAR_IMAGE", "value": "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.16.0@sha256:c669259e20ed1e11fad821f735c396c19d929985aa05b9544b13ecc8b5a386d0"}}}
	value(pod, "spec").(object)["containers"] = []any{container}
	value(dep, "spec", "template", "spec").(object)["containers"] = []any{container}
	status := value(pod, "status", "containerStatuses").([]any)[0].(object)
	status["name"] = "barman-cloud"
	status["containerID"] = "containerd://" + strings.Repeat("c", 64)
	status["imageID"] = "ghcr.io/cloudnative-pg/plugin-barman-cloud@sha256:6404a416718bd7aad943c67a25a7bb20d0558b43edfeab0bf167f013ca435882"
	return pod, dep, rs, node
}

// TestBarmanProcessBindsPinnedOperatorAndExplicitSidecar refuses inferring a process env from a mutable ConfigMap.
func TestBarmanProcessBindsPinnedOperatorAndExplicitSidecar(t *testing.T) {
	pod, dep, rs, node := barmanProcessFixture()
	bound, err := bindBarmanOperatorProcess(pod, dep, rs, node)
	if err != nil || bound.PodUID != "barman-pod-uid" || bound.Container == "" || bound.ControllerSHA == "" {
		t.Fatalf("pinned Barman process refused: %+v %v", bound, err)
	}
}

// TestBarmanProcessRefusesUnboundReleaseOrConfiguration covers actual process provenance failures.
func TestBarmanProcessRefusesUnboundReleaseOrConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(object, object, object, object){
		"mutable environment": func(p, _, _, _ object) {
			value(p, "spec", "containers").([]any)[0].(object)["env"] = []any{object{"name": "SIDECAR_IMAGE", "valueFrom": object{"configMapKeyRef": object{"name": "plugin-barman-cloud-config", "key": "SIDECAR_IMAGE"}}}}
		},
		"mutable operator": func(p, _, _, _ object) {
			value(p, "spec", "containers").([]any)[0].(object)["image"] = "ghcr.io/cloudnative-pg/plugin-barman-cloud:v0.16.0"
		},
		"foreign runtime": func(p, _, _, _ object) {
			value(p, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "ghcr.io/cloudnative-pg/plugin-barman-cloud@sha256:" + strings.Repeat("a", 64)
		},
		"wrong platform": func(p, _, _, n object) {
			value(n, "status", "nodeInfo").(object)["architecture"] = "arm64"
			value(p, "status", "containerStatuses").([]any)[0].(object)["imageID"] = "ghcr.io/cloudnative-pg/plugin-barman-cloud@sha256:44860fd00b3d294bf501c059dc2832822485aa26c2d5a29c31572ff765869000"
		},
		"wrapper": func(p, _, _, _ object) {
			value(p, "spec", "containers").([]any)[0].(object)["command"] = []any{"/bin/sh", "-c", "/manager operator"}
		},
		"foreign owner": func(p, _, _, _ object) {
			value(p, "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "foreign"
		},
		"unready": func(p, _, _, _ object) { value(p, "status", "containerStatuses").([]any)[0].(object)["ready"] = false },
		"rolling": func(_, d, _, _ object) { d["spec"].(object)["strategy"] = object{"type": "RollingUpdate"} },
	} {
		t.Run(name, func(t *testing.T) {
			pod, dep, rs, node := barmanProcessFixture()
			mutate(pod, dep, rs, node)
			if got, err := bindBarmanOperatorProcess(pod, dep, rs, node); err == nil || got != (cnpgOperatorProcess{}) {
				t.Fatal("unbound Barman process accepted")
			}
		})
	}
}
