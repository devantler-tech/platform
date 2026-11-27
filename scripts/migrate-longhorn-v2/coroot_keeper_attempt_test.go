package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestKeeperRecoveryBindsFirstAttemptAndRequiresLatestTerminal(t *testing.T) {
	const run = "991"
	const endpoint = "repos/devantler-tech/platform/actions/runs/" + run
	const workflow = ".github/workflows/prepare-longhorn-v2-node.yaml"
	source := strings.Repeat("a", 40)
	finished := finishedMigrationRun{
		ID: 991, Status: "completed", Conclusion: "failure", Source: source,
		Branch: "main", Event: "workflow_dispatch", Attempt: 1, Path: workflow,
	}
	later := finished
	later.Attempt, later.Conclusion = 2, "success"
	active := later
	active.Status = "in_progress"
	changed := finished
	changed.ID++

	for _, tt := range []struct {
		name          string
		latest, first finishedMigrationRun
		want          string
		calls         []string
	}{
		{"first and latest", finished, finished, "failure", []string{endpoint}},
		{"first interrupted before a terminal rerun", later, finished, "failure", []string{endpoint, endpoint + "/attempts/1"}},
		{"rerun still active", active, finished, "", []string{endpoint}},
		{"attempt response belongs to another run", later, changed, "", []string{endpoint, endpoint + "/attempts/1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			c := client{run: func(_ context.Context, stdin []byte, command string, args ...string) ([]byte, error) {
				if stdin != nil || command != "gh" || len(args) != 2 || args[0] != "api" {
					t.Fatalf("unexpected command: %s %v", command, args)
				}
				calls = append(calls, args[1])
				switch args[1] {
				case endpoint:
					return json.Marshal(tt.latest)
				case endpoint + "/attempts/1":
					return json.Marshal(tt.first)
				default:
					t.Fatalf("unexpected API route: %s", args[1])
					return nil, nil
				}
			}}
			got, err := readFinishedKeeperFirstAttempt(t.Context(), c, run, source)
			if got != tt.want || (err != nil) != (tt.want == "") || !reflect.DeepEqual(calls, tt.calls) {
				t.Fatalf("conclusion = %q, error = %v, calls = %v; want %q, %v", got, err, calls, tt.want, tt.calls)
			}
		})
	}
}
