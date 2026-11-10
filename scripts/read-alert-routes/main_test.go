package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func dispatch() map[string]string {
	return map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_ACTOR": "devantler", "GITHUB_SHA": strings.Repeat("a", 40), "RUNNER_ENVIRONMENT": "github-hosted", "ALERT_ROUTES_CONFIRM": "read-alert-routes"}
}

func responses() map[string]string {
	return map[string]string{
		"/apis/coroot.com/v1/namespaces/observability/coroots/coroot":                    `{"apiVersion":"coroot.com/v1","kind":"Coroot","metadata":{"uid":"coroot-uid","generation":2},"spec":{"projects":[{"name":"PRIVATE-CANARY","notificationIntegrations":{"webhook":{"url":"https://hooks.slack.com/services/PRIVATE-CANARY","incidents":true,"alerts":false}}}]}}`,
		"/apis/notification.toolkit.fluxcd.io/v1beta3/providers":                         `{"kind":"ProviderList","metadata":{},"items":[{"metadata":{"uid":"provider-uid","generation":1,"name":"slack","namespace":"flux-system"},"spec":{"type":"slack","secretRef":{"name":"PRIVATE-CANARY"}}}]}`,
		"/apis/notification.toolkit.fluxcd.io/v1beta3/alerts":                            `{"kind":"AlertList","metadata":{},"items":[{"metadata":{"uid":"alert-uid","generation":1,"name":"reconciliation","namespace":"flux-system"},"spec":{"providerRef":{"name":"slack"},"eventSeverity":"error","eventSources":[{"kind":"Kustomization","name":"*","namespace":"flux-system"}]}}]}`,
		"/apis/batch/v1/cronjobs":                                                        `{"kind":"CronJobList","metadata":{},"items":[{"metadata":{"uid":"cron-uid","generation":1,"name":"private-detector","namespace":"observability"},"spec":{"schedule":"*/30 * * * *","jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"detector","env":[{"name":"WEBHOOK_URL","valueFrom":{"secretKeyRef":{"name":"PRIVATE-CANARY","key":"url"}}}]}]}}}}}}]}`,
		"/api/v1/namespaces/kubescape/pods/alertmanager-0":                               pod("peer-zero"),
		"/api/v1/namespaces/kubescape/pods/alertmanager-1":                               pod("peer-one"),
		"/api/v1/namespaces/kubescape/pods/http:alertmanager-0:9093/proxy/api/v2/status": status("slack"),
		"/api/v1/namespaces/kubescape/pods/http:alertmanager-1:9093/proxy/api/v2/status": status("slack"),
	}
}

func pod(uid string) string {
	return fmt.Sprintf(`{"kind":"Pod","metadata":{"uid":%q},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"alertmanager","containerID":"container://%s","ready":true,"restartCount":0}]}}`, uid, uid)
}
func status(receiver string) string {
	return fmt.Sprintf(`{"config":{"original":"route:\n  receiver: %s\n  group_by: [alertname, host]\n  group_wait: 30s\n  group_interval: 5m\n  repeat_interval: 4h\nreceivers:\n  - name: %s\n    slack_configs:\n      - api_url_file: /secret/PRIVATE-CANARY\n        send_resolved: true\n"}}`, receiver, receiver)
}

func execute(t *testing.T, env map[string]string, payloads map[string]string, change func(int, string, string) (string, error)) (int, string, int) {
	t.Helper()
	calls := 0
	reader := func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if len(args) != 5 || args[0] != "--context=admin@prod" || args[1] != "--request-timeout=20s" || args[2] != "get" || args[3] != "--raw" {
			return nil, errors.New("not a fixed GET")
		}
		body, ok := payloads[args[4]]
		if !ok {
			return nil, errors.New("unreviewed target")
		}
		var err error
		if change != nil {
			body, err = change(calls, args[4], body)
		}
		return []byte(body), err
	}
	var out bytes.Buffer
	code := observe(context.Background(), func(k string) string { return env[k] }, reader, &out)
	if strings.Contains(out.String(), "PRIVATE-CANARY") || strings.Contains(out.String(), "hooks.slack.com") {
		t.Fatalf("private evidence escaped: %q", out.String())
	}
	return code, out.String(), calls
}

