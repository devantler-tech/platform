package recovery_test

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"cel.dev/cel-go/cel"
	"gopkg.in/yaml.v3"
)

func render(t *testing.T, path string) []map[string]any {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "../../..")
	out, err := exec.Command("kubectl", "kustomize", filepath.Join(root, path)).CombinedOutput()
	if err != nil {
		t.Fatalf("render %s: %v: %s", path, err, out)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	var objects []map[string]any
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			return objects
		} else if err != nil {
			t.Fatal(err)
		}
		if object != nil {
			objects = append(objects, object)
		}
	}
}

func object(t *testing.T, objects []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, object := range objects {
		if object["kind"] == kind && object["metadata"].(map[string]any)["name"] == name {
			return object
		}
	}
	t.Fatalf("missing %s %s", kind, name)
	return nil
}

func TestProductionRecoveryRetiresCopyBeforeWriterCutover(t *testing.T) {
	for _, resource := range render(t, "k8s/providers/hetzner/infrastructure") {
		metadata := resource["metadata"].(map[string]any)
		if metadata["namespace"] != "openbao" {
			continue
		}
		if metadata["name"] == "copy-openbao-snapshots-to-v2" ||
			metadata["name"] == "isolate-openbao-snapshot-copy" {
			t.Fatal("production must stop reconciling the completed archive copy before changing the writer")
		}
	}
}

