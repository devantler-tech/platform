package initialwait

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Run the manifest's actual shell entrypoint. Only the remote CLI and the
// three-second backoff are replaced; initialization and recovery guards run.
func TestInitialMemberWait(t *testing.T) {
	data, err := os.ReadFile("../../../k8s/bases/infrastructure/vault-config/job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					InitContainers []struct {
						Name    string
						Command []string
					} `yaml:"initContainers"`
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, container := range job.Spec.Template.Spec.InitContainers {
		if container.Name == "vault-init" && len(container.Command) == 3 {
			script = container.Command[2]
		}
	}
	if script == "" {
		t.Fatal("vault-init shell entrypoint missing")
	}
	for _, tc := range []struct {
		name      string
		reachable bool
	}{
		{"unavailable", false},
		{"malformed", false},
		{"missing-field", false},
		{"failed-with-json", false},
		{"initialized", true},
		{"eventually-initialized", true},
		{"sealed", true},
		{"uninitialized-retained-key", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "uninitialized-retained-key" {
				if err := os.Mkdir(filepath.Join(dir, "keys"), 0700); err != nil {
					t.Fatal(err)
				}
				write("keys/unseal-key", "synthetic-retained-key")
			}
			write("bao", `#!/bin/sh
if [ "$1" != status ] || [ "$2" != -format=json ]; then
  printf 'mutation\n' >> "$FIXTURE/mutations"
  exit 94
fi
printf '%s %s\n' "${BAO_CLIENT_TIMEOUT:-unset}" "${BAO_MAX_RETRIES:-unset}" >> "$FIXTURE/calls"
count=$(wc -l < "$FIXTURE/calls")
case "$CASE" in
  unavailable) printf 'sensitive-fixture\n' >&2; exit 1 ;;
  malformed) printf '{\n  "initialized": "unknown"\n}\n'; exit 0 ;;
  missing-field) printf '{}\n'; exit 0 ;;
  eventually-initialized) [ "$count" -gt 2 ] || exit 1 ;;
  uninitialized-retained-key) printf '{\n  "initialized": false,\n  "sealed": true\n}\n'; exit 2 ;;
esac
printf '{\n  "initialized": true,\n  "sealed": true\n}\n'
[ "$CASE" != failed-with-json ] || exit 1
[ "$CASE" != sealed ] || exit 2
`)
			write("sleep", "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$FIXTURE/sleeps\"\n")
			body := strings.NewReplacer("/openbao/unseal/", dir+"/keys/", "/shared/", dir+"/shared/", "/snapshots/", dir+"/snapshots/").Replace(script)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-ec", body)
			cmd.WaitDelay = 500 * time.Millisecond
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "FIXTURE=" + dir, "CASE=" + tc.name, "openbao_replicas=1"}
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("the first-member wait did not terminate within its finite attempt budget")
			}
			if runErr == nil {
				t.Fatal("the empty fixture must stop before initialization or unsealing")
			}
			if strings.Contains(string(output), "openbao-0 is reachable.") != tc.reachable {
				t.Fatalf("unexpected first-member reachability result: %s", output)
			}
			if _, err := os.Stat(filepath.Join(dir, "mutations")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("an unavailable member or missing stored keys must never authorize a mutation")
			}
			if strings.Contains(string(output), "sensitive-fixture") {
				t.Fatal("remote command output escaped the non-secret diagnostic")
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			rows := strings.Fields(strings.SplitN(string(calls), "\n", 2)[0])
			if len(rows) != 2 || rows[1] != "0" {
				t.Fatal("first-member status must disable hidden client retries")
			}
			requestLimit, err := time.ParseDuration(rows[0])
			if err != nil || requestLimit <= 0 {
				t.Fatal("first-member status must impose a finite client deadline")
			}
			if !tc.reachable {
				attempts := strings.Count(string(calls), "\n")
				// Every attempt can exhaust its HTTP deadline, followed by a
				// three-second backoff. Leave half the 20-minute health window
				// for later configuration and reporting.
				if time.Duration(attempts)*(requestLimit+3*time.Second) > 10*time.Minute {
					t.Fatal("first-member failure can consume the enclosing health-check window")
				}
			} else if tc.name == "uninitialized-retained-key" {
				if !strings.Contains(string(output), "Refusing to auto-initialize") {
					t.Fatalf("reachability bypassed the retained-key recovery guard: %s", output)
				}
			} else if !strings.Contains(string(output), "no unseal key is available") {
				t.Fatalf("reachability bypassed the existing stored-key guard: %s", output)
			}
		})
	}
}
