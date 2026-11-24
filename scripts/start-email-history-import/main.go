// Command start-email-history-import starts the reviewed one-time mailbox
// history copy (#5481) from its suspended CronJob and returns without waiting.
//
// It runs only inside the Production Operation workflow. It refuses unless the
// CronJob is still suspended, its inputs have synced, and no earlier copy is
// still running. It prints only a fixed verdict and the new Job's name; the
// copy's own aggregate receipt is read from the Job's log afterwards.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"time"
)

const (
	namespace = "private-mail"
	cronJob   = "email-history-import"
	inputs    = "email-history-import"
	operation = "start-email-history-import"
	maxOutput = 4 << 20
)

type executor func(context.Context, ...string) ([]byte, error)

func allowed(env func(string) string) bool {
	return env("GITHUB_ACTIONS") == "true" && env("GITHUB_REPOSITORY") == "devantler-tech/platform" &&
		env("GITHUB_EVENT_NAME") == "workflow_dispatch" && env("GITHUB_REF") == "refs/heads/main" &&
		env("GITHUB_RUN_ATTEMPT") == "1" && env("GITHUB_ACTOR") == "devantler" &&
		env("RUNNER_ENVIRONMENT") == "github-hosted" && env("PROD_OPERATION") == operation &&
		regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(env("GITHUB_SHA"))
}

type condition struct{ Type, Status string }

type cronJobObject struct {
	Spec struct {
		Suspend *bool `json:"suspend"`
	} `json:"spec"`
}

type externalSecretObject struct {
	Status struct {
		Conditions []condition `json:"conditions"`
	} `json:"status"`
}

type jobList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Active     int         `json:"active"`
			Conditions []condition `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

func finished(conditions []condition) bool {
	for _, c := range conditions {
		if (c.Type == "Complete" || c.Type == "Failed") && c.Status == "True" {
			return true
		}
	}
	return false
}

func start(ctx context.Context, env func(string) string, now time.Time, kubectl executor, out io.Writer) int {
	refuse := func(reason string) int {
		_, _ = fmt.Fprintf(out, "EMAIL_HISTORY_IMPORT_START=REFUSED reason=%s\n", reason)
		return 2
	}
	if !allowed(env) {
		return refuse("not-the-reviewed-dispatch")
	}
	get := func(target any, args ...string) bool {
		base := []string{"--context=admin@prod", "--request-timeout=20s", "-n", namespace, "get"}
		raw, err := kubectl(ctx, append(append(base, args...), "-o", "json")...)
		return err == nil && len(raw) > 0 && len(raw) <= maxOutput && json.Unmarshal(raw, target) == nil
	}

	var cron cronJobObject
	if !get(&cron, "cronjob", cronJob) {
		return refuse("template-unreadable")
	}
	if cron.Spec.Suspend == nil || !*cron.Spec.Suspend {
		return refuse("template-not-suspended")
	}
	var secret externalSecretObject
	if !get(&secret, "externalsecret", inputs) {
		return refuse("inputs-unreadable")
	}
	synced := false
	for _, c := range secret.Status.Conditions {
		synced = synced || c.Type == "Ready" && c.Status == "True"
	}
	if !synced {
		return refuse("inputs-not-synced")
	}
	var jobs jobList
	if !get(&jobs, "jobs", "-l", "app="+cronJob) {
		return refuse("earlier-copies-unreadable")
	}
	for _, job := range jobs.Items {
		if job.Status.Active > 0 || !finished(job.Status.Conditions) {
			return refuse("copy-already-running")
		}
	}

	name := fmt.Sprintf("%s-%s", cronJob, now.UTC().Format("20060102-150405"))
	if _, err := kubectl(ctx, "--context=admin@prod", "--request-timeout=20s", "-n", namespace,
		"create", "job", "--from=cronjob/"+cronJob, name); err != nil {
		_, _ = fmt.Fprintln(out, "EMAIL_HISTORY_IMPORT_START=UNKNOWN")
		return 3
	}
	_, _ = fmt.Fprintf(out, "EMAIL_HISTORY_IMPORT_START=STARTED job=%s\n", name)
	return 0
}

func runKubectl(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stderr = io.Discard
	return cmd.Output()
}

func main() {
	if len(os.Args) != 1 {
		_, _ = fmt.Fprintln(os.Stdout, "EMAIL_HISTORY_IMPORT_START=REFUSED reason=arguments")
		os.Exit(2)
	}
	os.Exit(start(context.Background(), os.Getenv, time.Now(), runKubectl, os.Stdout))
}
