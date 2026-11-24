package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func dispatch() map[string]string {
	return map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_ACTOR": "devantler", "GITHUB_SHA": strings.Repeat("a", 40), "RUNNER_ENVIRONMENT": "github-hosted", "PROD_OPERATION": "start-email-history-import"}
}

type cluster struct {
	cron, secret, jobs string
	createErr          error
	created            []string
}

func healthy() *cluster {
	return &cluster{
		cron:   `{"spec":{"suspend":true}}`,
		secret: `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`,
		jobs:   `{"items":[{"metadata":{"name":"email-history-import-old"},"status":{"active":0,"conditions":[{"type":"Failed","status":"True"}]}}]}`,
	}
}

func (c *cluster) kubectl(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "--context=admin@prod --request-timeout=20s -n private-mail ") {
		return nil, errors.New("unexpected scope")
	}
	switch {
	case strings.Contains(joined, " get cronjob email-history-import -o json"):
		return []byte(c.cron), nil
	case strings.Contains(joined, " get externalsecret email-history-import -o json"):
		return []byte(c.secret), nil
	case strings.Contains(joined, " get jobs -l app=email-history-import -o json"):
		return []byte(c.jobs), nil
	case strings.Contains(joined, " create job --from=cronjob/email-history-import "):
		if c.createErr != nil {
			return nil, c.createErr
		}
		c.created = append(c.created, args[len(args)-1])
		return []byte("job.batch created"), nil
	}
	return nil, errors.New("unexpected command: " + joined)
}

func run(env map[string]string, c *cluster) (int, string) {
	var out bytes.Buffer
	now := time.Date(2026, 11, 24, 21, 5, 9, 0, time.UTC)
	code := start(context.Background(), func(k string) string { return env[k] }, now, c.kubectl, &out)
	return code, out.String()
}

func TestStartsOneCopyFromTheSuspendedTemplate(t *testing.T) {
	c := healthy()
	code, out := run(dispatch(), c)
	if code != 0 || out != "EMAIL_HISTORY_IMPORT_START=STARTED job=email-history-import-20261124-210509\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if len(c.created) != 1 || c.created[0] != "email-history-import-20261124-210509" {
		t.Fatalf("created %q", c.created)
	}
}

func TestRefusesWithoutCreating(t *testing.T) {
	cases := map[string]struct {
		env    func(map[string]string)
		change func(*cluster)
		reason string
	}{
		"other operation":      {env: func(e map[string]string) { e["PROD_OPERATION"] = "read-alert-routes" }, reason: "not-the-reviewed-dispatch"},
		"re-run":               {env: func(e map[string]string) { e["GITHUB_RUN_ATTEMPT"] = "2" }, reason: "not-the-reviewed-dispatch"},
		"other actor":          {env: func(e map[string]string) { e["GITHUB_ACTOR"] = "someone" }, reason: "not-the-reviewed-dispatch"},
		"branch":               {env: func(e map[string]string) { e["GITHUB_REF"] = "refs/heads/feature" }, reason: "not-the-reviewed-dispatch"},
		"template missing":     {change: func(c *cluster) { c.cron = "" }, reason: "template-unreadable"},
		"template unsuspended": {change: func(c *cluster) { c.cron = `{"spec":{"suspend":false}}` }, reason: "template-not-suspended"},
		"suspend unset":        {change: func(c *cluster) { c.cron = `{"spec":{}}` }, reason: "template-not-suspended"},
		"inputs missing":       {change: func(c *cluster) { c.secret = "not json" }, reason: "inputs-unreadable"},
		"inputs not synced": {change: func(c *cluster) {
			c.secret = `{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`
		}, reason: "inputs-not-synced"},
		"inputs no status": {change: func(c *cluster) { c.secret = `{"status":{}}` }, reason: "inputs-not-synced"},
		"jobs unreadable":  {change: func(c *cluster) { c.jobs = "" }, reason: "earlier-copies-unreadable"},
		"copy running": {change: func(c *cluster) {
			c.jobs = `{"items":[{"metadata":{"name":"x"},"status":{"active":1}}]}`
		}, reason: "copy-already-running"},
		"copy pending": {change: func(c *cluster) {
			c.jobs = `{"items":[{"metadata":{"name":"x"},"status":{"active":0}}]}`
		}, reason: "copy-already-running"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env, c := dispatch(), healthy()
			if tc.env != nil {
				tc.env(env)
			}
			if tc.change != nil {
				tc.change(c)
			}
			code, out := run(env, c)
			if code != 2 || out != "EMAIL_HISTORY_IMPORT_START=REFUSED reason="+tc.reason+"\n" {
				t.Fatalf("code=%d out=%q", code, out)
			}
			if len(c.created) != 0 {
				t.Fatal("a refused start created a Job")
			}
		})
	}
}

func TestCreateFailureIsUnknown(t *testing.T) {
	c := healthy()
	c.createErr = errors.New("denied")
	code, out := run(dispatch(), c)
	if code != 3 || out != "EMAIL_HISTORY_IMPORT_START=UNKNOWN\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestAcceptsNoEarlierCopies(t *testing.T) {
	c := healthy()
	c.jobs = `{"items":[]}`
	if code, _ := run(dispatch(), c); code != 0 {
		t.Fatalf("code=%d", code)
	}
}
