package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// proxyFixture admits the declared public adapter, without company data or credentials.
func proxyFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	image := "ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:3c83be721743e85c455eaacb82d6db0ad442496c9d8c2d54b509430489ec9f82"
	p := changePod(t, fixturePod(t), func(p map[string]any) {
		spec := p["spec"].(map[string]any)
		containers := spec["containers"].([]any)
		containers[0].(map[string]any)["image"] = image
		spec["containers"] = append(containers, map[string]any{
			"name": "client-proxy", "image": image, "command": []string{"/usr/local/bin/caddy"},
			"args":         []string{"run", "--config", "/etc/email-client-proxy/Caddyfile", "--adapter", "caddyfile"},
			"volumeMounts": []any{map[string]any{"name": "client-proxy-config", "mountPath": "/etc/email-client-proxy", "readOnly": true}},
		})
		spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "client-proxy-config", "configMap": map[string]any{"name": "email-client-proxy-kkm788c4m9"}})
		status := p["status"].(map[string]any)
		status["containerStatuses"] = append(status["containerStatuses"].([]any), map[string]any{
			"name": "client-proxy", "containerID": "containerd://proxy", "imageID": "sha256:proxy", "restartCount": 0, "ready": true,
			"state": map[string]any{"running": map[string]any{"startedAt": "2026-10-27T00:00:00Z"}},
		})
	})
	config, err := os.ReadFile("../../k8s/bases/apps/private-mail-proxy-identity/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	cm, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": "email-client-proxy-kkm788c4m9", "namespace": "private-mail", "uid": "00000000-0000-0000-0000-000000000004", "resourceVersion": "9"},
		"data":     map[string]string{"Caddyfile": string(config)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, cm
}

// TestProxyObservationRejectsUnboundExecution catches accepting an altered adapter or stale observation.
func TestProxyObservationRejectsUnboundExecution(t *testing.T) {
	for _, scenario := range []string{"healthy", "reordered", "unreviewed-image", "retired-serving-image", "retired-proxy-image", "injected-environment", "secret-mount", "changed-command", "unknown-map", "changed-public-config", "proxy-config-replaced", "proxy-restarted", "proxy-unready"} {
		t.Run(scenario, func(t *testing.T) {
			before, proxyMap := proxyFixture(t)
			if scenario == "reordered" {
				before = changePod(t, before, func(p map[string]any) {
					containers := p["spec"].(map[string]any)["containers"].([]any)
					containers[0], containers[1] = containers[1], containers[0]
					statuses := p["status"].(map[string]any)["containerStatuses"].([]any)
					statuses[0], statuses[1] = statuses[1], statuses[0]
				})
			}
			after := append([]byte(nil), before...)
			if scenario != "healthy" && scenario != "changed-public-config" && scenario != "proxy-restarted" && scenario != "proxy-unready" {
				before = changePod(t, before, func(p map[string]any) {
					spec := p["spec"].(map[string]any)
					proxy := spec["containers"].([]any)[1].(map[string]any)
					switch scenario {
					case "unreviewed-image":
						proxy["image"] = "unreviewed"
					case "retired-serving-image":
						spec["containers"].([]any)[0].(map[string]any)["image"] = "ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:1d812772d101dbb1499c23319b0b8dfe77dc8079f4e45ab046b77dd2ac244376"
					case "retired-proxy-image":
						proxy["image"] = "ghcr.io/devantler-tech/platform-private-mail-bootstrap@sha256:1d812772d101dbb1499c23319b0b8dfe77dc8079f4e45ab046b77dd2ac244376"
					case "injected-environment":
						proxy["env"] = []any{map[string]any{"name": "PRIVATE_SENTINEL", "value": "unreviewed"}}
					case "secret-mount":
						proxy["volumeMounts"] = append(proxy["volumeMounts"].([]any), map[string]any{"name": "credentials", "mountPath": "/credentials"})
					case "changed-command":
						proxy["args"] = []string{"run", "--config", "/unreviewed"}
					case "unknown-map":
						spec["volumes"].([]any)[1].(map[string]any)["configMap"] = map[string]any{"name": "unreviewed"}
					}
				})
			}
			if scenario == "proxy-restarted" || scenario == "proxy-unready" {
				after = changePod(t, after, func(p map[string]any) {
					proxy := p["status"].(map[string]any)["containerStatuses"].([]any)[1].(map[string]any)
					if scenario == "proxy-restarted" {
						proxy["restartCount"] = 1
					} else {
						proxy["ready"] = false
					}
				})
			}
			if scenario == "changed-public-config" {
				proxyMap = []byte(strings.Replace(string(proxyMap), "keepalive off", "keepalive 1m", 1))
			}
			execs := 0
			read := func(_ context.Context, args ...string) ([]byte, error) {
				if len(args) < 5 {
					return nil, errors.New("invalid request")
				}
				switch args[3] {
				case "exec":
					execs++
					m := goodMeasurement()
					m.ConfigSHA256 = hashBytes([]byte(publicConfig))
					return json.Marshal(m)
				case "get":
					if args[4] == "pod" {
						if execs == 0 {
							return before, nil
						}
						return after, nil
					}
					if args[4] == "configmap" && args[5] == "email-client-proxy-kkm788c4m9" {
						if scenario == "proxy-config-replaced" && execs > 0 {
							return []byte(strings.Replace(string(proxyMap), `"resourceVersion":"9"`, `"resourceVersion":"10"`, 1)), nil
						}
						return proxyMap, nil
					}
					if args[4] == "configmap" && args[5] == "private-mail-datastore" {
						return fixtureMap(t), nil
					}
				}
				return nil, errors.New("unexpected request")
			}
			verdict, code := collect(context.Background(), read, func(context.Context) error { t.Fatal("unexpected wait"); return nil })
			if scenario == "healthy" || scenario == "reordered" {
				if code != 0 || verdict != "OBSERVED_HEALTHY_SAMPLE" || execs != 1 {
					t.Fatalf("declared adapter cannot be observed: %s/%d execs=%d", verdict, code, execs)
				}
			} else if code == 0 || verdict == "OBSERVED_HEALTHY_SAMPLE" {
				t.Fatal("unbound or unready adapter produced a healthy observation")
			}
			if scenario != "healthy" && scenario != "reordered" && scenario != "proxy-config-replaced" && scenario != "proxy-restarted" && scenario != "proxy-unready" && execs != 0 {
				t.Fatal("unverified adapter reached execution")
			}
		})
	}
}