func TestObservedConfigurationIsClosedAndPeersAgree(t *testing.T) {
	code, out, calls := execute(t, dispatch(), responses(), nil)
	const want = "ALERT_ROUTE_INVENTORY=OBSERVED scope=cluster-configuration coroot_projects=1 coroot_slack=1 flux_providers=1 flux_slack=1 flux_alerts=1 active_cronjobs=1 webhook_candidates=1 alertmanager_peers=2 alertmanager_slack=1\n"
	if code != 0 || out != want || calls != 16 {
		t.Fatalf("code=%d calls=%d output=%q", code, calls, out)
	}
}

func TestInvalidDispatchReadsNothing(t *testing.T) {
	for key, bad := range map[string]string{"GITHUB_ACTIONS": "false", "GITHUB_REPOSITORY": "other/repo", "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/proposal", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_ACTOR": "other", "GITHUB_SHA": "short", "RUNNER_ENVIRONMENT": "self-hosted", "ALERT_ROUTES_CONFIRM": "yes"} {
		t.Run(key, func(t *testing.T) {
			env := dispatch()
			env[key] = bad
			code, out, calls := execute(t, env, responses(), nil)
			if code != 3 || out != "ALERT_ROUTE_INVENTORY=UNKNOWN\n" || calls != 0 {
				t.Fatalf("%d %d %q", code, calls, out)
			}
		})
	}
}

func TestIncompleteOrChangedEvidenceCannotReportObserved(t *testing.T) {
	cases := map[string]func(int, string, string) (string, error){
		"read-error": func(_ int, _ string, b string) (string, error) { return b, errors.New("PRIVATE-CANARY") },
		"truncated-list": func(_ int, p, b string) (string, error) {
			if strings.HasSuffix(p, "/cronjobs") {
				b = strings.Replace(b, `"metadata":{}`, `"metadata":{"continue":"PRIVATE-CANARY"}`, 1)
			}
			return b, nil
		},
		"missing-list-items": func(_ int, p, b string) (string, error) {
			if strings.HasSuffix(p, "/providers") {
				b = `{"kind":"ProviderList","metadata":{}}`
			}
			return b, nil
		},
		"empty-required-list": func(_ int, p, b string) (string, error) {
			if strings.HasSuffix(p, "/alerts") {
				b = `{"kind":"AlertList","metadata":{},"items":[]}`
			}
			return b, nil
		},
		"duplicate-identity": func(_ int, p, b string) (string, error) {
			if strings.HasSuffix(p, "/providers") {
				b = strings.Replace(b, `}]}`, `},{"metadata":{"uid":"provider-uid","generation":1},"spec":{"type":"slack"}}]}`, 1)
			}
			return b, nil
		},
		"missing-coroot-projects": func(_ int, p, b string) (string, error) {
			if strings.Contains(p, "/coroots/") {
				b = `{"kind":"Coroot","metadata":{"uid":"coroot-uid","generation":2},"spec":{}}`
			}
			return b, nil
		},
		"bad-webhook": func(_ int, p, b string) (string, error) {
			if strings.Contains(p, "/coroots/") {
				b = strings.ReplaceAll(b, "https://hooks.slack.com/services/PRIVATE-CANARY", "not-a-url")
			}
			return b, nil
		},
		"different-peers": func(_ int, p, b string) (string, error) {
			if strings.Contains(p, "http:alertmanager-1") {
				b = status("changed")
			}
			return b, nil
		},
		"unready-peer": func(_ int, p, b string) (string, error) {
			if strings.HasSuffix(p, "/pods/alertmanager-0") {
				b = strings.Replace(b, `"status":"True"`, `"status":"False"`, 1)
			}
			return b, nil
		},
		"restart-during-read": func(n int, p, b string) (string, error) {
			if n > 8 && strings.HasSuffix(p, "/pods/alertmanager-0") {
				b = pod("replacement")
			}
			return b, nil
		},
		"same-peer-twice": func(_ int, p, b string) (string, error) {
			if strings.HasSuffix(p, "/pods/alertmanager-1") {
				b = pod("peer-zero")
			}
			return b, nil
		},
		"missing-container-identity": func(_ int, p, b string) (string, error) {
			if strings.HasSuffix(p, "/pods/alertmanager-0") {
				b = strings.Replace(b, "container://peer-zero", "", 1)
			}
			return b, nil
		},
		"runtime-receiver-missing": func(_ int, p, b string) (string, error) {
			if strings.Contains(p, "/proxy/") {
				b = strings.Replace(b, "receiver: slack", "receiver: absent", 1)
			}
			return b, nil
		},
		"runtime-extra-document": func(_ int, p, b string) (string, error) {
			if strings.Contains(p, "/proxy/") {
				b = strings.Replace(b, `send_resolved: true\n`, `send_resolved: true\n---\nPRIVATE-CANARY: true\n`, 1)
			}
			return b, nil
		},
		"configuration-change": func(n int, p, b string) (string, error) {
			if n > 8 && strings.Contains(p, "/coroots/") {
				b = strings.Replace(b, `"generation":2`, `"generation":3`, 1)
			}
			return b, nil
		},
		"malformed-json": func(_ int, _ string, _ string) (string, error) { return "{", nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, _ := execute(t, dispatch(), responses(), change)
			if code != 3 || out != "ALERT_ROUTE_INVENTORY=UNKNOWN\n" {
				t.Fatalf("%d %q", code, out)
			}
		})
	}
}

