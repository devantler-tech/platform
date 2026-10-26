package emailcredentials_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

const syntheticToken = "test-only-root-token"

type apiFixture struct {
	mu          sync.Mutex
	keys        map[string]map[string]any
	writes      int
	denied      string
	missingKV   bool
	invalidKV   bool
	race        string
	changeAfter bool
	loseReply   bool
}

func credentials() map[string]map[string]any {
	keys := map[string]map[string]any{}
	for _, account := range []string{"admin", "maintainer", "agent"} {
		keys["apps/private-mail/"+account] = map[string]any{
			"password": strings.Repeat("A", 48) + account,
			"recovery": map[string]any{"preserve": true},
		}
	}
	return keys
}

func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("X-Vault-Token") != syntheticToken {
		http.Error(w, `{"errors":["authentication refused"]}`, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/sys/mounts" {
		mounts := map[string]any{}
		if !f.missingKV {
			version := "2"
			if f.invalidKV {
				version = "1"
			}
			mounts["secret/"] = map[string]any{"type": "kv", "options": map[string]string{"version": version}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": mounts})
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/v1/secret/data/")
	if !ok || !allowedKey(key) {
		http.Error(w, `{"errors":["unexpected path"]}`, http.StatusBadRequest)
		return
	}
	if key == f.denied {
		http.Error(w, `{"errors":["read refused"]}`, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		data, exists := f.keys[key]
		if !exists {
			http.Error(w, `{"errors":[]}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"data": data, "metadata": map[string]any{"version": 1},
		}})
		return
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(key, "infrastructure/email/") {
		http.Error(w, `{"errors":["unexpected mutation"]}`, http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Options struct {
			CAS *int `json:"cas"`
		} `json:"options"`
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Options.CAS == nil || *request.Options.CAS != 0 {
		http.Error(w, `{"errors":["creation must be conditional"]}`, http.StatusBadRequest)
		return
	}
	if f.race != "" {
		data := request.Data
		if f.race == "different" {
			data = map[string]any{"password": strings.Repeat("Z", 48)}
		}
		f.keys[key] = data
		f.race = ""
		http.Error(w, `{"errors":["check-and-set conflict"]}`, http.StatusBadRequest)
		return
	}
	if _, exists := f.keys[key]; exists {
		http.Error(w, `{"errors":["check-and-set conflict"]}`, http.StatusBadRequest)
		return
	}
	f.keys[key] = request.Data
	f.writes++
	if f.loseReply {
		f.loseReply = false
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
		return
	}
	if f.changeAfter {
		f.keys["apps/private-mail/admin"] = map[string]any{"password": strings.Repeat("B", 48)}
		f.changeAfter = false
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": 1}})
}

func allowedKey(key string) bool {
	for _, prefix := range []string{"apps/private-mail/", "infrastructure/email/"} {
		for _, account := range []string{"admin", "maintainer", "agent"} {
			if key == prefix+account {
				return true
			}
		}
	}
	return false
}

func repositoryRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func runMigration(t *testing.T, fixture *apiFixture, mode string) (string, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	return runMigrationAt(t, server.URL, mode)
}

func runMigrationAt(t *testing.T, serverURL, mode string) (string, error) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-config/config-map-email-credentials.yaml"))
	if err != nil {
		t.Fatal("missing declarative Email credential preparation", err)
	}
	var config struct{ Data map[string]string }
	if err := yaml.Unmarshal(manifest, &config); err != nil {
		t.Fatal(err)
	}
	script := config.Data["prepare-email-credentials.sh"]
	if script == "" {
		t.Fatal("missing executable credential preparation")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"prepare.sh": script, "token": syntheticToken} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	environment := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LC_ALL=C",
		"BAO_ADDR=" + serverURL, "EMAIL_CUSTODY_MODE=" + mode,
		"EMAIL_CUSTODY_TOKEN_FILE=" + filepath.Join(dir, "token"),
		"EMAIL_CUSTODY_WORK_DIR=" + dir,
	}
	command := exec.Command("/bin/sh", "-eu", filepath.Join(dir, "prepare.sh"))
	if os.Getenv("EMAIL_CREDENTIAL_CONTAINER_TEST") == "1" {
		// Linux CI runs the identical fixture through the exact deployed image.
		// Bind only synthetic private inputs, and grant no container capabilities.
		if runtime.GOOS != "linux" {
			t.Fatal("native container proof requires the Linux CI host network")
		}
		args := []string{
			"run", "--rm", "--network=host", "--read-only", "--cap-drop=ALL",
			"--security-opt=no-new-privileges", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--mount", "type=bind,source=" + dir + ",target=" + dir,
			"--entrypoint", "/bin/sh",
		}
		for _, value := range environment[1:] {
			args = append(args, "--env", value)
		}
		args = append(args, deployedToolingImage(t), "-eu", filepath.Join(dir, "prepare.sh"))
		command = exec.Command("docker", args...)
	}
	command.Env = environment
	out, runErr := command.CombinedOutput()
	if strings.Contains(string(out), syntheticToken) || strings.Contains(string(out), strings.Repeat("A", 48)) || strings.Contains(string(out), serverURL) {
		t.Fatal("credential preparation emitted private material")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal("credential scratch material survived the operation")
	}
	return string(out), runErr
}

func deployedToolingImage(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-config/job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					InitContainers []struct{ Name, Image string } `yaml:"initContainers"`
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == "prepare-email-credentials" && strings.HasPrefix(c.Image, "docker.io/alpine/k8s:") && strings.Contains(c.Image, "@sha256:") {
			return c.Image
		}
	}
	t.Fatal("native proof must use the exact declared tooling image")
	return ""
}

func TestPlanIsReadOnlyAndCopyPreservesEveryCurrentField(t *testing.T) {
	fixture := &apiFixture{keys: credentials()}
	out, err := runMigration(t, fixture, "plan")
	if err != nil || !strings.Contains(out, "=PLANNED mode=plan existing=0 copied=0 pending=3 unseeded=0") || fixture.writes != 0 {
		t.Fatalf("plan must report three pending copies without writing: %v %s", err, out)
	}
	for attempt := 0; attempt < 2; attempt++ {
		out, err = runMigration(t, fixture, "copy")
		if err != nil || fixture.writes != 3 {
			t.Fatalf("copy and replay must create exactly three records: %v %s", err, out)
		}
		for _, account := range []string{"admin", "maintainer", "agent"} {
			if !reflect.DeepEqual(fixture.keys["apps/private-mail/"+account], fixture.keys["infrastructure/email/"+account]) {
				t.Fatal("copy changed the current credential or discarded another field")
			}
		}
	}
	if !strings.Contains(out, "=VERIFIED mode=copy existing=3 copied=0 pending=0 unseeded=0") {
		t.Fatal("replay must distinguish existing verified values from new copies")
	}
}

func TestMigrationRefusesConflictsAndUnreadableState(t *testing.T) {
	for _, name := range []string{"destination-conflict", "read-refused", "invalid-password", "numeric-field", "invalid-mount", "different-race", "source-rotation", "invalid-mode"} {
		t.Run(name, func(t *testing.T) {
			fixture := &apiFixture{keys: credentials()}
			mode := "copy"
			switch name {
			case "destination-conflict":
				fixture.keys["infrastructure/email/admin"] = map[string]any{"password": strings.Repeat("Z", 48)}
			case "read-refused":
				fixture.denied = "apps/private-mail/admin"
			case "invalid-password":
				fixture.keys["apps/private-mail/admin"]["password"] = "invalid"
			case "numeric-field":
				fixture.keys["apps/private-mail/admin"]["extra"] = json.Number("9007199254740993")
			case "invalid-mount":
				fixture.invalidKV = true
			case "different-race":
				fixture.race = "different"
			case "source-rotation":
				fixture.changeAfter = true
			case "invalid-mode":
				mode = "replace"
			}
			out, err := runMigration(t, fixture, mode)
			if err == nil || strings.Contains(out, "=PASS") {
				t.Fatalf("uncertain state must fail closed: %s", out)
			}
			if name != "source-rotation" && fixture.writes != 0 {
				t.Fatal("refusal wrote a credential")
			}
		})
	}
}

func TestConcurrentIdenticalCreationIsVerifiedWithoutOverwrite(t *testing.T) {
	fixture := &apiFixture{keys: credentials(), race: "same"}
	out, err := runMigration(t, fixture, "copy")
	if err != nil || fixture.writes != 2 || !strings.Contains(out, "existing=1 copied=2") {
		t.Fatalf("identical conditional-create race must be read back and compared: %v %s", err, out)
	}
}

func TestLostWriteReplyIsRecoveredByReadbackWithoutRotation(t *testing.T) {
	fixture := &apiFixture{keys: credentials(), loseReply: true}
	if _, err := runMigration(t, fixture, "copy"); err == nil || fixture.writes != 1 {
		t.Fatal("a committed write with a lost reply must remain an uncertain result")
	}
	out, err := runMigration(t, fixture, "copy")
	if err != nil || fixture.writes != 3 || !strings.Contains(out, "=VERIFIED mode=copy existing=1 copied=2") {
		t.Fatalf("retry must verify the committed value and copy only missing records: %v %s", err, out)
	}
	for _, account := range []string{"admin", "maintainer", "agent"} {
		if !reflect.DeepEqual(fixture.keys["apps/private-mail/"+account], fixture.keys["infrastructure/email/"+account]) {
			t.Fatal("uncertain-write recovery changed or erased a credential")
		}
	}
}

func TestUnseededInstallationIsExplicitAndNeverCreatesCredentials(t *testing.T) {
	for _, missingMount := range []bool{false, true} {
		t.Run(fmt.Sprint(missingMount), func(t *testing.T) {
			fixture := &apiFixture{keys: map[string]map[string]any{}, missingKV: missingMount}
			out, err := runMigration(t, fixture, "plan")
			if err != nil || fixture.writes != 0 || !strings.Contains(out, "=UNSEEDED") || !strings.Contains(out, "unseeded=3") {
				t.Fatalf("fresh installation must report unseeded state: %v %s", err, out)
			}
		})
	}
}

func TestDeclaredPreparationRunsReadOnlyWithPrivateMemoryScratch(t *testing.T) {
	out, err := exec.Command("kubectl", "kustomize", filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-config")).CombinedOutput()
	if err != nil {
		t.Fatalf("configuration bundle must render: %v", err)
	}
	type volume struct {
		Name     string
		EmptyDir *struct {
			Medium    string
			SizeLimit string `yaml:"sizeLimit"`
		} `yaml:"emptyDir"`
	}
	type container struct {
		Name, Image  string
		Command      []string
		Env          []struct{ Name, Value string }
		VolumeMounts []struct {
			Name      string
			MountPath string `yaml:"mountPath"`
			ReadOnly  bool   `yaml:"readOnly"`
		} `yaml:"volumeMounts"`
		SecurityContext struct {
			ReadOnlyRootFilesystem   bool  `yaml:"readOnlyRootFilesystem"`
			RunAsNonRoot             bool  `yaml:"runAsNonRoot"`
			AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		} `yaml:"securityContext"`
	}
	var foundConfig, foundContainer bool
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var object struct {
			Kind     string
			Metadata struct{ Name string }
			Spec     struct {
				Template struct {
					Spec struct {
						InitContainers []container `yaml:"initContainers"`
						Volumes        []volume
					}
				}
			}
		}
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if object.Kind == "ConfigMap" && object.Metadata.Name == "vault-email-credentials" {
			foundConfig = true
		}
		if object.Kind != "Job" || object.Metadata.Name != "vault-config" {
			continue
		}
		for _, c := range object.Spec.Template.Spec.InitContainers {
			if c.Name != "prepare-email-credentials" {
				continue
			}
			foundContainer = true
			if c.Image != deployedToolingImage(t) || !reflect.DeepEqual(c.Command, []string{"/bin/sh", "-eu", "/vault-config/email/prepare-email-credentials.sh"}) {
				t.Fatal("preparation must execute the declared script with the existing pinned tooling image")
			}
			env := map[string]string{}
			for _, v := range c.Env {
				env[v.Name] = v.Value
			}
			if len(env) != 4 || env["EMAIL_CUSTODY_MODE"] != "plan" || env["EMAIL_CUSTODY_TOKEN_FILE"] != "/shared/root-token" || env["EMAIL_CUSTODY_WORK_DIR"] != "/work" {
				t.Fatal("deployed preparation must only plan and reuse the existing private credential file")
			}
			if !c.SecurityContext.ReadOnlyRootFilesystem || !c.SecurityContext.RunAsNonRoot || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
				t.Fatal("preparation must preserve the restricted container boundary")
			}
			mounts := map[string]bool{}
			for _, m := range c.VolumeMounts {
				mounts[m.Name+":"+m.MountPath] = m.ReadOnly
			}
			if len(mounts) != 3 || !mounts["shared:/shared"] || !mounts["email-credentials:/vault-config/email"] {
				t.Fatal("credential and script mounts must remain read only")
			}
			foundScratch := false
			for _, v := range object.Spec.Template.Spec.Volumes {
				if v.Name == "email-credential-work" && v.EmptyDir != nil && v.EmptyDir.Medium == "Memory" && v.EmptyDir.SizeLimit == "16Mi" {
					if _, ok := mounts["email-credential-work:/work"]; ok {
						foundScratch = true
					}
				}
			}
			if !foundScratch {
				t.Fatal("credential scratch must use bounded memory-backed storage")
			}
		}
	}
	if !foundConfig || !foundContainer {
		t.Fatal("declarative Email preparation is not wired into the configuration Job")
	}
}
