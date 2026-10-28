package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

const ingressDaemonSample = `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"cilium","namespace":"kube-system","uid":"22222222-2222-2222-2222-222222222222"}}`

// Unexpected containers, observer failures and identity changes must prevent a
// usable result, even after the observer has returned plausible flow bytes.
func TestIngressObserverBindsExecutionAndRefusesPartialEvidence(t *testing.T) {
	for _, mode := range []string{"matching", "unready-agent", "agent-churn", "daemon-churn", "probe-failure", "observer-failure", "truncated-flow"} {
		t.Run(mode, func(t *testing.T) {
			podReads, daemonReads, executions, probes, clockReads := 0, 0, 0, 0, 0
			start := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
			run := func(_ context.Context, args ...string) ([]byte, error) {
				switch strings.Join(args, " ") {
				case "--context admin@prod --request-timeout=10s get daemonset cilium -n kube-system -o json":
					daemonReads++
					raw := ingressDaemonSample
					if mode == "daemon-churn" && daemonReads > 1 {
						raw = strings.ReplaceAll(raw, "22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333")
					}
					return []byte(raw), nil
				case "--context admin@prod --request-timeout=10s get pods -n kube-system -l k8s-app=cilium -o json":
					podReads++
					raw := ingressAgentSample
					if mode == "unready-agent" {
						raw = strings.Replace(raw, `"ready":true`, `"ready":false`, 1)
					}
					if mode == "agent-churn" && podReads > 1 {
						raw = strings.Replace(raw, `"restartCount":0`, `"restartCount":1`, 1)
					}
					return []byte(raw), nil
				}
				prefix := []string{"--context", "admin@prod", "--request-timeout=10s", "exec", "-n", "kube-system", "cilium-abcde", "-c", "cilium-agent", "--", "hubble", "observe", "--server", "unix:///var/run/cilium/hubble.sock", "--timeout", "5s"}
				if len(args) != 24 || !reflect.DeepEqual(args[:16], prefix) || args[16] != "--since" || args[17] != "2026-10-28T16:00:00Z" || args[18] != "--until" || args[19] != "2026-10-28T16:00:10Z" || args[20] != "--allowlist" || args[22] != "--output" || args[23] != "jsonpb" {
					return nil, errors.New("unexpected native operation")
				}
				var actual, expected any
				if json.Unmarshal([]byte(args[21]), &actual) != nil || json.Unmarshal([]byte(`{"destination_ip":["10.244.2.2"],"destination_pod":["private-mail/private-mail-0"],"destination_port":["8080"],"protocol":["tcp"],"source_identity":[8]}`), &expected) != nil || !reflect.DeepEqual(actual, expected) {
					return nil, errors.New("unexpected filter")
				}
				executions++
				if mode == "observer-failure" {
					return nil, errors.New("private-observer-error")
				}
				raw := ingressFlowSample
				if mode == "truncated-flow" {
					raw += "\n{"
				}
				return []byte(raw), nil
			}
			probe := func(context.Context) (int, error) {
				probes++
				if mode == "probe-failure" {
					return 0, errors.New("private-probe-error")
				}
				return 503, nil
			}
			now := func() time.Time {
				clockReads++
				if clockReads == 1 {
					return start
				}
				return start.Add(10 * time.Second)
			}
			result, err := observeIngressSources(context.Background(), run, binding{UID: "44444444-4444-4444-4444-444444444444", Node: "worker-1"}, accessAddresses{Pod: netip.MustParseAddr("10.244.2.2")}, probe, func(context.Context) error { return nil }, now)
			if (err == nil) != (mode == "matching") {
				t.Fatal("partial observation accepted or bound observation refused")
			}
			if mode == "matching" && (len(result.Sources) != 1 || result.Sources[0] != netip.MustParseAddr("10.244.1.1") || result.Public503 != 2 || result.Public200 != 0) {
				t.Fatal("observed peer or public response was lost")
			}
			if err != nil && (len(result.Sources) != 0 || strings.Contains(err.Error(), "private-")) {
				t.Fatal("failed observation released private or partial evidence")
			}
			if mode == "unready-agent" && (executions != 0 || probes != 0) {
				t.Fatal("unready privileged target caused network activity")
			}
		})
	}
}
