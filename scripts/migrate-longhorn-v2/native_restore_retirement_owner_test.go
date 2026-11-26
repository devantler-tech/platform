package main

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestNativeRestoreOutcomeCanBeObservedWithoutReclaimAuthority(t *testing.T) {
	_, _, state := nativeRestoreStateFixture(t, "completed")
	reads := 0
	c := client{context: "oidc@prod", run: func(_ context.Context, input []byte, binary string, args ...string) ([]byte, error) {
		if input != nil || binary != "gh" || !slices.Equal(args, []string{"api", "repos/devantler-tech/platform/actions/runs/" + state.Run}) {
			t.Fatal("read-only ownership check attempted an unrelated command")
		}
		reads++
		return json.Marshal(object{"id": 5678, "head_sha": state.Source, "head_branch": "main", "event": "workflow_dispatch",
			"status": "completed", "conclusion": "success", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"})
	}}
	if err := verifyNativeRestoreSuccess(t.Context(), c, state, "", ""); err != nil || reads != 1 {
		t.Fatal("the original success could not be read without reclaim authority", err)
	}
	if err := nativeRestoreReclaimOwnerSucceeded(t.Context(), c, state, ""); err == nil || reads != 1 {
		t.Fatal("read access conferred reclaim authority")
	}
}

func TestReclaimBindsAllThreeRestoreAttempts(t *testing.T) {
	for _, fault := range []string{"", "missing continuation", "prior succeeded", "prior rerun", "unrelated prior source", "aliased executor"} {
		t.Run(fault, func(t *testing.T) {
			_, _, state := testPendingReattachState(t)
			state.Phase = "completed"
			const previousRun, resumedRun = "6789", "9876"
			previous, resumed := strings.Repeat("a", 40), strings.Repeat("b", 40)
			executor := resumed + ":" + resumedRun
			state.Reattachment = &nativeReattachmentRecovery{PreviousExecutor: previous + ":" + previousRun}
			if fault == "missing continuation" {
				executor = ""
			}
			if fault == "aliased executor" {
				state.Reattachment.PreviousExecutor = executor
			}
			var reads []string
			c := client{context: "oidc@prod", run: func(_ context.Context, input []byte, binary string, args ...string) ([]byte, error) {
				if input != nil || binary != "gh" || len(args) != 2 || args[0] != "api" {
					t.Fatal("the receipt check left the Actions reader")
				}
				run, ok := strings.CutPrefix(args[1], "repos/devantler-tech/platform/actions/runs/")
				if !ok {
					t.Fatal("the receipt check called an unrelated API")
				}
				reads = append(reads, run)
				id, err := strconv.Atoi(run)
				if err != nil {
					t.Fatal(err)
				}
				result := object{"id": id, "head_branch": "main", "event": "workflow_dispatch", "status": "completed",
					"conclusion": "failure", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"}
				switch run {
				case state.Run:
					result["head_sha"] = state.Source
				case previousRun:
					result["head_sha"] = previous
					switch fault {
					case "prior succeeded":
						result["conclusion"] = "success"
					case "prior rerun":
						result["run_attempt"] = 2
					case "unrelated prior source":
						result["head_sha"] = strings.Repeat("c", 40)
					}
				case resumedRun:
					result["head_sha"], result["conclusion"] = resumed, "success"
				default:
					t.Fatal("unrelated Actions run", run)
				}
				return json.Marshal(result)
			}}
			err := verifyNativeRestoreSuccess(t.Context(), c, state, executor, "8888")
			if (err == nil) != (fault == "") {
				t.Fatalf("unexpected verdict for the three restore attempts: %v", err)
			}
			if err == nil && !slices.Equal(reads, []string{state.Run, previousRun, resumedRun}) {
				t.Fatalf("missed one of the recorded attempts: %v", reads)
			}
			if err != nil && slices.Contains(reads, resumedRun) {
				t.Fatalf("continued after a missing or invalid prior attempt: %v", reads)
			}
		})
	}
}

func TestNativeRestoreReclaimChecksTheOriginalSuccessfulFirstAttempt(t *testing.T) {
	for _, fault := range []string{"", "unexpected run", "changed source", "wrong path", "other branch", "other event", "live run", "failed run", "second attempt"} {
		t.Run(fault, func(t *testing.T) {
			_, _, state := nativeRestoreStateFixture(t, "completed")
			for key, value := range nativeRestoreDispatch(state) {
				t.Setenv(key, value)
			}
			t.Setenv("GITHUB_RUN_ID", "6789")
			t.Setenv("LONGHORN_V2_OPERATION", nativeRestoreReclaimOperation)
			t.Setenv("LONGHORN_V2_CONFIRM", nativeRestoreReclaimConfirmation+state.Run)
			run := object{"id": 5678, "head_sha": state.Source, "head_branch": "main", "event": "workflow_dispatch", "status": "completed",
				"conclusion": "success", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"}
			switch fault {
			case "unexpected run":
				run["id"] = 1234
			case "changed source":
				run["head_sha"] = "different"
			case "wrong path":
				run["path"] = ".github/workflows/ci.yaml"
			case "other branch":
				run["head_branch"] = "different"
			case "other event":
				run["event"] = "push"
			case "live run":
				run["status"] = "in_progress"
			case "failed run":
				run["conclusion"] = "failure"
			case "second attempt":
				run["run_attempt"] = 2
			}
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, binary string, args ...string) ([]byte, error) {
				if input != nil || binary != "gh" || !slices.Equal(args, []string{"api", "repos/devantler-tech/platform/actions/runs/" + state.Run}) {
					t.Fatalf("run verification invoked an unexpected command: %s %v", binary, args)
				}
				return json.Marshal(run)
			}}
			if err := nativeRestoreReclaimOwnerSucceeded(context.Background(), c, state, ""); (err != nil) != (fault != "") {
				t.Fatalf("original owner verdict for %q: %v", fault, err)
			}
		})
	}
}

func TestNativeRestoreReclaimRequiresTheRecordedContinuationToHaveSucceeded(t *testing.T) {
	_, _, state := nativeRestoreStateFixture(t, "completed")
	for _, fault := range []string{"", "original still running", "original succeeded", "continuation failed", "continuation rerun", "unrelated source", "same owner", "malformed receipt"} {
		t.Run(fault, func(t *testing.T) {
			for key, value := range nativeRestoreDispatch(state) {
				t.Setenv(key, value)
			}
			t.Setenv("GITHUB_RUN_ID", "6789")
			t.Setenv("LONGHORN_V2_OPERATION", nativeRestoreReclaimOperation)
			t.Setenv("LONGHORN_V2_CONFIRM", nativeRestoreReclaimConfirmation+state.Run)
			source, run := strings.Repeat("a", 40), "9876"
			executor := source + ":" + run
			switch fault {
			case "same owner":
				executor = source + ":" + state.Run
			case "malformed receipt":
				executor = source + ":" + run + ":unexpected"
			}
			var reads []string
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, binary string, args ...string) ([]byte, error) {
				if input != nil || binary != "gh" || len(args) != 2 || args[0] != "api" {
					t.Fatal("continuation verification escaped the Actions reader", binary, args)
				}
				requested, ok := strings.CutPrefix(args[1], "repos/devantler-tech/platform/actions/runs/")
				if !ok || requested != state.Run && requested != run {
					t.Fatal("unexpected Actions run", args[1])
				}
				reads = append(reads, requested)
				id, _ := strconv.Atoi(requested)
				result := object{"id": id, "head_sha": source, "head_branch": "main", "event": "workflow_dispatch", "status": "completed",
					"conclusion": "success", "run_attempt": 1, "path": ".github/workflows/prepare-longhorn-v2-node.yaml"}
				if requested == state.Run {
					result["head_sha"], result["conclusion"] = state.Source, "failure"
					if fault == "original still running" {
						result["status"] = "in_progress"
					}
					if fault == "original succeeded" {
						result["conclusion"] = "success"
					}
				} else {
					switch fault {
					case "continuation failed":
						result["conclusion"] = "failure"
					case "continuation rerun":
						result["run_attempt"] = 2
					case "unrelated source":
						result["head_sha"] = strings.Repeat("b", 40)
					}
				}
				return json.Marshal(result)
			}}
			if err := nativeRestoreReclaimOwnerSucceeded(t.Context(), c, state, executor); (err == nil) != (fault == "") {
				t.Fatalf("unexpected continuation verdict: %v", err)
			}
			if fault == "" && !slices.Equal(reads, []string{state.Run, run}) {
				t.Fatalf("the original failure and continuation success were not both bound: %v", reads)
			}
		})
	}
}
