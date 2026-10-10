package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCLIRequiresNativeAttemptAndNeverUsesRecoveryCheckoutAsOwner(t *testing.T) {
	s := fixture()
	b, _ := json.Marshal(struct{ State state }{s})
	var out bytes.Buffer
	if run("claim", bytes.NewReader(b), &out) == nil {
		t.Fatal("local mutation planner accepted")
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", "devantler-tech/platform")
	t.Setenv("GITHUB_REF", "refs/heads/gh-readonly-queue/main/pr-1")
	t.Setenv("GITHUB_RUN_ID", s.Owner.Run)
	t.Setenv("GITHUB_RUN_ATTEMPT", s.Owner.Attempt)
	t.Setenv("GITHUB_SHA", s.Owner.SHA)
	if err := run("claim", bytes.NewReader(b), &out); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_REF", "refs/pull/1/merge")
	if run("claim", bytes.NewReader(b), &out) == nil {
		t.Fatal("PR branch accepted")
	}
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("GITHUB_SHA", strings.Repeat("b", 40))
	if run("claim", bytes.NewReader(b), &out) == nil {
		t.Fatal("checkout SHA impersonated producer")
	}
}
