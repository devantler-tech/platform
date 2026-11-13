package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIngressWorkflowRetainsReviewedIsolationAndMainOnlyGuard(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/read-email-ingress-source.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow observationWorkflow
	if yaml.Unmarshal(raw, &workflow) != nil {
		t.Fatal("invalid ingress observation workflow")
	}
	baseline := readObservationWorkflow(t)
	job, exists := workflow.Jobs["observe"]
	if !exists || len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil || len(workflow.Jobs) != 1 || len(workflow.Permissions) != 0 || workflow.Concurrency != baseline.Concurrency || job.Environment != "prod" || job.Timeout != 10 || !reflect.DeepEqual(job.Permissions, baseline.Jobs["observe"].Permissions) || len(job.Steps) != 6 || len(job.Env) != 1 || job.Env["MAIL_INGRESS_CONFIRM"] != "${{ inputs.confirm }}" {
		t.Fatal("ingress observation lost its protected fixed manual boundary")
	}
	for i := 1; i < 5; i++ {
		if !reflect.DeepEqual(job.Steps[i], baseline.Jobs["observe"].Steps[i]) {
			t.Fatal("build, immutable checkout or tool pin changed")
		}
	}
	wantRun := strings.ReplaceAll(baseline.Jobs["observe"].Steps[5].Run, "MAIL_STARTUP_SAMPLE", "MAIL_INGRESS_STATE")
	wantRun = strings.Replace(wantRun, "MAIL_INGRESS_STATE=INCONCLUSIVE", "MAIL_INGRESS_STATE=INCONCLUSIVE stage=configuration", 1)
	wantRun = strings.Replace(wantRun, "'MAIL_INGRESS_STATE=INCONCLUSIVE'", "'MAIL_INGRESS_STATE=INCONCLUSIVE stage=endpoint'", 1)
	if job.Steps[5].Run != wantRun || !reflect.DeepEqual(job.Steps[5].Env, baseline.Jobs["observe"].Steps[5].Env) {
		t.Fatal("credential handling differs from the exercised private cleanup contract")
	}
	for _, key := range []string{"GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_RUN_ATTEMPT", "MAIL_INGRESS_CONFIRM", "valid"} {
		env := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "MAIL_INGRESS_CONFIRM": "read-email-ingress-source"}
		if key != "valid" {
			env[key] = "unapproved"
		}
		command := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", job.Steps[0].Run)
		command.Env = []string{"PATH=/usr/bin:/bin"}
		for name, value := range env {
			command.Env = append(command.Env, name+"="+value)
		}
		out, err := command.CombinedOutput()
		if len(out) != 0 || (err == nil) != (key == "valid") {
			t.Fatal("workflow admitted an unapproved invocation or refused its exact confirmation")
		}
		reads := 0
		run := func(context.Context, ...string) ([]byte, error) {
			reads++
			return []byte("PRIVATE_ERROR"), errors.New("PRIVATE_ERROR")
		}
		var result bytes.Buffer
		code := runIngressEntry(context.Background(), func(name string) string { return env[name] }, run, func(context.Context, []byte, ...string) ([]byte, error) {
			t.Fatal("unexpected credential read")
			return nil, nil
		}, func(context.Context) error { t.Fatal("unexpected wait"); return nil }, &result)
		stage := "guard"
		if key == "valid" {
			stage = "pod_before"
		}
		if code != 3 || result.String() != "MAIL_INGRESS_STATE=INCONCLUSIVE stage="+stage+"\n" || (reads == 1) != (key == "valid") {
			t.Fatal("entrypoint lost its safe failure stage or admitted an unconfirmed read")
		}
	}
}
