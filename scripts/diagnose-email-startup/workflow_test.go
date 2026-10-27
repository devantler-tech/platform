package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowStep struct {
	Name, Run, Uses, Shell string
	Env                    map[string]string
	With                   map[string]any
}
type observationWorkflow struct {
	On          map[string]any
	Permissions map[string]string
	Concurrency struct {
		Group  string
		Cancel bool `yaml:"cancel-in-progress"`
		Queue  string
	}
	Jobs map[string]struct {
		Environment string
		Timeout     int `yaml:"timeout-minutes"`
		Permissions map[string]string
		Env         map[string]string
		Steps       []workflowStep
	}
}

// readObservationWorkflow loads the shipped workflow rather than a copied script.
func readObservationWorkflow(t *testing.T) observationWorkflow {
	t.Helper()
	b, e := os.ReadFile("../../.github/workflows/read-email-startup-state.yaml")
	if e != nil {
		t.Fatal(e)
	}
	var w observationWorkflow
	if e = yaml.Unmarshal(b, &w); e != nil {
		t.Fatal(e)
	}
	return w
}

// TestWorkflowKeepsManualMainGuardBeforeCredentials executes the shipped dispatch guard in isolation.
func TestWorkflowKeepsManualMainGuardBeforeCredentials(t *testing.T) {
	w := readObservationWorkflow(t)
	if len(w.On) != 1 || w.On["workflow_dispatch"] == nil || w.Permissions == nil || len(w.Permissions) != 0 ||
		w.Concurrency.Group != "prod-deploy" || w.Concurrency.Cancel || w.Concurrency.Queue != "max" || len(w.Jobs) != 1 {
		t.Fatal("manual dispatch, token or serialization contract changed")
	}
	j, ok := w.Jobs["observe"]
	if !ok || j.Environment != "prod" || j.Timeout != 10 || len(j.Permissions) != 1 || j.Permissions["contents"] != "read" ||
		j.Env["MAIL_STARTUP_CONFIRM"] != "$"+"{{ inputs.confirm }}" || len(j.Steps) != 6 {
		t.Fatal("job contract changed")
	}
	s := j.Steps
	if s[0].Uses != "" || s[0].Shell != "bash" ||
		s[1].Uses != "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1" ||
		s[1].With["ref"] != "$"+"{{ github.sha }}" || s[1].With["persist-credentials"] != false ||
		s[2].With["cache"] != false || !strings.Contains(s[3].Run, "go build") {
		t.Fatal("reviewed-source guard/build order changed")
	}
	for i := 0; i < 5; i++ {
		for _, value := range s[i].Env {
			if strings.Contains(value, "secrets.") {
				t.Fatal("credentials available before guard/build")
			}
		}
	}
	if len(s[5].Env) != 2 || s[5].Env["KUBE_CONFIG"] != "$"+"{{ secrets.KUBE_CONFIG }}" ||
		s[5].Env["HCLOUD_TOKEN"] != "$"+"{{ secrets.HCLOUD_TOKEN }}" {
		t.Fatal("unexpected credential surface")
	}
	for _, key := range []string{"GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_RUN_ATTEMPT", "MAIL_STARTUP_CONFIRM", "valid"} {
		t.Run(key, func(t *testing.T) {
			env := []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "KUBECONFIG=" + filepath.Join(t.TempDir(), "absent")}
			for _, name := range []string{"GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_RUN_ATTEMPT", "MAIL_STARTUP_CONFIRM"} {
				value := admittedEnvironment(name)
				if name == key {
					value = "unapproved"
				}
				env = append(env, name+"="+value)
			}
			cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", s[0].Run)
			cmd.Env = env
			b, e := cmd.CombinedOutput()
			if (e == nil) != (key == "valid") || len(b) != 0 {
				t.Fatalf("guard result %v/%q", e, b)
			}
		})
	}
}