func TestWorkflowFirstStepRejectsUnreviewedInvocation(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "../../.github/workflows/read-alert-routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Run string } }
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	steps := workflow.Jobs["observe"].Steps
	if len(steps) == 0 || steps[0].Run == "" {
		t.Fatal("missing pre-checkout guard")
	}
	run := func(env map[string]string) error {
		command := exec.Command("bash", "-c", steps[0].Run)
		for key, value := range env {
			command.Env = append(command.Env, key+"="+value)
		}
		return command.Run()
	}
	if err := run(dispatch()); err != nil {
		t.Fatalf("reviewed dispatch refused: %v", err)
	}
	for key, bad := range map[string]string{"GITHUB_REPOSITORY": "other/repo", "GITHUB_ACTOR": "other", "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/unreviewed", "GITHUB_RUN_ATTEMPT": "2", "RUNNER_ENVIRONMENT": "self-hosted", "ALERT_ROUTES_CONFIRM": "yes"} {
		env := dispatch()
		env[key] = bad
		if run(env) == nil {
			t.Fatalf("first step admitted %s", key)
		}
	}
}

func TestNativeReaderRejectsOversizedEvidence(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\n/usr/bin/head -c 8388609 /dev/zero\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := nativeRead(context.Background(), "get", "--raw", "/fixed"); err == nil {
		t.Fatal("oversized evidence admitted")
	}
}

func TestNativeReaderDoesNotReturnPrivateFailureOutput(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\nprintf PRIVATE-CANARY\nprintf PRIVATE-CANARY >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	body, err := nativeRead(context.Background(), "get", "--raw", "/fixed")
	if err == nil || len(body) != 0 || strings.Contains(err.Error(), "PRIVATE-CANARY") {
		t.Fatalf("private failure leaked: %q %v", body, err)
	}
}

type refusingOutput struct{}

func (refusingOutput) Write([]byte) (int, error) { return 0, errors.New("unavailable output") }
func TestUnavailableReceiptCannotReturnSuccess(t *testing.T) {
	payloads := responses()
	read := func(_ context.Context, args ...string) ([]byte, error) { return []byte(payloads[args[4]]), nil }
	env := dispatch()
	if code := observe(context.Background(), func(k string) string { return env[k] }, read, refusingOutput{}); code != 3 {
		t.Fatalf("unacknowledged output returned %d", code)
	}
}
