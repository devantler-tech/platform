package preparation_test

import (
	"testing"

	"cel.dev/cel-go/cel"
)

// The storage-only stage has no consumer, so a WFFC claim is correctly Pending.
// Serving mail must instead wait for that same claim to bind. Evaluate the
// rendered Flux expression against both rendered lifecycle states.
func TestMailStorageHealthAcrossActivation(t *testing.T) {
	apps := object(t, render(t, "k8s/clusters/prod"), "Kustomization", "apps")
	spec := mapping(t, apps["spec"])
	if spec["wait"] != true {
		t.Fatal("application health waiting must remain enabled")
	}
	var expression, failedExpression string
	for _, item := range sequence(t, spec["healthCheckExprs"]) {
		check := mapping(t, item)
		if check["apiVersion"] == "v1" && check["kind"] == "PersistentVolumeClaim" {
			expression, _ = check["current"].(string)
			failedExpression, _ = check["failed"].(string)
		}
	}
	if expression == "" {
		t.Fatal("staged mail storage needs an explicit Pending-versus-serving health decision")
	}
	env, err := cel.NewEnv(
		cel.Variable("metadata", cel.DynType),
		cel.Variable("spec", cel.DynType),
		cel.Variable("status", cel.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	failedAST, issues := env.Compile(failedExpression)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	failedProgram, err := env.Program(failedAST)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, phase, path string
		mutate            func(map[string]any, map[string]any)
		want              bool
		wantFailed        bool
	}{
		{name: "inactive WFFC storage", phase: "Pending", want: true},
		{name: "retained bound storage", phase: "Bound", want: true},
		{name: "lost staged storage", phase: "Lost", wantFailed: true},
		{name: "status not observed"},
		{name: "serving must bind", phase: "Pending", path: "k8s/bases/apps/private-mail"},
		{name: "serving bound", phase: "Bound", path: "k8s/bases/apps/private-mail", want: true},
		{name: "production serving must bind", phase: "Pending", path: "k8s/providers/hetzner/apps"},
		{name: "production serving bound", phase: "Bound", path: "k8s/providers/hetzner/apps", want: true},
		{name: "another name", phase: "Pending", mutate: func(m, _ map[string]any) { m["name"] = "other-data" }},
		{name: "another namespace", phase: "Pending", mutate: func(m, _ map[string]any) { m["namespace"] = "other" }},
		{name: "another storage class", phase: "Pending", mutate: func(_, s map[string]any) { s["storageClassName"] = "longhorn" }},
		{name: "volume assignment has started", phase: "Pending", mutate: func(_, s map[string]any) { s["volumeName"] = "mail-volume" }},
		{name: "unmarked storage", phase: "Pending", mutate: func(m, _ map[string]any) { delete(m, "labels") }},
		{name: "another lifecycle marker", phase: "Pending", mutate: func(m, _ map[string]any) {
			mapping(t, m["labels"])["platform.devantler.tech/storage-stage"] = "serving"
		}},
		{name: "provisioning has started", phase: "Pending", mutate: func(m, _ map[string]any) {
			mapping(t, m["annotations"])["volume.kubernetes.io/selected-node"] = "worker"
		}},
		{name: "bound volume terminating", phase: "Bound", mutate: func(m, _ map[string]any) {
			m["deletionTimestamp"] = "2026-10-26T00:00:00Z"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path == "" {
				path = "k8s/bases/apps/private-mail-storage"
			}
			claim := object(t, render(t, path), "PersistentVolumeClaim", "private-mail-data")
			metadata, claimSpec := mapping(t, claim["metadata"]), mapping(t, claim["spec"])
			// Kubernetes assigns the configured default WFFC class at admission.
			claimSpec["storageClassName"] = "longhorn-wffc"
			if tt.mutate != nil {
				tt.mutate(metadata, claimSpec)
			}
			observed := map[string]any{}
			if tt.phase != "" {
				observed["phase"] = tt.phase
			}
			result, _, err := program.Eval(map[string]any{"metadata": metadata, "spec": claimSpec, "status": observed})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := result.Value().(bool); !ok || got != tt.want {
				t.Fatalf("storage ready = %v, want %v", result, tt.want)
			}
			failed, _, err := failedProgram.Eval(map[string]any{"metadata": metadata, "spec": claimSpec, "status": observed})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := failed.Value().(bool); !ok || got != tt.wantFailed {
				t.Fatalf("storage failed = %v, want %v", failed, tt.wantFailed)
			}
		})
	}
}