// TestWorkflowKeepsCredentialValuesAndHelperErrorsPrivate executes both success and error shell paths.
func TestWorkflowKeepsCredentialValuesAndHelperErrorsPrivate(t *testing.T) {
	step := readObservationWorkflow(t).Jobs["observe"].Steps[5]
	for _, scenario := range []string{"healthy", "endpoint-fails", "empty-config", "oversized-config"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			runner := filepath.Join(base, "runner with spaces")
			scripts := filepath.Join(base, "scripts")
			for _, p := range []string{runner, scripts} {
				if e := os.Mkdir(p, 0700); e != nil {
					t.Fatal(e)
				}
			}
			helper := "#!/bin/bash\nset -euo pipefail\n[[ -z ${KUBE_CONFIG+x} && -n ${HCLOUD_TOKEN} && -s $KUBECONFIG ]]\n"
			if scenario == "endpoint-fails" {
				helper += "printf 'PRIVATE_HELPER_SENTINEL\\n'; printf 'PRIVATE_HELPER_SENTINEL\\n' >&2; exit 1\n"
			}
			if e := os.WriteFile(filepath.Join(scripts, "use-prod-stable-api-endpoint.sh"), []byte(helper), 0700); e != nil {
				t.Fatal(e)
			}
			observer := "#!/bin/bash\nset -euo pipefail\n[[ -z ${KUBE_CONFIG+x} && -z ${HCLOUD_TOKEN+x} && -s $KUBECONFIG ]]\nprintf 'MAIL_STARTUP_SAMPLE=OBSERVED_HEALTHY_SAMPLE\\n'\n"
			if e := os.WriteFile(filepath.Join(runner, "observe-email-startup"), []byte(observer), 0700); e != nil {
				t.Fatal(e)
			}
			config := "PRIVATE_CONFIG_SENTINEL"
			if scenario == "empty-config" {
				config = ""
			}
			if scenario == "oversized-config" {
				config = strings.Repeat("x", 1048577)
			}
			// Avoid an oversized environment exceeding the host's exec limit:
			// assign its synthetic value in the isolated shell instead.
			prefix := ""
			if scenario == "oversized-config" {
				config = ""
				prefix = "printf -v KUBE_CONFIG '%1048577s' x\nexport KUBE_CONFIG\n"
			}
			cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", prefix+step.Run)
			cmd.Dir = base
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + base, "RUNNER_TEMP=" + runner, "KUBE_CONFIG=" + config, "HCLOUD_TOKEN=PRIVATE_TOKEN_SENTINEL"}
			b, e := cmd.CombinedOutput()
			want := "MAIL_STARTUP_SAMPLE=INCONCLUSIVE\n"
			if scenario == "healthy" {
				want = "MAIL_STARTUP_SAMPLE=OBSERVED_HEALTHY_SAMPLE\n"
			}
			if string(b) != want || (e == nil) != (scenario == "healthy") {
				t.Fatalf("public result %v/%q", e, b)
			}
			matches, e := filepath.Glob(filepath.Join(runner, "email-startup-context.*"))
			if e != nil || len(matches) != 0 {
				t.Fatal("private credential directory survived")
			}
		})
	}
}

// TestAPIErrorsCannotEscapeToPublicOutput confines private response and error payloads.
func TestAPIErrorsCannotEscapeToPublicOutput(t *testing.T) {
	var out bytes.Buffer
	code := runEntry(context.Background(), admittedEnvironment, func(context.Context, ...string) ([]byte, error) {
		return []byte("PRIVATE_RESPONSE_SENTINEL"), errors.New("PRIVATE_ERROR_SENTINEL")
	}, nil, &out)
	if code != 3 || out.String() != "MAIL_STARTUP_SAMPLE=INCONCLUSIVE\n" {
		t.Fatalf("public result %d/%q", code, out.String())
	}
}

// TestWaitingForAContainerIsFiniteAndNeverExecutes bounds waiting without probing an unready process.
func TestWaitingForAContainerIsFiniteAndNeverExecutes(t *testing.T) {
	p := changePod(t, fixturePod(t), func(p map[string]any) {
		p["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["state"] = map[string]any{"waiting": map[string]any{"reason": "SyntheticBackoff"}}
	})
	reads, pauses := 0, 0
	verdict, code := collect(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
		reads++
		if len(args) < 5 || args[3] != "get" || args[4] != "pod" {
			t.Fatal("waiting container reached exec or unrelated read")
		}
		return p, nil
	}, func(context.Context) error { pauses++; return nil })
	if verdict != "INCONCLUSIVE" || code != 3 || reads != 11 || pauses != 10 {
		t.Fatalf("unbounded wait %s/%d reads=%d pauses=%d", verdict, code, reads, pauses)
	}
}
