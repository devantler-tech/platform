package main

import (
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOwnedPauseContinuationAdmission(t *testing.T) {
	for _, failure := range []string{"", "initial mode", "unpaused", "unowned", "foreign owner", "shared owner", "prior observation", "empty observation", "orphaned ownership", "wrong confirmation", "missing nonce"} {
		t.Run(failure, func(t *testing.T) {
			s, g := completedFixture()
			o := testOptions()
			o.continuePause, o.pauseObservation = true, "123.1"
			at(s.cluster, "metadata", "annotations")[pauseKey] = "disabled"
			fields := object{"f:" + pauseKey: object{}}
			at(s.cluster, "metadata")["managedFields"] = []any{object{"manager": fieldManager, "fieldsV1": object{"f:metadata": object{"f:annotations": fields}}}}
			switch failure {
			case "initial mode":
				o.continuePause = false
			case "unpaused":
				delete(at(s.cluster, "metadata", "annotations"), pauseKey)
			case "unowned":
				delete(at(s.cluster, "metadata"), "managedFields")
			case "foreign owner":
				list(s.cluster, "metadata", "managedFields")[0]["manager"] = "other"
			case "shared owner":
				at(s.cluster, "metadata")["managedFields"] = append(value(s.cluster, "metadata", "managedFields").([]any), object{"manager": "other", "fieldsV1": object{"f:metadata": object{"f:annotations": fields}}})
			case "prior observation":
				at(s.cluster, "metadata", "annotations")[pauseObservationKey] = "122.1"
			case "empty observation":
				at(s.cluster, "metadata", "annotations")[pauseObservationKey] = ""
			case "orphaned ownership":
				fields["f:"+pauseObservationKey] = object{}
			case "wrong confirmation":
				o.pauseObservation = "123.2"
			case "missing nonce":
				o.pauseObservation = ""
			}
			// Read-only plans need no execution nonce; execution is tested below.
			_, err := completedJoinPlan(s, o, g)
			want := failure == "" || failure == "missing nonce" || failure == "wrong confirmation"
			if (err == nil) != want {
				t.Fatalf("admission %q accepted=%v: %v", failure, err == nil, err)
			}
		})
	}
}

func TestOwnedPauseCLIRejectsMixedOrUnconfirmedModes(t *testing.T) {
	for _, failure := range []string{"old confirmation", "missing nonce", "initial mode", "diagnostic", "no quarantine", "missing UID"} {
		t.Run(failure, func(t *testing.T) {
			env := map[string]string{"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/recover-retained-wedding-standby.yaml@refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "123", "GITHUB_SHA": strings.Repeat("a", 40), "WEDDING_REPAIR_CONFIRM": "continue-owned-pause-retain-volume"}
			for k, v := range env {
				t.Setenv(k, v)
			}
			args := []string{"repair", "--execute", "--quarantine-completed-join", "--continue-owned-pause", "--cluster-uid", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "--pod-uid", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "--job-uid", "cccccccc-cccc-cccc-cccc-cccccccccccc", "--claim-uid", "dddddddd-dddd-dddd-dddd-dddddddddddd", "--volume-uid", "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"}
			switch failure {
			case "old confirmation":
				t.Setenv("WEDDING_REPAIR_CONFIRM", "retain-volume-rebuild-after-rejection")
			case "missing nonce":
				t.Setenv("GITHUB_RUN_ID", "")
			case "initial mode":
				args = append(args[:3], args[4:]...)
			case "diagnostic":
				args = append(args, "--diagnose-pause")
			case "no quarantine":
				args = append(args[:2], args[3:]...)
			case "missing UID":
				args = args[:len(args)-2]
			}
			previousFlags, previousArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine = previousFlags; os.Args = previousArgs })
			flag.CommandLine = flag.NewFlagSet("repair", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = args
			err := run()
			if err == nil || strings.Contains(err.Error(), "flag provided but not defined") || (!strings.Contains(err.Error(), "separately confirmed") && !strings.Contains(err.Error(), "cannot share") && !strings.Contains(err.Error(), "requires completed-join") && !strings.Contains(err.Error(), "UIDs are required")) {
				t.Fatalf("unconfirmed continuation reached beyond CLI admission: %v", err)
			}
		})
	}
}

// Run the actual workflow admission shell with synthetic identity inputs. This
// exercises its protected first-main boundary without restoring credentials.
func TestOwnedPauseWorkflowAdmission(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/recover-retained-wedding-standby.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w object
	if err = yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	steps := list(w, "jobs", "repair", "steps")
	for _, mode := range []string{"retain-volume-rebuild-after-rejection", "continue-owned-pause-retain-volume", "consumed"} {
		for _, ref := range []string{"refs/heads/main", "refs/heads/other"} {
			for _, attempt := range []string{"1", "2"} {
				cmd := exec.Command("bash", "-c", str(steps[0], "run"))
				cmd.Env = []string{"PATH=/usr/bin:/bin", "CONFIRM=" + mode, "DISPATCH_REF=" + ref, "GITHUB_RUN_ATTEMPT=" + attempt}
				err := cmd.Run()
				want := mode != "consumed" && ref == "refs/heads/main" && attempt == "1"
				if (err == nil) != want {
					t.Fatalf("workflow mode=%s ref=%s attempt=%s admitted=%v", mode, ref, attempt, err == nil)
				}
			}
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"retain-volume-rebuild-after-rejection", "continue-owned-pause-retain-volume"} {
		cmd := exec.Command("/bin/bash", "-c", str(steps[len(steps)-1], "run"))
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "WEDDING_REPAIR_CONFIRM=" + mode, "CLUSTER_UID=cluster", "POD_UID=pod", "JOB_UID=job", "CLAIM_UID=claim", "VOLUME_UID=volume"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		continued := strings.Contains(string(out), "\n--continue-owned-pause\n")
		if continued != (mode == "continue-owned-pause-retain-volume") || !strings.Contains(string(out), "\n--execute\n--quarantine-completed-join\n") {
			t.Fatalf("confirmation selected the wrong execution mode: %q", out)
		}
	}
}

func TestOwnedPauseDispatchCannotReuseInitialApproval(t *testing.T) {
	for _, failure := range []string{"", "old confirmation", "attempt", "run ID", "zero run ID", "oversized run ID", "workflow", "branch", "event", "repo", "SHA"} {
		t.Run(failure, func(t *testing.T) {
			env := map[string]string{"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/recover-retained-wedding-standby.yaml@refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "123", "GITHUB_SHA": strings.Repeat("a", 40), "WEDDING_REPAIR_CONFIRM": "continue-owned-pause-retain-volume"}
			switch failure {
			case "old confirmation":
				env["WEDDING_REPAIR_CONFIRM"] = "retain-volume-rebuild-after-rejection"
			case "attempt":
				env["GITHUB_RUN_ATTEMPT"] = "2"
			case "run ID":
				env["GITHUB_RUN_ID"] = ""
			case "zero run ID":
				env["GITHUB_RUN_ID"] = "0"
			case "oversized run ID":
				env["GITHUB_RUN_ID"] = strings.Repeat("1", 21)
			case "workflow":
				env["GITHUB_WORKFLOW_REF"] = "another workflow"
			case "branch":
				env["GITHUB_REF"] = "refs/heads/other"
			case "event":
				env["GITHUB_EVENT_NAME"] = "pull_request"
			case "repo":
				env["GITHUB_REPOSITORY"] = "other/repo"
			case "SHA":
				env["GITHUB_SHA"] = ""
			}
			get := func(k string) string { return env[k] }
			if ownedPauseDispatchAllowed(get) != (failure == "") {
				t.Fatalf("dispatch boundary %q", failure)
			}
			if failure == "" && storageDispatchAllowed(get) {
				t.Fatal("continuation grant admitted an initial unpaused repair")
			}
		})
	}
}
