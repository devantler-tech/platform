package arcstaging_test

import (
	"reflect"
	"testing"
)

func TestAnalysisCapacityIsIsolatedAndKeepsTheClusterCeiling(t *testing.T) {
	config := readYAML(t, "ksail.prod.yaml")
	node := field(t, config, "spec", "cluster", "autoscaler", "node")
	equal(t, field(t, node, "maxNodesTotal"), 9)
	equal(t, field(t, config, "spec", "provider", "hetzner", "serverLimit"), 10)
	pools := field(t, node, "pools").([]any)
	count := 0
	for _, pool := range pools {
		if field(t, pool, "name") != "autoscale-ksail-analysis" {
			continue
		}
		count++
		equal(t, field(t, pool, "serverType"), "cx53")
		equal(t, field(t, pool, "min"), 0)
		equal(t, field(t, pool, "max"), 1)
		equal(t, field(t, pool, "labels", "platform.devantler.tech/ksail-analysis"), "enabled")
		wantTaints := []any{map[string]any{
			"key": "platform.devantler.tech/ksail-analysis", "value": "enabled", "effect": "NoSchedule",
		}}
		if !reflect.DeepEqual(field(t, pool, "taints"), wantTaints) {
			t.Fatal("analysis nodes must reject ordinary workload scheduling")
		}
	}
	equal(t, count, 1)
	buffer := readYAML(t, "k8s/providers/hetzner/infrastructure/overprovisioning/pod-template.yaml")
	if _, exists := field(t, buffer, "template", "spec").(map[string]any)["tolerations"]; exists {
		t.Fatal("ordinary warm-capacity buffers must not tolerate the analysis taint")
	}
}

func TestRunnerHasOnlyBoundedDisposableStorageAndRestrictedBootstrap(t *testing.T) {
	release := readYAML(t, "k8s/bases/infrastructure/ksail-analysis-runners/helm-release.yaml")
	spec := field(t, release, "spec", "values", "template", "spec")
	assertRunnerStorage(t, spec)
}

func assertRunnerStorage(t *testing.T, spec any) {
	t.Helper()
	equal(t, field(t, spec, "securityContext", "fsGroup"), 1001)
	wantVolumes := []any{
		map[string]any{"name": "runner-home", "emptyDir": map[string]any{"sizeLimit": "40Gi"}},
		map[string]any{"name": "runner-tmp", "emptyDir": map[string]any{"sizeLimit": "2Gi"}},
	}
	if !reflect.DeepEqual(field(t, spec, "volumes"), wantVolumes) {
		t.Fatal("only two explicitly bounded disk emptyDirs are permitted")
	}
	runner := field(t, spec, "containers").([]any)[0]
	equal(t, field(t, runner, "securityContext", "readOnlyRootFilesystem"), true)
	wantMounts := []any{
		map[string]any{"name": "runner-home", "mountPath": "/home/runner"},
		map[string]any{"name": "runner-tmp", "mountPath": "/tmp"},
	}
	if !reflect.DeepEqual(field(t, runner, "volumeMounts"), wantMounts) {
		t.Fatal("runner may mount only its disposable home and temporary storage")
	}
	initializers := field(t, spec, "initContainers").([]any)
	equal(t, len(initializers), 1)
	initial := initializers[0]
	equal(t, field(t, initial, "name"), "init-runner-home")
	equal(t, field(t, initial, "image"), field(t, runner, "image"))
	wantCommand := []any{"/bin/sh", "-ec", "cp -R /home/runner/. /runner-data/"}
	if !reflect.DeepEqual(field(t, initial, "command"), wantCommand) {
		t.Fatal("bootstrap must copy the baked runner without root-only preservation")
	}
	for _, name := range []string{"privileged", "allowPrivilegeEscalation"} {
		equal(t, field(t, initial, "securityContext", name), false)
	}
	equal(t, field(t, initial, "securityContext", "readOnlyRootFilesystem"), true)
	if !reflect.DeepEqual(field(t, initial, "securityContext", "capabilities", "drop"), []any{"ALL"}) {
		t.Fatal("bootstrap must drop every capability")
	}
	wantInitMounts := []any{map[string]any{"name": "runner-home", "mountPath": "/runner-data"}}
	if !reflect.DeepEqual(field(t, initial, "volumeMounts"), wantInitMounts) {
		t.Fatal("bootstrap may mount only the empty destination volume")
	}
	equal(t, field(t, initial, "resources", "requests", "memory"), "256Mi")
	equal(t, field(t, initial, "resources", "limits", "memory"), "512Mi")
}