// TestCutoverMovesTheBackupWriterToV2 fences the storage handoff.
func TestCutoverMovesTheBackupWriterToV2(t *testing.T) {
	production := render(t, "k8s/providers/hetzner/infrastructure")
	replacement := object(t, production, "PersistentVolumeClaim", "vault-snapshots-recovery-v2")
	replacementMetadata := replacement["metadata"].(map[string]any)
	replacementSpec := replacement["spec"].(map[string]any)
	if replacementMetadata["namespace"] != "openbao" || replacementMetadata["annotations"].(map[string]any)["kustomize.toolkit.fluxcd.io/prune"] != "disabled" ||
		replacementSpec["storageClassName"] != "longhorn-v2" || replacementSpec["volumeName"] != nil ||
		replacementSpec["volumeMode"] != "Filesystem" || replacementSpec["resources"].(map[string]any)["requests"].(map[string]any)["storage"] != "10Gi" {
		t.Fatal("the V2 claim must retain its original capacity and controller-managed binding")
	}
	labels, _ := replacementMetadata["labels"].(map[string]any)
	if labels["platform.devantler.tech/storage-stage"] != nil ||
		labels["backup.platform.devantler.tech/volume-data"] != nil {
		t.Fatal("serving V2 must bind for health checks and preserve ordinary Velero eligibility")
	}
	if modes := replacementSpec["accessModes"].([]any); len(modes) != 1 || modes[0] != "ReadWriteOnce" {
		t.Fatal("the V2 claim must preserve single-writer access")
	}
	for _, name := range []string{"vault-snapshots", "vault-snapshots-recovery"} {
		claim := object(t, production, "PersistentVolumeClaim", name)
		metadata := claim["metadata"].(map[string]any)
		spec := claim["spec"].(map[string]any)
		if metadata["namespace"] != "openbao" || metadata["annotations"].(map[string]any)["kustomize.toolkit.fluxcd.io/prune"] != "disabled" {
			t.Fatal("both claims must remain independently retained in the original namespace")
		}
		if spec["storageClassName"] != "hcloud" || spec["resources"].(map[string]any)["requests"].(map[string]any)["storage"] != "10Gi" {
			t.Fatal("recovery must preserve the declared storage class and capacity")
		}
		modes := spec["accessModes"].([]any)
		if len(modes) != 1 || modes[0] != "ReadWriteOnce" {
			t.Fatal("snapshot storage must preserve single-writer access")
		}
		if _, assigned := spec["volumeName"]; assigned {
			t.Fatal("recovery must not rebind the missing provider volume")
		}
		if name == "vault-snapshots-recovery" {
			labels, _ := metadata["labels"].(map[string]any)
			if labels["backup.platform.devantler.tech/volume-data"] != "independently-mirrored" {
				t.Fatal("the retained Hetzner source must preserve its independent mirror classification")
			}
			if _, marked := labels["platform.devantler.tech/storage-stage"]; marked {
				t.Fatal("active recovery storage must leave the prepared-only health exemption")
			}
		}
	}
	for _, workload := range []struct{ kind, name string }{{"Job", "vault-snapshot-init"}, {"CronJob", "vault-snapshot"}} {
		spec := object(t, production, workload.kind, workload.name)["spec"].(map[string]any)
		if workload.kind == "CronJob" {
			spec = spec["jobTemplate"].(map[string]any)["spec"].(map[string]any)
		}
		pod := spec["template"].(map[string]any)["spec"].(map[string]any)
		found := false
		for _, volume := range pod["volumes"].([]any) {
			claim, ok := volume.(map[string]any)["persistentVolumeClaim"].(map[string]any)
			if !ok {
				continue
			}
			if claim["claimName"] == "vault-snapshots" || claim["claimName"] == "vault-snapshots-recovery" {
				t.Fatalf("%s must stop mounting both old provider claim names", workload.name)
			}
			found = found || claim["claimName"] == "vault-snapshots-recovery-v2"
		}
		if !found {
			t.Fatalf("%s must mount the main-owned retained replacement", workload.name)
		}
		strictMirror := false
		for _, container := range pod["containers"].([]any) {
			c := container.(map[string]any)
			if c["name"] != "mirror" {
				continue
			}
			for _, entry := range c["env"].([]any) {
				e := entry.(map[string]any)
				strictMirror = strictMirror || e["name"] == "REQUIRE_REMOTE_SNAPSHOT" && e["value"] == "true"
			}
		}
		if !strictMirror {
			t.Fatal("production snapshots must not succeed without their off-cluster mirror")
		}
	}
	initial := object(t, production, "Job", "vault-snapshot-init")
	annotations := initial["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations["kustomize.toolkit.fluxcd.io/force"] != "enabled" {
		t.Fatal("Flux must be able to replace this immutable snapshot Job on the storage switch")
	}
	// Bootstrap reads the offsite mirror into scratch space. Keep it independent
	// of both the failed RWO claim and its retained replacement.
	bootstrap := object(t, production, "Job", "vault-config")["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	foundScratch := false
	for _, volume := range bootstrap["volumes"].([]any) {
		v := volume.(map[string]any)
		if v["name"] == "snapshots" {
			if _, scratch := v["emptyDir"]; !scratch {
				t.Fatal("bootstrap restore must remain independent of snapshot PVC binding")
			}
			foundScratch = true
		}
	}
	if !foundScratch {
		t.Fatal("bootstrap restore lost its snapshot scratch space")
	}
	for _, resource := range render(t, "k8s/providers/docker/infrastructure") {
		if resource["kind"] == "PersistentVolumeClaim" && resource["metadata"].(map[string]any)["name"] == "vault-snapshots-recovery" {
			t.Fatal("provider recovery storage must not enter local deployments")
		}
	}
}

// TestActiveSnapshotStorageRequiresBinding evaluates the deployed CEL predicates.
func TestActiveSnapshotStorageRequiresBinding(t *testing.T) {
	infrastructure := object(t, render(t, "k8s/clusters/prod"), "Kustomization", "infrastructure")["spec"].(map[string]any)
	if infrastructure["wait"] != true || infrastructure["force"] != false {
		t.Fatal("recovery must retain health waiting and immutable-conflict protection")
	}
	var current, failed string
	for _, item := range infrastructure["healthCheckExprs"].([]any) {
		check := item.(map[string]any)
		if check["apiVersion"] == "v1" && check["kind"] == "PersistentVolumeClaim" {
			current, _ = check["current"].(string)
			failed, _ = check["failed"].(string)
		}
	}
	if current == "" || failed == "" {
		t.Fatal("active snapshot storage needs its binding health predicate")
	}
	env, err := cel.NewEnv(cel.Variable("metadata", cel.DynType), cel.Variable("spec", cel.DynType), cel.Variable("status", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	compile := func(expression string) cel.Program {
		ast, issues := env.Compile(expression)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
	}
	ready, lost := compile(current), compile(failed)
	for _, tt := range []struct {
		name, phase string
		claim       string
		mutate      func(map[string]any, map[string]any)
		want        bool
		wantFailed  bool
	}{
		{name: "old prepared marker cannot bypass binding", phase: "Pending", mutate: func(m, _ map[string]any) {
			m["labels"] = map[string]any{"platform.devantler.tech/storage-stage": "prepared"}
		}},
		{name: "a stale preparation label cannot bypass V2 binding", phase: "Pending", claim: "vault-snapshots-recovery-v2", mutate: func(m, _ map[string]any) {
			m["labels"] = map[string]any{"platform.devantler.tech/storage-stage": "prepared"}
		}},
		{name: "a selected V2 consumer must bind", phase: "Pending", claim: "vault-snapshots-recovery-v2", mutate: func(m, _ map[string]any) {
			m["annotations"].(map[string]any)["volume.kubernetes.io/selected-node"] = "worker"
		}},
		{name: "the V2 claim must bind after serving starts", phase: "Pending", claim: "vault-snapshots-recovery-v2", mutate: func(m, _ map[string]any) {
			delete(m, "labels")
		}},
		{name: "bound V2 claim", phase: "Bound", claim: "vault-snapshots-recovery-v2", want: true},
		{name: "bound claim", phase: "Bound", want: true},
		{name: "lost claim", phase: "Lost", wantFailed: true},
		{name: "unobserved status"},
		{name: "unknown status", phase: "Unknown"},
		{name: "old failed claim cannot bypass binding", phase: "Pending", mutate: func(m, _ map[string]any) { m["name"] = "vault-snapshots" }},
		{name: "other namespace", phase: "Pending", mutate: func(m, _ map[string]any) { m["namespace"] = "other" }},
		{name: "other storage class", phase: "Pending", mutate: func(_, s map[string]any) { s["storageClassName"] = "other" }},
		{name: "unmarked", phase: "Pending", mutate: func(m, _ map[string]any) { delete(m, "labels") }},
		{name: "active", phase: "Pending", mutate: func(m, _ map[string]any) {
			m["labels"] = map[string]any{"platform.devantler.tech/storage-stage": "active"}
		}},
		{name: "assigned volume", phase: "Pending", mutate: func(_, s map[string]any) { s["volumeName"] = "existing-volume" }},
		{name: "selected consumer", phase: "Pending", mutate: func(m, _ map[string]any) {
			m["annotations"].(map[string]any)["volume.kubernetes.io/selected-node"] = "worker"
		}},
		{name: "terminating bound claim", phase: "Bound", mutate: func(m, _ map[string]any) { m["deletionTimestamp"] = "2026-10-26T00:00:00Z" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name := tt.claim
			if name == "" {
				name = "vault-snapshots-recovery"
			}
			claim := object(t, render(t, "k8s/providers/hetzner/infrastructure/vault-backup-recovery"), "PersistentVolumeClaim", name)
			metadata, spec := claim["metadata"].(map[string]any), claim["spec"].(map[string]any)
			if tt.mutate != nil {
				tt.mutate(metadata, spec)
			}
			status := map[string]any{}
			if tt.phase != "" {
				status["phase"] = tt.phase
			}
			for _, check := range []struct {
				program cel.Program
				want    bool
			}{{ready, tt.want}, {lost, tt.wantFailed}} {
				result, _, err := check.program.Eval(map[string]any{"metadata": metadata, "spec": spec, "status": status})
				if err != nil || result.Value() != check.want {
					t.Fatalf("health=%v err=%v, want %t", result, err, check.want)
				}
			}
		})
	}
}
