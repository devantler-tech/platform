// Package prodoperation tests .github/workflows/prod-operation.yaml, the single
// reusable workflow through which dispatched production operations reach the
// production credential (platform#6268), and every workflow that calls it.
package prodoperation

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	reusablePath = "../../.github/workflows/prod-operation.yaml"
	workflowsDir = "../../.github/workflows"
	reusableUses = "./.github/workflows/prod-operation.yaml"
)

type step struct {
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
	Run             string            `yaml:"run"`
	Uses            string            `yaml:"uses"`
	Env             map[string]string `yaml:"env"`
	With            map[string]any    `yaml:"with"`
}

type job struct {
	Uses        string            `yaml:"uses"`
	With        map[string]any    `yaml:"with"`
	Secrets     any               `yaml:"secrets"`
	Environment string            `yaml:"environment"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency map[string]any    `yaml:"concurrency"`
	Env         map[string]string `yaml:"env"`
	Steps       []step            `yaml:"steps"`
}

type workflow struct {
	On          map[string]any    `yaml:"on"`
	Concurrency any               `yaml:"concurrency"`
	Permissions map[string]string `yaml:"permissions"`
	Env         map[string]string `yaml:"env"`
	Jobs        map[string]job    `yaml:"jobs"`
}

func load(t *testing.T, path string) (workflow, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	return w, raw
}

func operate(t *testing.T) (job, []byte) {
	t.Helper()
	w, raw := load(t, reusablePath)
	if len(w.On) != 1 || w.On["workflow_call"] == nil || len(w.Permissions) != 0 || len(w.Env) != 0 || len(w.Jobs) != 1 {
		t.Fatal("prod-operation must be workflow_call-only with empty top-level grants and one job")
	}
	j, ok := w.Jobs["operate"]
	if !ok {
		t.Fatal("missing operate job")
	}
	return j, raw
}

func TestReusableWorkflowBoundary(t *testing.T) {
	j, _ := operate(t)
	if j.Environment != "prod" || len(j.Permissions) != 1 || j.Permissions["contents"] != "read" {
		t.Fatal("operation job must bind the prod environment with contents: read only")
	}
	if j.Concurrency["group"] != "prod-deploy" || j.Concurrency["cancel-in-progress"] != false || j.Concurrency["queue"] != "max" {
		t.Fatal("operation must queue on the shared prod-deploy lock")
	}
	steps := j.Steps
	if len(steps) < 5 {
		t.Fatal("operation steps missing")
	}
	first := steps[0]
	if first.Run == "" || first.Uses != "" || first.If != "" || (first.ContinueOnError != nil && first.ContinueOnError != false) {
		t.Fatal("dispatch guard must be an unconditional first step before checkout")
	}
	checkout := steps[1]
	if checkout.Uses != "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1" ||
		checkout.With["ref"] != "${{ github.sha }}" || checkout.With["persist-credentials"] != false {
		t.Fatal("checkout is not bound to the reviewed dispatch revision")
	}
	if !strings.Contains(steps[2].Run, "./scripts/verify-prod-recovery-source.sh") {
		t.Fatal("current-main check must precede credentials")
	}
	built := false
	for i, s := range steps {
		if strings.Contains(s.Run, "go build ") {
			built = true
		}
		if strings.Contains(s.Uses, "upload-") || strings.Contains(s.Uses, "cache") {
			t.Fatal("private responses must not be uploaded or cached")
		}
		if hasSecret(s) && (i != len(steps)-1 || !built) {
			t.Fatal("credentials are not confined to the final step after compilation")
		}
	}
	last := steps[len(steps)-1]
	if last.Env["KUBE_CONFIG"] != "${{ secrets.KUBE_CONFIG }}" || last.Env["HCLOUD_TOKEN"] != "${{ secrets.HCLOUD_TOKEN }}" || len(last.Env) != 3 {
		t.Fatal("final step environment changed")
	}
	for _, fragment := range []string{"set +x", "umask 077", "mktemp -d /dev/shm/", "trap '", "unset KUBE_CONFIG", "unset HCLOUD_TOKEN"} {
		if !strings.Contains(last.Run, fragment) {
			t.Fatalf("missing credential boundary: %s", fragment)
		}
	}
}

func hasSecret(s step) bool {
	for _, v := range s.Env {
		if strings.Contains(v, "secrets.") {
			return true
		}
	}
	return false
}

func goodDispatch() map[string]string {
	return map[string]string{
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch",
		"GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_ACTOR": "devantler",
		"GITHUB_TRIGGERING_ACTOR": "devantler", "RUNNER_ENVIRONMENT": "github-hosted",
		"PROD_OPERATION": "observe-runtime-health", "PROD_OPERATION_CONFIRM": "observe-runtime-health",
		"PROD_OPERATION_PROGRAM": "scripts/observe-runtime-health", "PROD_OPERATION_ARGS": "",
		"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/observe-runtime-health.yaml@refs/heads/main",
	}
}

func TestGuardAdmitsOnlyConfirmedFirstMainDispatchByDevantler(t *testing.T) {
	j, _ := operate(t)
	guard := j.Steps[0].Run
	var lastOutput string
	run := func(env map[string]string) error {
		cmd := exec.Command("bash", "--noprofile", "--norc", "-c", guard)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		lastOutput = string(out)
		return err
	}
	if err := run(goodDispatch()); err != nil {
		t.Fatalf("reviewed dispatch refused: %v", err)
	}
	withArgs := goodDispatch()
	withArgs["PROD_OPERATION_ARGS"] = "account-census"
	if err := run(withArgs); err != nil {
		t.Fatalf("fixed arguments refused: %v", err)
	}
	for _, tc := range []struct{ key, bad string }{
		{"GITHUB_REPOSITORY", "fork/platform"},
		{"GITHUB_EVENT_NAME", "push"},
		{"GITHUB_EVENT_NAME", "workflow_call"},
		{"GITHUB_REF", "refs/heads/feature"},
		{"GITHUB_REF", "refs/tags/main"},
		{"GITHUB_RUN_ATTEMPT", "2"},
		{"GITHUB_ACTOR", "someone-else"},
		{"GITHUB_TRIGGERING_ACTOR", "someone-else"},
		{"RUNNER_ENVIRONMENT", "self-hosted"},
		{"PROD_OPERATION_CONFIRM", "yes"},
		{"PROD_OPERATION_CONFIRM", ""},
		{"PROD_OPERATION", "Bad Operation"},
		{"PROD_OPERATION_PROGRAM", "scripts/../cmd"},
		{"PROD_OPERATION_PROGRAM", "/usr/bin"},
		{"PROD_OPERATION_PROGRAM", "scripts/a/b"},
		{"PROD_OPERATION_ARGS", "x; rm -rf /"},
		{"PROD_OPERATION_ARGS", "$(id)"},
		{"PROD_OPERATION_ARGS", "--kubeconfig=/tmp/x"},
		{"GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/x.yaml@refs/heads/feature"},
		{"GITHUB_WORKFLOW_REF", "fork/platform/.github/workflows/x.yaml@refs/heads/main"},
		{"GITHUB_WORKFLOW_REF", ""},
	} {
		env := goodDispatch()
		env[tc.key] = tc.bad
		if tc.key == "PROD_OPERATION" {
			env["PROD_OPERATION_CONFIRM"] = tc.bad
		}
		if run(env) == nil {
			t.Fatalf("guard admitted %s=%q", tc.key, tc.bad)
		}
		if !strings.Contains(lastOutput, "::error::refused: ") {
			t.Fatalf("refusal of %s=%q names no reason: %q", tc.key, tc.bad, lastOutput)
		}
	}
}

// runCredentialStep executes the real final step of prod-operation.yaml as
// bash against fixtures: a fake endpoint helper, a fake program and an age
// binary (fake unless ageBin names a real one).
func runCredentialStep(t *testing.T, helperExit, programExit int, keep, args, ageBin, recipient string) (string, string, error) {
	t.Helper()
	j, _ := operate(t)
	stepRun := j.Steps[len(j.Steps)-1].Run
	mktemp, err := exec.LookPath("mktemp")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, dir := range []string{"scripts", "tools"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixtures := map[string]string{
		"scripts/use-prod-stable-api-endpoint.sh": "#!/bin/bash\nprintf '%s\\n' '::add-mask::192.0.2.1' >&2\nprintf '%s\\n' 'helper stdout must stay private'\nexit \"$OP_TEST_HELPER_EXIT\"\n",
		"tools/mktemp":         fmt.Sprintf("#!/bin/bash\nexec %q -d \"$OP_TEST_TMPDIR/prod-operation.XXXXXX\"\n", mktemp),
		"tools/prod-operation": "#!/bin/bash\n[[ -z ${KUBE_CONFIG+x} && -z ${KEEP_HCLOUD_TOKEN+x} && -s $KUBECONFIG ]] || exit 99\nprintf 'token=%s\\n' \"${HCLOUD_TOKEN:-unset}\"\nprintf 'argc=%s' \"$#\"; [[ $# -eq 0 ]] || printf ' %s' \"$@\"; printf '\\n'\nprintf '%s\\n' 'program stderr detail' >&2\nexit \"$OP_TEST_PROGRAM_EXIT\"\n",
	}
	if ageBin == "" {
		// A stand-in that records its arguments and wraps stdin in base64, so the
		// test can prove what was encrypted without a real key.
		fixtures["tools/age"] = "#!/bin/bash\nprintf '%s\\n' \"$*\" > \"$OP_TEST_TMPDIR/age-args\"\n[[ ${OP_TEST_AGE_FAIL:-0} == 1 ]] && exit 1\nprintf 'FAKE-AGE:'; base64 | tr -d '\\n'; printf '\\n'\n"
	} else {
		fixtures["tools/age"] = fmt.Sprintf("#!/bin/bash\nexec %q \"$@\"\n", ageBin)
	}
	for path, content := range fixtures {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", "-c", stepRun)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + filepath.Join(root, "tools") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RUNNER_TEMP=" + filepath.Join(root, "tools"), "OP_TEST_TMPDIR=" + root,
		fmt.Sprintf("OP_TEST_HELPER_EXIT=%d", helperExit), fmt.Sprintf("OP_TEST_PROGRAM_EXIT=%d", programExit),
		"KUBE_CONFIG=synthetic-config", "HCLOUD_TOKEN=synthetic-token", "KEEP_HCLOUD_TOKEN=" + keep,
		"PROD_OPERATION_ARGS=" + args, "PROD_OPERATION_RECIPIENT=" + recipient,
	}
	if os.Getenv("OP_TEST_AGE_FAIL") == "1" {
		cmd.Env = append(cmd.Env, "OP_TEST_AGE_FAIL=1")
	}
	output, runErr := cmd.CombinedOutput()
	leftovers, _ := filepath.Glob(filepath.Join(root, "prod-operation.*"))
	if len(leftovers) != 0 {
		t.Fatalf("credential directory not removed: %v", leftovers)
	}
	ageArgs, _ := os.ReadFile(filepath.Join(root, "age-args"))
	return string(output), strings.TrimSpace(string(ageArgs)), runErr
}

// ciphertext returns what the step printed between its output markers.
func ciphertext(t *testing.T, out string) string {
	t.Helper()
	begin := strings.Index(out, "----- BEGIN PROD OPERATION OUTPUT -----\n")
	end := strings.Index(out, "----- END PROD OPERATION OUTPUT -----\n")
	if begin < 0 || end < begin {
		t.Fatalf("no encrypted output block: %q", out)
	}
	return out[begin+len("----- BEGIN PROD OPERATION OUTPUT -----\n") : end]
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

// The public log carries the endpoint masks, the exit status and age
// ciphertext only: nothing the program printed, on stdout or stderr.
func TestCredentialStepPublishesOnlyEncryptedOutput(t *testing.T) {
	plaintextMarkers := []string{"token=", "argc=", "program stderr detail", "helper stdout must stay private", "synthetic-config", "synthetic-token"}
	for _, tc := range []struct {
		name        string
		programExit int
		keep, args  string
		wantPlain   string
	}{
		{"read without provider token", 0, "false", "", "token=unset\nargc=0\nprogram stderr detail\n"},
		{"provider read keeps token", 0, "true", "account-census", "token=synthetic-token\nargc=1 account-census\nprogram stderr detail\n"},
		{"program failure still encrypts and propagates", 3, "false", "", "token=unset\nargc=0\nprogram stderr detail\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ageArgs, runErr := runCredentialStep(t, 0, tc.programExit, tc.keep, tc.args, "", "age1recipient")
			if !strings.Contains(out, "::add-mask::192.0.2.1\n") {
				t.Fatalf("endpoint masks must still reach the runner: %q", out)
			}
			for _, marker := range plaintextMarkers {
				if strings.Contains(out, marker) {
					t.Fatalf("plaintext %q reached the public log: %q", marker, out)
				}
			}
			if exitCode(runErr) != tc.programExit || !strings.Contains(out, fmt.Sprintf("PROD_OPERATION_EXIT=%d\n", tc.programExit)) {
				t.Fatalf("exit status not propagated: %v %q", runErr, out)
			}
			if ageArgs != "--encrypt --armor --recipient age1recipient" {
				t.Fatalf("age called as %q", ageArgs)
			}
			block := strings.TrimSpace(ciphertext(t, out))
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(block, "FAKE-AGE:"))
			if err != nil || string(decoded) != tc.wantPlain {
				t.Fatalf("encrypted payload %q (err %v), want %q", decoded, err, tc.wantPlain)
			}
		})
	}
	t.Run("helper failure stops before the program", func(t *testing.T) {
		out, _, runErr := runCredentialStep(t, 1, 0, "false", "", "", "age1recipient")
		if exitCode(runErr) != 2 || !strings.Contains(out, "PROD_OPERATION=UNVERIFIED\n") || strings.Contains(out, "PROD_OPERATION_EXIT=") {
			t.Fatalf("failed helper did not stop with an unverified verdict: %v, %q", runErr, out)
		}
	})
	t.Run("encryption failure publishes nothing", func(t *testing.T) {
		t.Setenv("OP_TEST_AGE_FAIL", "1")
		out, _, runErr := runCredentialStep(t, 0, 0, "false", "", "", "age1recipient")
		if exitCode(runErr) != 2 || !strings.Contains(out, "PROD_OPERATION=UNVERIFIED\n") || strings.Contains(out, "BEGIN PROD OPERATION OUTPUT") {
			t.Fatalf("encryption failure did not stop unverified: %v, %q", runErr, out)
		}
		for _, marker := range plaintextMarkers {
			if strings.Contains(out, marker) {
				t.Fatalf("plaintext %q reached the public log: %q", marker, out)
			}
		}
	})
}

// With real age installed, the published block decrypts back to exactly what
// the program printed, and only with the matching identity.
func TestCredentialStepOutputDecryptsWithRealAge(t *testing.T) {
	ageBin, err := exec.LookPath("age")
	if err != nil {
		t.Skip("age not installed")
	}
	keygen, err := exec.LookPath("age-keygen")
	if err != nil {
		t.Skip("age-keygen not installed")
	}
	identity := filepath.Join(t.TempDir(), "identity.txt")
	if out, err := exec.Command(keygen, "-o", identity).CombinedOutput(); err != nil {
		t.Fatalf("age-keygen: %v %s", err, out)
	}
	pub, err := exec.Command(keygen, "-y", identity).Output()
	if err != nil {
		t.Fatal(err)
	}
	out, _, runErr := runCredentialStep(t, 0, 0, "false", "", ageBin, strings.TrimSpace(string(pub)))
	if runErr != nil {
		t.Fatalf("operation failed: %v %q", runErr, out)
	}
	decrypt := exec.Command(ageBin, "--decrypt", "--identity", identity)
	decrypt.Stdin = strings.NewReader(ciphertext(t, out))
	plain, err := decrypt.Output()
	if err != nil || string(plain) != "token=unset\nargc=0\nprogram stderr detail\n" {
		t.Fatalf("decrypted %q (err %v)", plain, err)
	}
	other := filepath.Join(t.TempDir(), "other.txt")
	if out, err := exec.Command(keygen, "-o", other).CombinedOutput(); err != nil {
		t.Fatalf("age-keygen: %v %s", err, out)
	}
	wrong := exec.Command(ageBin, "--decrypt", "--identity", other)
	wrong.Stdin = strings.NewReader(ciphertext(t, out))
	if _, err := wrong.Output(); err == nil {
		t.Fatal("another identity decrypted the output")
	}
}

// Each pinned download must pass exactly one URL to curl with the hardening
// flags, so the checksum and install never run on a missing download.
func TestKubectlDownloadCurlChild(_ *testing.T) {
	if os.Getenv("PROD_OPERATION_DOWNLOAD_CHILD") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	want := []string{"--disable", "--fail", "--silent", "--show-error", "--location", "--proto", "=https", "--proto-redir", "=https", "--max-redirs", "3", "--connect-timeout", "10", "--max-time", "60", "--retry", "3", "--retry-all-errors", "--retry-delay", "5", "--retry-max-time", "120", os.Getenv("WANT_URL"), "-o", os.Getenv("WANT_OUT")}
	if !reflect.DeepEqual(args, want) {
		os.Exit(31)
	}
	os.Exit(0)
}

func TestPinnedDownloadsPassOnlyOneURLToCurl(t *testing.T) {
	j, _ := operate(t)
	var statements []string
	for _, s := range j.Steps {
		lines := strings.Split(s.Run, "\n")
		for i := 0; i < len(lines); i++ {
			if !strings.HasPrefix(strings.TrimSpace(lines[i]), "curl ") {
				continue
			}
			statement := lines[i]
			for strings.HasSuffix(strings.TrimSpace(lines[i]), "\\") {
				i++
				if i >= len(lines) {
					t.Fatal("unfinished download")
				}
				statement += "\n" + lines[i]
			}
			statements = append(statements, statement)
		}
	}
	if len(statements) != 2 {
		t.Fatalf("want the kubectl and age downloads, found %d", len(statements))
	}
	bin := t.TempDir()
	testbin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(testbin, "'", "'\\''") + "' -test.run='^TestKubectlDownloadCurlChild$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ statement, prelude, url, out string }{
		{statements[0], "kubectl_path=\"${RUNNER_TEMP}/prod-operation-kubectl\"", "https://dl.k8s.io/release/v1.36.2/bin/linux/amd64/kubectl", bin + "/prod-operation-kubectl"},
		{statements[1], "age_archive=\"${RUNNER_TEMP}/prod-operation-age.tar.gz\"", "https://github.com/FiloSottile/age/releases/download/v1.3.2/age-v1.3.2-linux-amd64.tar.gz", bin + "/prod-operation-age.tar.gz"},
	} {
		command := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", "set -euo pipefail\n"+tc.prelude+"\n"+tc.statement+"\nprintf DOWNLOAD_ADMITTED")
		command.Env = []string{"PROD_OPERATION_DOWNLOAD_CHILD=1", "PATH=" + bin, "RUNNER_TEMP=" + bin, "KUBECTL_VERSION=v1.36.2", "AGE_VERSION=v1.3.2", "WANT_URL=" + tc.url, "WANT_OUT=" + tc.out}
		// Coverage runs make the instrumented child warn on stderr, so only stdout
		// is compared exactly; stderr is reported on failure.
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil || !bytes.Equal(stdout.Bytes(), []byte("DOWNLOAD_ADMITTED")) {
			t.Fatalf("download %s rejected: %v %q %q", tc.url, err, stdout.String(), stderr.String())
		}
	}
}

var operationName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Every caller is a thin dispatch wrapper: it passes no secret, grants no
// more than contents: read, and names a reviewed program that exists.
func TestCallersAreThinDispatchWrappers(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(workflowsDir, "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	callers := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "prod-operation.yaml") || filepath.Base(path) == "prod-operation.yaml" {
			continue
		}
		callers++
		name := filepath.Base(path)
		w, _ := load(t, path)
		dispatch, ok := w.On["workflow_dispatch"].(map[string]any)
		if len(w.On) != 1 || !ok || len(w.Permissions) != 0 || len(w.Env) != 0 || len(w.Jobs) != 1 {
			t.Fatalf("%s: caller must be dispatch-only with empty top-level grants and one job", name)
		}
		// The prod environment secrets reach the called job only when the caller
		// names them; a caller passes exactly these two through and expands no other.
		if strings.Count(string(raw), "secrets.") != 2 {
			t.Fatalf("%s: caller may expand only the two passed-through secrets", name)
		}
		// A workflow-level prod-deploy group would wait on the called job's own.
		if w.Concurrency != nil {
			t.Fatalf("%s: concurrency belongs to prod-operation.yaml", name)
		}
		inputs, _ := dispatch["inputs"].(map[string]any)
		for key := range inputs {
			if key != "confirm" && key != "operation" {
				t.Fatalf("%s: dispatch input %q could steer the operation", name, key)
			}
		}
		for _, j := range w.Jobs {
			if j.Uses != reusableUses || len(j.Steps) != 0 || !reflect.DeepEqual(j.Secrets, map[string]any{"KUBE_CONFIG": "${{ secrets.KUBE_CONFIG }}", "HCLOUD_TOKEN": "${{ secrets.HCLOUD_TOKEN }}"}) || j.Environment != "" {
				t.Fatalf("%s: job must only call %s passing only KUBE_CONFIG and HCLOUD_TOKEN", name, reusableUses)
			}
			if len(j.Permissions) != 1 || j.Permissions["contents"] != "read" {
				t.Fatalf("%s: caller grants more than contents: read", name)
			}
			if j.With["confirm"] != "${{ inputs.confirm }}" {
				t.Fatalf("%s: confirmation must come from the dispatcher", name)
			}
			program, _ := j.With["program"].(string)
			if !regexp.MustCompile(`^scripts/[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(program) {
				t.Fatalf("%s: program %q is not a reviewed scripts/ directory", name, program)
			}
			mains, _ := filepath.Glob(filepath.Join("../..", program, "*.go"))
			if len(mains) == 0 {
				t.Fatalf("%s: program %s has no Go source", name, program)
			}
			if args, ok := j.With["args"]; ok {
				s, _ := args.(string)
				if strings.Contains(s, "${{") && !regexp.MustCompile(`^\$\{\{ inputs\.operation == '[a-z0-9-]+' && '[a-z0-9 -]+' \|\| '[a-z0-9 -]+' \}\}$`).MatchString(s) {
					t.Fatalf("%s: args must be literal or chosen by the fixed operation, got %q", name, s)
				}
			}
			if h, ok := j.With["hcloud-token"]; ok {
				if _, literal := h.(bool); !literal {
					t.Fatalf("%s: hcloud-token must be a literal boolean", name)
				}
			}
			operation, _ := j.With["operation"].(string)
			var names []string
			if operation == "${{ inputs.operation }}" {
				inputs, _ := dispatch["inputs"].(map[string]any)
				op, _ := inputs["operation"].(map[string]any)
				if op["type"] != "choice" {
					t.Fatalf("%s: a dispatched operation name must be a fixed choice", name)
				}
				for _, o := range op["options"].([]any) {
					names = append(names, o.(string))
				}
			} else {
				names = []string{operation}
			}
			for _, n := range names {
				if !operationName.MatchString(n) {
					t.Fatalf("%s: operation %q is not a plain name", name, n)
				}
			}
		}
	}
	if callers < 3 {
		t.Fatalf("expected at least 3 callers, found %d", callers)
	}
}
