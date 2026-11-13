package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/devantler-tech/platform/internal/backupcatalogue"
)

func authorizedEnvironment() map[string]string {
	return map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_RUN_ATTEMPT": "1", "RUNNER_ENVIRONMENT": "github-hosted",
		"GITHUB_WORKFLOW_REF":        "devantler-tech/platform/.github/workflows/observe-historical-longhorn-backups.yaml@refs/heads/main",
		"LONGHORN_CATALOGUE_CONFIRM": "observe-historical-longhorn-backups", "GITHUB_SHA": strings.Repeat("a", 40), "GITHUB_RUN_ID": "123",
	}
}

func TestAuthorizationRequiresConfirmedFirstAttemptCurrentWorkflow(t *testing.T) {
	env := authorizedEnvironment()
	get := func(key string) string { return env[key] }
	if authorize(get) != nil {
		t.Fatal("valid authorization")
	}
	for key := range env {
		original := env[key]
		env[key] = ""
		if authorize(get) == nil {
			t.Errorf("missing %s accepted", key)
		}
		env[key] = original
	}
	for key, wrong := range map[string]string{"GITHUB_RUN_ATTEMPT": "2", "GITHUB_REF": "refs/heads/foreign", "RUNNER_ENVIRONMENT": "self-hosted", "GITHUB_WORKFLOW_REF": "foreign", "GITHUB_SHA": strings.Repeat("A", 40), "GITHUB_RUN_ID": "0"} {
		original := env[key]
		env[key] = wrong
		if authorize(get) == nil {
			t.Errorf("wrong %s accepted", key)
		}
		env[key] = original
	}
}

func TestReadSurfaceIsOnlyFixedGetsAndMainRef(t *testing.T) {
	for _, namespace := range []string{"wedding-app", "actual-budget"} {
		for _, path := range documentPaths(namespace) {
			args := []string{"--context=admin@prod", "--request-timeout=20s", "get", "--raw=" + path}
			if !readAllowed("kubectl", args...) {
				t.Fatal("canonical GET refused")
			}
			for _, wrong := range [][]string{
				append(append([]string{}, args...), "--as=other"),
				{"--context=admin@prod", "--request-timeout=20s", "delete", "--raw=" + path},
				{"--context=admin@prod", "--request-timeout=20s", "get", "--raw=" + path + "?watch=true"},
				{"--context=foreign", "--request-timeout=20s", "get", "--raw=" + path},
			} {
				if readAllowed("kubectl", wrong...) {
					t.Fatal("foreign or mutating read scope")
				}
			}
		}
	}
	for _, path := range []string{"/api/v1/secrets", "/api/v1/namespaces/velero/secrets/foreign", "https://foreign.invalid/api/v1/secrets"} {
		if readAllowed("kubectl", "--context=admin@prod", "--request-timeout=20s", "get", "--raw="+path) {
			t.Fatal("foreign data scope")
		}
	}
	if readAllowed("sh", "-c", "anything") || readAllowed("git", "ls-remote", "https://foreign.invalid", "refs/heads/main") {
		t.Fatal("foreign executable or Git remote")
	}
}

func TestRunNeverPublishesPartialOrStaleMetadata(t *testing.T) {
	for _, mode := range []string{"stable", "stale-before", "stale-between", "stale-after", "second-held", "bad-receipt"} {
		t.Run(mode, func(t *testing.T) {
			env := authorizedEnvironment()
			get := func(k string) string { return env[k] }
			checks, observations := 0, 0
			runner := func(_ context.Context, program string, args ...string) ([]byte, error) {
				if program != "git" || !readAllowed(program, args...) {
					t.Fatal("unexpected command")
				}
				checks++
				sha := env["GITHUB_SHA"]
				if mode == "stale-before" && checks == 1 || mode == "stale-between" && checks == 2 || mode == "stale-after" && checks == 3 {
					sha = strings.Repeat("b", 40)
				}
				return []byte(sha + "\trefs/heads/main\n"), nil
			}
			observe := func(context.Context, string, backupcatalogue.Policy, func(context.Context) (backupcatalogue.DeploymentDocuments, error), string) (backupcatalogue.Catalogue, error) {
				observations++
				if mode == "second-held" && observations == 2 {
					return backupcatalogue.Catalogue{}, errHeld
				}
				result := backupcatalogue.Catalogue{MetadataOnly: true, Snapshots: 1, Digest: strings.Repeat("c", 64), RepositoryDigest: strings.Repeat("d", 64)}
				if mode == "bad-receipt" {
					result.MetadataOnly = false
				}
				return result, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			result, err := run(ctx, get, runner, observe, backupcatalogue.Policy{}, t.TempDir())
			if mode == "stable" {
				if err != nil || len(result.Repositories) != 2 || checks != 3 || observations != 2 {
					t.Fatal("complete current observation refused")
				}
				return
			}
			if !errors.Is(err, errHeld) || len(result.Repositories) != 0 {
				t.Fatal("partial or stale receipt escaped")
			}
			if mode == "stale-before" && observations != 0 || mode == "stale-between" && observations != 1 {
				t.Fatal("read after source went stale")
			}
		})
	}
}

type delayedCancellationContext struct {
	context.Context
	deadline time.Time
}

func (c *delayedCancellationContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestWallDeadlinePassingDuringFinalSourceReadDiscardsReceipt(t *testing.T) {
	env := authorizedEnvironment()
	ctx := &delayedCancellationContext{Context: context.Background(), deadline: time.Now().Add(time.Minute)}
	calls := 0
	execute := func(context.Context, string, ...string) ([]byte, error) {
		calls++
		if calls == 3 {
			// Model scheduler-delayed cancellation notification: the real wall
			// deadline has passed, but Err and Done have not changed yet.
			ctx.deadline = time.Now().Add(-time.Second)
		}
		return []byte(env["GITHUB_SHA"] + "\trefs/heads/main\n"), nil
	}
	observe := func(context.Context, string, backupcatalogue.Policy, func(context.Context) (backupcatalogue.DeploymentDocuments, error), string) (backupcatalogue.Catalogue, error) {
		return backupcatalogue.Catalogue{MetadataOnly: true, Digest: strings.Repeat("c", 64), RepositoryDigest: strings.Repeat("d", 64)}, nil
	}
	result, err := run(ctx, func(k string) string { return env[k] }, execute, observe, backupcatalogue.Policy{}, t.TempDir())
	if !errors.Is(err, errHeld) || len(result.Repositories) != 0 {
		t.Fatal("expired final source observation produced a receipt")
	}
}
