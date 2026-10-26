package recovery_test

import (
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

func commandExitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	t.Fatal(err)
	return -1
}

func recoveryFixture(t *testing.T, reader string, ref string) (*exec.Cmd, string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	source := filepath.Join(filepath.Dir(file), "..", "..", "verify-private-mail-storage-recovery.sh")
	code, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("recovery verifier is unavailable: %v", err)
	}
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	if err := os.Mkdir(scripts, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"verify-private-mail-storage-recovery.sh": code, "check-flux-orphaned-objects.sh": []byte(reader)} {
		if err := os.WriteFile(filepath.Join(scripts, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", filepath.Join(scripts, "verify-private-mail-storage-recovery.sh"))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RUNNER_TEMP=" + root,
		"GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_REF=" + ref,
		"GITHUB_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"PRIVATE_MAIL_RECOVERY_CONFIRM=verify-private-mail-storage-recovery",
		"GITHUB_STEP_SUMMARY=" + filepath.Join(root, "summary"),
		"FLUX_ORPHANS_SINCE=caller-selected-window", "FLUX_ORPHANS_CONTEXT=other-cluster",
		"READER_TOUCHED=" + filepath.Join(root, "reader-touched")}
	return cmd, root
}

func TestRecoveryVerifierContainsPrivilegedReaderOutput(t *testing.T) {
	for _, tt := range []struct {
		name, footer, verdict string
		readerExit, wantExit  int
	}{
		{"clean", "✅ No object is outside every inventory across both reads among fixed scope (42 Flux-applied objects).", "PASS", 0, 0},
		{"cleared", "✅ Nothing stayed outside every inventory across 3 reads among fixed scope: cleared.", "PASS", 0, 0},
		{"finding", "", "FAIL", 1, 1},
		{"unreadable", "", "UNVERIFIED", 2, 2},
		{"unexpected failure", "", "UNVERIFIED", 7, 2},
		{"empty success", "", "UNVERIFIED", 0, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := fmt.Sprintf(`set -eu
test "$FLUX_ORPHANS_SINCE" = 2026-10-26T07:28:50Z
test "$FLUX_ORPHANS_BASE_SHA" = ac76568f45ac2196f34ea8c4741000ad6db18b84
test "$FLUX_ORPHANS_RECOVERY_SHA" = aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
test "$FLUX_ORPHANS_CONTEXT" = admin@prod
test "$FLUX_ORPHANS_SETTLE_SECONDS" = 60
test "$FLUX_ORPHANS_RETRY_SECONDS" = 20
test "$FLUX_ORPHANS_KUBECTL_BIN" = kubectl
touch "$READER_TOUCHED"
if test -n "${GITHUB_STEP_SUMMARY:-}"; then echo private-canary > "$GITHUB_STEP_SUMMARY"; fi
echo private-canary
echo private-canary >&2
printf '%%s\n' '%s'
exit %d
`, tt.footer, tt.readerExit)
			cmd, root := recoveryFixture(t, reader, "refs/heads/main")
			output, err := cmd.CombinedOutput()
			code := commandExitCode(t, err)
			if code != tt.wantExit || string(output) != "MAIL_STORAGE_RECOVERY="+tt.verdict+"\n" {
				t.Fatalf("verdict/exit mismatch: code=%d output=%q", code, output)
			}
			if _, err := os.Stat(filepath.Join(root, "summary")); !os.IsNotExist(err) {
				t.Fatal("the privileged reader must not publish its private summary")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "mail-storage-recovery.") {
					t.Fatal("private scan output must be removed after every verdict")
				}
			}
		})
	}
}

func TestRecoveryVerifierRefusesUnreviewedInvocationBeforeReadingCluster(t *testing.T) {
	for _, tt := range []struct{ name, environment string }{
		{"ref", "GITHUB_REF=refs/heads/unreviewed"},
		{"event", "GITHUB_EVENT_NAME=push"},
		{"missing revision", "GITHUB_SHA="},
		{"short revision", "GITHUB_SHA=aaaaaaa"},
		{"confirmation", "PRIVATE_MAIL_RECOVERY_CONFIRM=other"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd, root := recoveryFixture(t, `touch "$READER_TOUCHED"`, "refs/heads/main")
			cmd.Env = append(cmd.Env, tt.environment)
			output, err := cmd.CombinedOutput()
			if commandExitCode(t, err) != 2 || string(output) != "MAIL_STORAGE_RECOVERY=UNVERIFIED\n" {
				t.Fatalf("unreviewed invocation must be refused: error=%v output=%q", err, output)
			}
			if _, err := os.Stat(filepath.Join(root, "reader-touched")); !os.IsNotExist(err) {
				t.Fatal("unreviewed invocation reached the cluster reader")
			}
		})
	}
}

func TestRecoveryWorkflowRefusesUnreviewedCodeBeforeCheckout(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", ".github", "workflows", "verify-private-mail-storage-recovery.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Concurrency struct {
			Group  string `yaml:"group"`
			Cancel bool   `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			Environment string            `yaml:"environment"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run  string         `yaml:"run"`
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	_, dispatch := workflow.On["workflow_dispatch"]
	job, exists := workflow.Jobs["verify"]
	if !dispatch || len(workflow.On) != 1 || len(workflow.Jobs) != 1 || !exists || len(workflow.Permissions) != 0 || job.Environment != "prod" || len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatal("recovery must be dispatch-only with the existing prod environment and read-only repository permission")
	}
	if workflow.Concurrency.Group != "prod-deploy" || workflow.Concurrency.Cancel {
		t.Fatal("the read must serialize with deployment without canceling it")
	}
	if len(job.Steps) < 2 || job.Steps[0].Run == "" || job.Steps[0].Uses != "" || job.Steps[1].With["ref"] != "${{ github.sha }}" || job.Steps[1].With["persist-credentials"] != false || job.Steps[1].With["fetch-depth"] != 0 {
		t.Fatal("the refusal must precede checkout of the pinned revision and its recovery history")
	}
	for _, tt := range []struct {
		name, event, ref, confirm string
		want                      int
	}{
		{"approved", "workflow_dispatch", "refs/heads/main", "verify-private-mail-storage-recovery", 0},
		{"other ref", "workflow_dispatch", "refs/heads/unreviewed", "verify-private-mail-storage-recovery", 2},
		{"other event", "push", "refs/heads/main", "verify-private-mail-storage-recovery", 2},
		{"other confirmation", "workflow_dispatch", "refs/heads/main", "other", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", job.Steps[0].Run)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_EVENT_NAME=" + tt.event, "GITHUB_REF=" + tt.ref, "CONFIRM=" + tt.confirm}
			_, err := cmd.CombinedOutput()
			got := commandExitCode(t, err)
			if got != tt.want {
				t.Fatalf("checkout guard exit=%d, want %d", got, tt.want)
			}
		})
	}
}
