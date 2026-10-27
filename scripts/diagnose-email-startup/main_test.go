package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// goodMeasurement supplies one complete synthetic healthy observation.
func goodMeasurement() measurement {
	return measurement{PID1Stalwart: true, UID2000: true, ConfigSHA256: strings.Repeat("a", 64),
		CertificateReadable: true, KeyReadable: true, DataReadable: true, DataWritable: true, TemporaryWritable: true,
		LockDevice: 2049, LockInode: 42, WriteLocks: []string{"08:01:42"}, ManagementListeners: 1, LocalHealth: 200}
}

func TestClassifyObservedPhases(t *testing.T) {
	cases := []struct {
		name, want     string
		change         func(*measurement)
		matched, ready bool
	}{
		{"healthy", "OBSERVED_HEALTHY_SAMPLE", func(*measurement) {}, true, true},
		{"mounted configuration", "MOUNTED_CONFIGURATION_DIFFERS", func(*measurement) {}, false, true},
		{"runtime identity", "UNEXPECTED_RUNTIME_IDENTITY", func(m *measurement) { m.UID2000 = false }, true, true},
		{"required input", "REQUIRED_RUNTIME_INPUT_UNAVAILABLE", func(m *measurement) { m.KeyReadable = false }, true, true},
		{"datastore lock", "DATASTORE_NOT_OBSERVED_OPEN", func(m *measurement) { m.WriteLocks = nil }, true, true},
		{"recovery process", "UNEXPECTED_RECOVERY_MODE", func(m *measurement) { m.RecoveryMode = true }, true, true},
		{"missing listener", "MANAGEMENT_LISTENER_ABSENT", func(m *measurement) { m.ManagementListeners = 0; m.LocalHealth = 0 }, true, false},
		{"local health", "LOCAL_HEALTH_NOT_READY", func(m *measurement) { m.LocalHealth = 503 }, true, false},
		{"routing or probe", "LOCAL_HEALTHY_KUBERNETES_UNREADY", func(*measurement) {}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := goodMeasurement()
			tc.change(&m)
			if got := classify(m, tc.matched, tc.ready); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestLinuxDeviceIdentityIncludesHighMinorBits(t *testing.T) {
	for _, tc := range []struct {
		device uint64
		want   string
	}{{2049, "08:01"}, {0x123abc45, "abc:12345"}} {
		if got := linuxDevice(tc.device); got != tc.want {
			t.Fatalf("%x got %s want %s", tc.device, got, tc.want)
		}
	}
}

// fixturePod describes only the fixed, synthetic approved runtime contract.
func fixturePod(t *testing.T) []byte {
	t.Helper()
	p := map[string]any{
		"metadata": map[string]any{"name": "private-mail-0", "namespace": "private-mail", "uid": "00000000-0000-0000-0000-000000000001", "resourceVersion": "42"},
		"spec":     map[string]any{"nodeName": "synthetic-node", "volumes": []any{map[string]any{"name": "datastore", "configMap": map[string]any{"name": "private-mail-datastore"}}}, "containers": []any{map[string]any{"name": "private-mail", "image": approvedImage, "command": []string{"/usr/local/bin/stalwart"}, "args": []string{"--config", "/etc/stalwart/config.json"}, "volumeMounts": []any{map[string]any{"name": "datastore", "mountPath": "/etc/stalwart/config.json", "subPath": "config.json", "readOnly": true}}}}},
		"status":   map[string]any{"containerStatuses": []any{map[string]any{"name": "private-mail", "containerID": "containerd://synthetic", "imageID": "sha256:synthetic", "restartCount": 0, "ready": true, "state": map[string]any{"running": map[string]any{"startedAt": "2026-10-27T00:00:00Z"}}}}},
	}
	b, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// changePod applies a test-only identity or lifecycle mutation.
func changePod(t *testing.T, b []byte, change func(map[string]any)) []byte {
	t.Helper()
	var p map[string]any
	if e := json.Unmarshal(b, &p); e != nil {
		t.Fatal(e)
	}
	change(p)
	out, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	return out
}

// fixtureMap contains only the public datastore startup configuration.
func fixtureMap(t *testing.T) []byte {
	t.Helper()
	b, e := json.Marshal(map[string]any{"metadata": map[string]any{"name": "private-mail-datastore", "namespace": "private-mail", "uid": "00000000-0000-0000-0000-000000000002", "resourceVersion": "7"}, "data": map[string]string{"config.json": publicConfig}})
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestObservationBindsCompleteReadsAndOneFixedExec(t *testing.T) {
	baseline := fixturePod(t)
	for _, scenario := range []string{"healthy", "pod-replaced", "container-replaced", "restarted", "map-changed", "exec-failed", "partial-json", "extra-field", "unexpected-image", "api-failed"} {
		t.Run(scenario, func(t *testing.T) {
			pod := append([]byte(nil), baseline...)
			after := append([]byte(nil), baseline...)
			cm := fixtureMap(t)
			cmAfter := append([]byte(nil), cm...)
			m := goodMeasurement()
			m.ConfigSHA256 = hashBytes([]byte(publicConfig))
			measured, _ := json.Marshal(m)
			switch scenario {
			case "pod-replaced":
				after = changePod(t, after, func(p map[string]any) { p["metadata"].(map[string]any)["uid"] = "00000000-0000-0000-0000-000000000003" })
			case "container-replaced":
				after = changePod(t, after, func(p map[string]any) {
					p["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["containerID"] = "containerd://replacement"
				})
			case "restarted":
				after = changePod(t, after, func(p map[string]any) {
					p["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["restartCount"] = 1
				})
			case "map-changed":
				cmAfter = []byte(strings.Replace(string(cm), `"7"`, `"8"`, 1))
			case "partial-json":
				measured = []byte("{")
			case "extra-field":
				measured = []byte(strings.TrimSuffix(string(measured), "}") + `,"secret":"PRIVATE_SENTINEL"}`)
			case "unexpected-image":
				pod = changePod(t, pod, func(p map[string]any) {
					p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "unreviewed"
				})
			}
			calls, execs := 0, 0
			fake := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if len(args) < 4 || strings.Join(args[:3], " ") != "--context admin@prod --request-timeout=10s" {
					t.Fatalf("unexpected identity or bound: %q", args)
				}
				verb := args[3]
				if scenario == "api-failed" {
					return nil, errors.New("PRIVATE_SENTINEL")
				}
				if verb == "exec" {
					execs++
					if !strings.Contains(strings.Join(args, " "), "/bin/bash --noprofile --norc -euo pipefail -c") {
						t.Fatal("exec is not fixed")
					}
					if args[len(args)-1] != podProbe {
						t.Fatal("probe differs from reviewed embedded source")
					}
					if scenario == "exec-failed" {
						return measured, errors.New("PRIVATE_SENTINEL")
					}
					return measured, nil
				}
				if verb != "get" {
					t.Fatalf("mutation attempted: %q", args)
				}
				if args[4] == "pod" {
					if execs == 0 {
						return pod, nil
					}
					return after, nil
				}
				if args[4] == "configmap" {
					if execs == 0 {
						return cm, nil
					}
					return cmAfter, nil
				}
				t.Fatalf("unexpected resource: %q", args)
				return nil, nil
			}
			verdict, code := collect(context.Background(), fake, func(context.Context) error { t.Fatal("unexpected wait"); return nil })
			if scenario == "healthy" {
				if verdict != "OBSERVED_HEALTHY_SAMPLE" || code != 0 {
					t.Fatalf("got %s/%d", verdict, code)
				}
			} else if verdict != "INCONCLUSIVE" || code != 3 {
				t.Fatalf("unsafe %s returned %s/%d", scenario, verdict, code)
			}
			if scenario == "unexpected-image" || scenario == "api-failed" {
				if execs != 0 {
					t.Fatal("unverified target reached exec")
				}
			} else if execs != 1 {
				t.Fatalf("exec count %d", execs)
			}
			if calls > 5 {
				t.Fatalf("unexpected unbounded reads: %d", calls)
			}
		})
	}
}

func TestDispatchGuardAndOutputDoNotExposeAPIData(t *testing.T) {
	for _, key := range []string{"GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_RUN_ATTEMPT", "MAIL_STARTUP_CONFIRM"} {
		t.Run(key, func(t *testing.T) {
			env := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "MAIL_STARTUP_CONFIRM": "read-email-startup-state"}
			env[key] = "unapproved"
			var out bytes.Buffer
			code := runEntry(context.Background(), func(k string) string { return env[k] }, func(context.Context, ...string) ([]byte, error) {
				t.Fatal("invalid dispatch reached API")
				return nil, nil
			}, func(context.Context) error { return nil }, &out)
			if code != 3 || out.String() != "MAIL_STARTUP_SAMPLE=INCONCLUSIVE\n" {
				t.Fatalf("unexpected public result %d/%q", code, out.String())
			}
		})
	}
}

func TestMalformedOrUnboundedMeasurementsAreInconclusive(t *testing.T) {
	for _, body := range []string{"", `{}`, `null`, strings.Repeat("x", maxOutput+1), `{"local_health":200} {}`} {
		if _, e := decodeMeasurement([]byte(body)); e == nil {
			t.Fatalf("incomplete measurement accepted: %q", body[:min(len(body), 40)])
		}
	}
}

func TestDuplicateNullAndInvalidMeasurementFieldsAreRefused(t *testing.T) {
	b, e := json.Marshal(goodMeasurement())
	if e != nil {
		t.Fatal(e)
	}
	valid := string(b)
	for _, body := range []string{
		strings.TrimSuffix(valid, "}") + `,"local_health":200}`,
		strings.Replace(valid, `"key_readable":true`, `"key_readable":null`, 1),
		strings.Replace(valid, `"write_locks":["08:01:42"]`, `"write_locks":null`, 1),
		strings.Replace(valid, `"local_health":200`, `"local_health":999`, 1),
		strings.Replace(valid, `"management_listeners":1`, `"management_listeners":-1`, 1),
		valid + ` {}`,
	} {
		if _, e := decodeMeasurement([]byte(body)); e == nil {
			t.Fatal("invalid measurement accepted")
		}
	}
}

func TestAPIBufferRefusesTruncation(t *testing.T) {
	b := boundedBuffer{limit: 4}
	if n, e := b.Write([]byte("test")); n != 4 || e != nil {
		t.Fatal("complete bounded response failed")
	}
	if n, e := b.Write([]byte("extra")); n != 0 || e == nil || b.String() != "test" {
		t.Fatal("oversized response was silently truncated")
	}
}

type failedWriter struct{ short bool }

// admittedEnvironment models only the five independently required dispatch facts.
func admittedEnvironment(k string) string {
	return map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "MAIL_STARTUP_CONFIRM": "read-email-startup-state"}[k]
}

// healthyExecutor returns a complete stable synthetic observation in call order.
func healthyExecutor(t *testing.T) executor {
	t.Helper()
	m := goodMeasurement()
	m.ConfigSHA256 = hashBytes([]byte(publicConfig))
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	responses := [][]byte{fixturePod(t), fixtureMap(t), b, fixturePod(t), fixtureMap(t)}
	index := 0
	return func(context.Context, ...string) ([]byte, error) {
		if index >= len(responses) {
			t.Fatal("unexpected extra request")
		}
		out := responses[index]
		index++
		return out, nil
	}
}

// Write models a failed or short public log write without retaining its payload.
func (w failedWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

func TestFailedPublicOutputCannotSucceed(t *testing.T) {
	for _, short := range []bool{false, true} {
		if code := runEntry(context.Background(), admittedEnvironment, healthyExecutor(t), nil, failedWriter{short: short}); code != 3 {
			t.Fatalf("failed output returned %d", code)
		}
	}
}
