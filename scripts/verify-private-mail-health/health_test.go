package health

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type healthProbe struct {
	Exec             *struct{ Command []string }
	HTTPGet          *struct{ Path string } `yaml:"httpGet"`
	TimeoutSeconds   int                    `yaml:"timeoutSeconds"`
	PeriodSeconds    int                    `yaml:"periodSeconds"`
	FailureThreshold int                    `yaml:"failureThreshold"`
}

func renderedChecks(t *testing.T) map[string]healthProbe {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "kubectl", "kustomize", "../../k8s/bases/apps/private-mail-administration").Output()
	if err != nil {
		t.Fatalf("render administration: %v", err)
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	checks := map[string]healthProbe{}
	for {
		var object struct {
			Kind     string
			Metadata struct{ Name string }
			Spec     struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name    string
							Startup healthProbe `yaml:"startupProbe"`
							Ready   healthProbe `yaml:"readinessProbe"`
							Live    healthProbe `yaml:"livenessProbe"`
						}
					}
				}
			}
		}
		if err = d.Decode(&object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if object.Kind != "StatefulSet" || object.Metadata.Name != "private-mail" {
			continue
		}
		for _, c := range object.Spec.Template.Spec.Containers {
			if c.Name == "private-mail" {
				if len(checks) != 0 {
					t.Fatal("repeated serving container")
				}
				checks = map[string]healthProbe{"startup": c.Startup, "ready": c.Ready, "live": c.Live}
			}
		}
	}
	if len(checks) != 3 {
		t.Fatal("missing serving health checks")
	}
	for name, p := range checks {
		if p.Exec == nil || len(p.Exec.Command) < 3 || p.Exec.Command[0] != "/usr/bin/env" || p.Exec.Command[1] != "-i" || p.HTTPGet != nil {
			t.Fatalf("%s self-check still depends on the client-facing network path", name)
		}
		if p.TimeoutSeconds != 2 {
			t.Fatalf("%s lacks bounded execution overhead", name)
		}
	}
	if checks["startup"].PeriodSeconds != 5 || checks["startup"].FailureThreshold != 24 {
		t.Fatal("startup failure budget changed")
	}
	return checks
}

func runCheck(t *testing.T, p healthProbe, target string) error {
	t.Helper()
	args := append([]string(nil), p.Exec.Command...)
	count := 0
	for i := range args {
		count += strings.Count(args[i], "http://127.0.0.1:8080")
		args[i] = strings.ReplaceAll(args[i], "http://127.0.0.1:8080", target)
	}
	if count != 1 {
		t.Fatal("probe must have exactly one fixed loopback health target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/env", args[1:]...)
	hook := filepath.Join(t.TempDir(), "untrusted-shell-hook")
	if err := os.WriteFile(hook, []byte("exit 97\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "BASH_ENV=" + hook}
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("probe exceeded its own request deadline")
	}
	if err == nil && len(output) != 0 {
		t.Fatal("successful probe leaked response content")
	}
	return err
}

func TestRenderedMailSelfCheckOutcomes(t *testing.T) {
	checks := renderedChecks(t)
	cases := []struct {
		name  string
		code  int
		delay time.Duration
		ok    bool
	}{
		{"healthy", 200, 0, true}, {"unexpected-success", 201, 0, false},
		{"redirect", 302, 0, false}, {"unauthorized", 401, 0, false},
		{"missing", 404, 0, false}, {"server-error", 500, 0, false},
		{"deadline", 200, 1600 * time.Millisecond, false},
	}
	for name, p := range checks {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				path := "/healthz/live"
				if name == "ready" {
					path = "/healthz/ready"
				}
				seen := make(chan string, 1)
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					select {
					case seen <- r.URL.Path:
					default:
					}
					if tc.delay > 0 {
						select {
						case <-time.After(tc.delay):
						case <-r.Context().Done():
							return
						}
					}
					w.Header().Set("Location", "/healthz/live")
					w.WriteHeader(tc.code)
					_, _ = io.WriteString(w, "private response fixture")
				}))
				defer s.Close()
				err := runCheck(t, p, s.URL)
				if (err == nil) != tc.ok {
					t.Fatalf("HTTP %d accepted=%t, want %t", tc.code, err == nil, tc.ok)
				}
				select {
				case got := <-seen:
					if got != path {
						t.Fatalf("checked %s, want %s", got, path)
					}
				default:
					t.Fatal("health endpoint was not requested")
				}
			})
		}
		t.Run(name+"/connection-refused", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var listenConfig net.ListenConfig
			l, err := listenConfig.Listen(ctx, "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			target := "http://" + l.Addr().String()
			if err = l.Close(); err != nil {
				t.Fatal(err)
			}
			if runCheck(t, p, target) == nil {
				t.Fatal("unreachable health endpoint accepted")
			}
		})
	}
}
