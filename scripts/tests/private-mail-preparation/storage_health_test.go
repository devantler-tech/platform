package preparation_test

import (
	"testing"

	"cel.dev/cel-go/cel"
)

// claimHealth evaluates the rendered Flux PersistentVolumeClaim health
// expressions for one observed claim state.
type claimHealth struct{ current, failed cel.Program }

func appsClaimHealth(t *testing.T) claimHealth {
	t.Helper()
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
	compile := func(source string) cel.Program {
		ast, issues := env.Compile(source)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
	}
	return claimHealth{current: compile(expression), failed: compile(failedExpression)}
}

func (h claimHealth) assert(t *testing.T, metadata, claimSpec map[string]any, phase string, want, wantFailed bool) {
	t.Helper()
	observed := map[string]any{}
	if phase != "" {
		observed["phase"] = phase
	}
	input := map[string]any{"metadata": metadata, "spec": claimSpec, "status": observed}
	result, _, err := h.current.Eval(input)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := result.Value().(bool); !ok || got != want {
		t.Fatalf("storage ready = %v, want %v", result, want)
	}
	failed, _, err := h.failed.Eval(input)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := failed.Value().(bool); !ok || got != wantFailed {
		t.Fatalf("storage failed = %v, want %v", failed, wantFailed)
	}
}

// The storage-only stage has no consumer, so a WFFC claim is correctly Pending.
// Serving mail must instead wait for that same claim to bind. Evaluate the
// rendered Flux expression against both rendered lifecycle states.
func TestMailStorageHealthAcrossActivation(t *testing.T) {
	health := appsClaimHealth(t)
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
		{name: "the encrypted class", phase: "Pending", mutate: func(_, s map[string]any) { s["storageClassName"] = "email-encrypted" }},
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
			health.assert(t, metadata, claimSpec, tt.phase, tt.want, tt.wantFailed)
		})
	}
}

// No workload selects the prepared encrypted claim, so on a new cluster it
// stays Pending under WFFC. That must not hold application readiness, while any
// sign that binding has started still waits for Bound. Production serves from
// it, so there it must bind (#5387).
func TestEncryptedMailStorageHealthAcrossServing(t *testing.T) {
	health := appsClaimHealth(t)
	tests := []struct {
		name, phase, path, claim string
		mutate                   func(map[string]any, map[string]any)
		want                     bool
		wantFailed               bool
	}{
		{name: "prepared encrypted storage", phase: "Pending", want: true},
		{name: "prepared encrypted V2 claim", phase: "Pending", claim: "email-data-encrypted-v2", want: true},
		{name: "production mounts the V2 claim and waits for binding", phase: "Pending", path: "k8s/providers/hetzner/apps", claim: "email-data-encrypted-v2"},
		{name: "production V2 claim is bound", phase: "Bound", path: "k8s/providers/hetzner/apps", claim: "email-data-encrypted-v2", want: true},
		{name: "encrypted V2 with the older class", phase: "Pending", claim: "email-data-encrypted-v2",
			mutate: func(_, s map[string]any) { s["storageClassName"] = "email-encrypted" }},
		{name: "encrypted V1 with the V2 class", phase: "Pending",
			mutate: func(_, s map[string]any) { s["storageClassName"] = "email-encrypted-v2" }},
		{name: "V2 provisioning has started", phase: "Pending", claim: "email-data-encrypted-v2", mutate: func(m, _ map[string]any) {
			mapping(t, m["annotations"])["volume.kubernetes.io/selected-node"] = "worker"
		}},
		{name: "V2 binding has started", phase: "Pending", claim: "email-data-encrypted-v2",
			mutate: func(_, s map[string]any) { s["volumeName"] = "mail-v2-volume" }},
		{name: "production serving must bind", phase: "Pending", path: "k8s/providers/hetzner/apps"},
		{name: "production serving bound", phase: "Bound", path: "k8s/providers/hetzner/apps", want: true},
		{name: "retained bound storage", phase: "Bound", want: true},
		{name: "lost storage", phase: "Lost", wantFailed: true},
		{name: "status not observed"},
		{name: "another name", phase: "Pending", mutate: func(m, _ map[string]any) { m["name"] = "other-data" }},
		{name: "another namespace", phase: "Pending", mutate: func(m, _ map[string]any) { m["namespace"] = "other" }},
		{name: "the original claim's class", phase: "Pending", mutate: func(_, s map[string]any) { s["storageClassName"] = "longhorn-wffc" }},
		{name: "no storage class", phase: "Pending", mutate: func(_, s map[string]any) { delete(s, "storageClassName") }},
		{name: "volume assignment has started", phase: "Pending", mutate: func(_, s map[string]any) { s["volumeName"] = "mail-volume" }},
		{name: "unmarked storage", phase: "Pending", mutate: func(m, _ map[string]any) { delete(m, "labels") }},
		{name: "another lifecycle marker", phase: "Pending", mutate: func(m, _ map[string]any) {
			mapping(t, m["labels"])["platform.devantler.tech/storage-stage"] = "serving"
		}},
		{name: "provisioning has started", phase: "Pending", mutate: func(m, _ map[string]any) {
			mapping(t, m["annotations"])["volume.kubernetes.io/selected-node"] = "worker"
		}},
		{name: "bound volume terminating", phase: "Bound", mutate: func(m, _ map[string]any) {
			m["deletionTimestamp"] = "2026-11-14T00:00:00Z"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path == "" {
				path = "k8s/bases/apps/email-encrypted-storage"
			}
			name := tt.claim
			if name == "" {
				name = "email-data-encrypted"
			}
			claim := object(t, render(t, path), "PersistentVolumeClaim", name)
			metadata, claimSpec := mapping(t, claim["metadata"]), mapping(t, claim["spec"])
			if tt.mutate != nil {
				tt.mutate(metadata, claimSpec)
			}
			health.assert(t, metadata, claimSpec, tt.phase, tt.want, tt.wantFailed)
		})
	}
}
