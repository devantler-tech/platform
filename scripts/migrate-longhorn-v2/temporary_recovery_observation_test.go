package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTemporaryRecoveryObservationUsesItsOwnConfirmation(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_NODE"] = "temporary-v1-2"
	env["LONGHORN_V2_OPERATION"] = observeTemporaryRecoveryOperation
	env["LONGHORN_V2_CONFIRM"] = "observe-longhorn-v2-recovery-temporary-v1-2"
	env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/observe-longhorn-temporary-recovery.yaml@refs/heads/main"
	getenv := func(key string) string { return env[key] }
	if err := authorizeTemporaryRecoveryObservation(getenv, "temporary-v1-2"); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []struct{ key, value string }{
		{"LONGHORN_V2_CONFIRM", "prepare-unused-longhorn-v2-temporary-v1-2"},
		{"LONGHORN_V2_DATABASE", "coroot-db"},
		{"LONGHORN_V2_NODE", "prod-worker-2"},
		{"GITHUB_RUN_ATTEMPT", "2"},
		{"GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/prepare-longhorn-v2-node.yaml@refs/heads/main"},
	} {
		original := env[fault.key]
		env[fault.key] = fault.value
		if err := authorizeTemporaryRecoveryObservation(getenv, "temporary-v1-2"); err == nil {
			t.Fatalf("the substituted %s was admitted", fault.key)
		}
		env[fault.key] = original
	}
}

func TestObserveTemporaryRecoveryPreservesTheOriginalJournalAndCordon(t *testing.T) {
	f, p := unusedPreparationAPI(t)
	p.staging = true
	initial, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.afterReset = cancel
	if err := p.prepare(initial); !errors.Is(err, context.Canceled) || f.base.resets != 1 || p.j.Phase != "reset-requested" {
		t.Fatalf("need a journaled and interrupted reset: %v", err)
	}
	original := text(f.base.cm, "data", "record")
	p.owner = "6789"
	p.c.run = func(ctx context.Context, input []byte, tool string, args ...string) ([]byte, error) {
		if input != nil || !temporaryRecoveryReadAllowed(tool, args) {
			t.Fatalf("observation escaped the protected read surface: %s %v", tool, args)
		}
		return f.run(ctx, input, tool, args...)
	}
	if err := p.observeRecovery(t.Context()); err != nil {
		t.Fatal(err)
	}
	if text(f.base.cm, "data", "record") != original || f.base.resets != 1 ||
		value(f.base.node, "spec", "unschedulable") != true {
		t.Fatal("observation modified the reset journal or released the worker")
	}
	p.c.run = func(ctx context.Context, input []byte, tool string, args ...string) ([]byte, error) {
		if tool == "talosctl" && len(args) == 5 && args[2] == "volumeconfig" {
			return nil, errors.New("private volume diagnostics")
		}
		return f.run(ctx, input, tool, args...)
	}
	if err := p.observeRecovery(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "readable Talos volume configuration") ||
		strings.Contains(err.Error(), "private volume diagnostics") {
		t.Fatalf("expected the qualified stage without native diagnostics: %v", err)
	}
}

func TestTemporaryRecoveryObservationRejectsWritesAndUnrelatedReads(t *testing.T) {
	for _, command := range []struct {
		program string
		args    []string
	}{
		{"kubectl", []string{"--context=admin@prod", "--request-timeout=20s", "patch", "node", "autoscale-longhorn-v1-0123456789abcdef"}},
		{"kubectl", []string{"--context=admin@prod", "--request-timeout=20s", "get", "secret", "unrelated", "--output=json"}},
		{"talosctl", []string{"--nodes=10.0.1.3", "reset", "--reboot", "--wait", "--graceful"}},
		{"talosctl", []string{"--nodes=10.0.1.3", "get", "machineconfig", "v1alpha1", "--output=json"}},
		{"talosctl", []string{"--nodes=192.0.2.10", "get", "volumeconfig", "EPHEMERAL", "--output=json"}},
		{"gh", []string{"api", "repos/devantler-tech/platform/actions/secrets"}},
	} {
		if temporaryRecoveryReadAllowed(command.program, command.args) {
			t.Fatalf("observation admitted %s %v", command.program, command.args)
		}
	}
}
