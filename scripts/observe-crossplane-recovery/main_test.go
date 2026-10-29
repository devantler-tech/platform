package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func dispatchEnvironment() map[string]string {
	return map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "CROSSPLANE_RECOVERY_CONFIRM": "read-crossplane-alert-recovery"}
}
func fixedReader(t *testing.T, e recoveryEvidence) executor {
	t.Helper()
	responses := map[string][]byte{
		"--context=admin@prod --request-timeout=20s -n observability get configmap crossplane-sync-exporter -o json":                          e.Configuration,
		"--context=admin@prod --request-timeout=20s -n observability get deployment crossplane-sync-exporter -o json":                         e.Deployment,
		"--context=admin@prod --request-timeout=20s -n observability get cronjob crossplane-sync-alerter -o json":                             e.Cron,
		"--context=admin@prod --request-timeout=20s get --raw /api/v1/namespaces/kubescape/pods/http:alertmanager-0:9093/proxy/api/v2/alerts": e.Peers[0],
		"--context=admin@prod --request-timeout=20s get --raw /api/v1/namespaces/kubescape/pods/http:alertmanager-1:9093/proxy/api/v2/alerts": e.Peers[1],
	}
	return func(_ context.Context, args ...string) ([]byte, error) {
		raw, ok := responses[strings.Join(args, " ")]
		if !ok {
			t.Fatalf("observer attempted an unexpected command: %v", args)
		}
		return raw, nil
	}
}
func TestCommandReportsRecoveryOnlyAfterFixedReads(t *testing.T) {
	env := dispatchEnvironment()
	var out bytes.Buffer
	code := runObservation(t.Context(), func(k string) string { return env[k] }, fixedReader(t, goodEvidence()), map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime, &out)
	if code != 0 || !strings.Contains(out.String(), "CROSSPLANE_RECOVERY=RECOVERED") {
		t.Fatalf("complete fixed reads must prove recovery: code=%d output=%q", code, out.String())
	}
}
func TestCommandRefusesBeforeReadingCredentialsOrCluster(t *testing.T) {
	for _, key := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_RUN_ATTEMPT", "CROSSPLANE_RECOVERY_CONFIRM"} {
		t.Run(key, func(t *testing.T) {
			env := dispatchEnvironment()
			env[key] = "wrong"
			var out bytes.Buffer
			code := runObservation(t.Context(), func(k string) string { return env[k] }, func(context.Context, ...string) ([]byte, error) {
				t.Fatal("refused dispatch reached cluster")
				return nil, nil
			}, nil, "", observationTime, &out)
			if code != 3 || !strings.Contains(out.String(), "CROSSPLANE_RECOVERY=UNKNOWN") {
				t.Fatalf("guard failed: code=%d output=%q", code, out.String())
			}
		})
	}
}
func TestCommandDoesNotExposeNativeReadErrors(t *testing.T) {
	env := dispatchEnvironment()
	var out bytes.Buffer
	code := runObservation(t.Context(), func(k string) string { return env[k] }, func(context.Context, ...string) ([]byte, error) {
		return []byte("private-response"), errors.New("private-native-error")
	}, nil, "", observationTime, &out)
	if code != 3 || strings.Contains(out.String(), "private-") || !strings.Contains(out.String(), "CROSSPLANE_RECOVERY=UNKNOWN") {
		t.Fatalf("native read failure was not safely inconclusive: code=%d output=%q", code, out.String())
	}
}
func TestCommandDoesNotReportPartialPeerRecovery(t *testing.T) {
	env := dispatchEnvironment()
	var out bytes.Buffer
	read := fixedReader(t, goodEvidence())
	code := runObservation(t.Context(), func(k string) string { return env[k] }, func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "alertmanager-1") {
			return nil, errors.New("denied")
		}
		return read(ctx, args...)
	}, map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime, &out)
	if code != 3 || strings.Contains(out.String(), "RECOVERY=RECOVERED") {
		t.Fatalf("partial read cleared recovery: code=%d output=%q", code, out.String())
	}
}
