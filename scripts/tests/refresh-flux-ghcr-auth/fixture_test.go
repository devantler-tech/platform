package refreshfluxghcrauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var (
	rootPath             = findRepoRoot()
	helperPath           = filepath.Join(rootPath, "scripts", "refresh-flux-ghcr-auth.sh")
	ksailPullWrapperPath = filepath.Join(rootPath, "scripts", "run-ksail-prod-with-pull-auth.sh")
	ksailOperatorVersion = readKSailOperatorVersion()
	ksailTargetImage     = "ghcr.io/devantler-tech/ksail:v" + ksailOperatorVersion
)

type commandResult struct {
	exitCode int
	stdout   string
	stderr   string
}

type fixture struct {
	t                            *testing.T
	workspace                    string
	binDir                       string
	decryptedConfig              string
	encryptedSecret              string
	patchCapture                 string
	variablesPatchCapture        string
	kubectlCalled                string
	outputPathLog                string
	registryReadLog              string
	fanoutLog                    string
	talosLog                     string
	talosPatchPathLog            string
	operationLog                 string
	ksailTokenCapture            string
	ksailUsernameCapture         string
	ksailRevisionCapture         string
	ksailCommandCapture          string
	ksailConfigPathCapture       string
	ksailRegistryCapture         string
	ksailRegistryOverrideCapture string
	syncStateDir                 string
	encryptedCiphertext          string
}

func TestMain(m *testing.M) {
	var code int
	switch filepath.Base(os.Args[0]) {
	case "ksail":
		code = fakeKSail(os.Args[1:])
	case "talosctl":
		code = fakeTalosctl(os.Args[1:])
	case "curl":
		code = fakeCurl(os.Args[1:])
	case "kubectl":
		code = fakeKubectl(os.Args[1:])
	default:
		code = m.Run()
	}
	os.Exit(code)
}

func findRepoRoot() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot locate fixture source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
}

func readKSailOperatorVersion() string {
	path := filepath.Join(rootPath, "k8s", "bases", "infrastructure", "controllers", "ksail-operator", "helm-release.yaml")
	for _, line := range strings.Split(mustReadFile(path), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "version:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "version:"))
		}
	}
	panic("KSail operator chart version not found")
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	workspace := t.TempDir()
	f := &fixture{
		t:                            t,
		workspace:                    workspace,
		binDir:                       filepath.Join(workspace, "bin"),
		decryptedConfig:              filepath.Join(workspace, "decrypted-config.json"),
		encryptedSecret:              filepath.Join(workspace, "secret.enc.yaml"),
		patchCapture:                 filepath.Join(workspace, "patch.json"),
		variablesPatchCapture:        filepath.Join(workspace, "variables-patch.json"),
		kubectlCalled:                filepath.Join(workspace, "kubectl-called"),
		outputPathLog:                filepath.Join(workspace, "ksail-output-path"),
		registryReadLog:              filepath.Join(workspace, "registry-reads"),
		fanoutLog:                    filepath.Join(workspace, "fanout-log"),
		talosLog:                     filepath.Join(workspace, "talos-log"),
		talosPatchPathLog:            filepath.Join(workspace, "talos-patch-path"),
		operationLog:                 filepath.Join(workspace, "operation-log"),
		ksailTokenCapture:            filepath.Join(workspace, "ksail-token"),
		ksailUsernameCapture:         filepath.Join(workspace, "ksail-username"),
		ksailRevisionCapture:         filepath.Join(workspace, "ksail-revision"),
		ksailCommandCapture:          filepath.Join(workspace, "ksail-command"),
		ksailConfigPathCapture:       filepath.Join(workspace, "ksail-config-path"),
		ksailRegistryCapture:         filepath.Join(workspace, "ksail-registry"),
		ksailRegistryOverrideCapture: filepath.Join(workspace, "ksail-registry-override"),
		syncStateDir:                 filepath.Join(workspace, "sync-state"),
	}
	mustMkdir(f.binDir)
	mustMkdir(f.syncStateDir)
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	for _, name := range []string{"ksail", "talosctl", "curl", "kubectl"} {
		if err := os.Symlink(testExecutable, filepath.Join(f.binDir, name)); err != nil {
			t.Fatalf("link fake %s: %v", name, err)
		}
	}
	f.writeEncryptedSecret("ENC[AES256_GCM,data:fixture-one]")
	return f
}

func (f *fixture) writeEncryptedSecret(ciphertext string) {
	f.t.Helper()
	f.encryptedCiphertext = ciphertext
	mustWriteJSON(f.t, f.encryptedSecret, map[string]any{
		"stringData": map[string]any{"ghcr_dockerconfigjson": ciphertext},
	})
}

func (f *fixture) expectedRevision() string {
	digest := sha256.Sum256([]byte(f.encryptedCiphertext + "\n"))
	return hex.EncodeToString(digest[:])
}

func expectedCredentials(config any) (string, string) {
	root, ok := config.(map[string]any)
	if !ok {
		return "unused", "unused"
	}
	auths, ok := root["auths"].(map[string]any)
	if !ok {
		return "unused", "unused"
	}
	registry, ok := auths["ghcr.io"].(map[string]any)
	if !ok {
		return "unused", "unused"
	}
	username, usernameOK := registry["username"].(string)
	password, passwordOK := registry["password"].(string)
	if usernameOK && passwordOK && username != "" && password != "" {
		return username, password
	}
	encoded, ok := registry["auth"].(string)
	if !ok {
		return "unused", "unused"
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "unused", "unused"
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "unused", "unused"
	}
	return parts[0], parts[1]
}

func validConfig() map[string]any {
	return map[string]any{
		"auths": map[string]any{
			"ghcr.io": map[string]any{
				"username": "devantler",
				"password": "fixture-secret-token",
			},
		},
	}
}

func (f *fixture) runHelper(config any, helperArgs []string, overrides map[string]string) commandResult {
	return f.runHelperWithClusterState(config, helperArgs, overrides, false)
}

func (f *fixture) runHelperPreservingClusterState(
	config any,
	helperArgs []string,
	overrides map[string]string,
) commandResult {
	return f.runHelperWithClusterState(config, helperArgs, overrides, true)
}

func (f *fixture) runHelperWithClusterState(
	config any,
	helperArgs []string,
	overrides map[string]string,
	preserveClusterState bool,
) commandResult {
	f.t.Helper()
	mustWriteJSON(f.t, f.decryptedConfig, config)
	f.clearRunStatePreservingCluster(true, preserveClusterState)
	username, token := expectedCredentials(config)
	env := f.baseEnvironment()
	env["EXPECTED_PULL_USERNAME"] = username
	env["EXPECTED_PULL_TOKEN"] = token
	env["EXPECTED_GHCR_REVISION"] = f.expectedRevision()
	env["EXPECTED_KSAIL_TARGET_IMAGE"] = ksailTargetImage
	env["FLUX_GHCR_SYNC_ATTEMPTS"] = "2"
	env["FLUX_GHCR_SYNC_INTERVAL"] = "0"
	env["FLUX_GHCR_TALOS_CONVERGENCE_ATTEMPTS"] = "6"
	for key, value := range overrides {
		env[key] = value
	}
	return runCaptured(f.t, rootPath, env, helperPath, helperArgs...)
}

func (f *fixture) runKSailPullWrapper(config any, command []string, overrides map[string]string) commandResult {
	f.t.Helper()
	mustWriteJSON(f.t, f.decryptedConfig, config)
	f.clearRunState(false)
	username, token := expectedCredentials(config)
	env := f.baseEnvironment()
	env["EXPECTED_PULL_USERNAME"] = username
	env["EXPECTED_PULL_TOKEN"] = token
	env["EXPECTED_GHCR_REVISION"] = f.expectedRevision()
	for key, value := range overrides {
		env[key] = value
	}
	return runCaptured(f.t, rootPath, env, ksailPullWrapperPath, command...)
}

func (f *fixture) baseEnvironment() map[string]string {
	env := environmentMap(os.Environ())
	env["PATH"] = f.binDir + string(os.PathListSeparator) + env["PATH"]
	env["FAKE_DECRYPTED_CONFIG"] = f.decryptedConfig
	env["FLUX_GHCR_SECRET_FILE"] = f.encryptedSecret
	env["PATCH_CAPTURE"] = f.patchCapture
	env["VARIABLES_PATCH_CAPTURE"] = f.variablesPatchCapture
	env["KUBECTL_CALLED"] = f.kubectlCalled
	env["KSAIL_OUTPUT_PATH_LOG"] = f.outputPathLog
	env["REGISTRY_READ_LOG"] = f.registryReadLog
	env["FANOUT_LOG"] = f.fanoutLog
	env["TALOS_LOG"] = f.talosLog
	env["TALOS_PATCH_PATH_LOG"] = f.talosPatchPathLog
	env["OPERATION_LOG"] = f.operationLog
	env["KSAIL_TOKEN_CAPTURE"] = f.ksailTokenCapture
	env["KSAIL_USERNAME_CAPTURE"] = f.ksailUsernameCapture
	env["KSAIL_REVISION_CAPTURE"] = f.ksailRevisionCapture
	env["KSAIL_COMMAND_CAPTURE"] = f.ksailCommandCapture
	env["KSAIL_CONFIG_PATH_CAPTURE"] = f.ksailConfigPathCapture
	env["KSAIL_REGISTRY_CAPTURE"] = f.ksailRegistryCapture
	env["KSAIL_REGISTRY_OVERRIDE_CAPTURE"] = f.ksailRegistryOverrideCapture
	env["FAKE_SYNC_STATE_DIR"] = f.syncStateDir
	return env
}

func (f *fixture) clearRunState(helper bool) {
	f.clearRunStatePreservingCluster(helper, false)
}

func (f *fixture) clearRunStatePreservingCluster(helper, preserveClusterState bool) {
	f.t.Helper()
	paths := []string{
		f.outputPathLog,
		f.ksailTokenCapture,
		f.ksailUsernameCapture,
		f.ksailRevisionCapture,
		f.ksailCommandCapture,
		f.ksailConfigPathCapture,
		f.ksailRegistryCapture,
		f.ksailRegistryOverrideCapture,
	}
	if helper {
		paths = append(paths,
			f.patchCapture,
			f.variablesPatchCapture,
			f.kubectlCalled,
			f.registryReadLog,
			f.fanoutLog,
			f.talosLog,
			f.talosPatchPathLog,
			f.operationLog,
		)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			f.t.Fatalf("clear %s: %v", path, err)
		}
	}
	if helper && !preserveClusterState {
		entries, err := os.ReadDir(f.syncStateDir)
		if err != nil {
			f.t.Fatalf("read sync state: %v", err)
		}
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(f.syncStateDir, entry.Name())); err != nil {
				f.t.Fatalf("clear sync marker %s: %v", entry.Name(), err)
			}
		}
	}
}

func runCaptured(t *testing.T, cwd string, environment map[string]string, executable string, args ...string) commandResult {
	t.Helper()
	cmd := exec.Command(executable, args...)
	cmd.Dir = cwd
	cmd.Env = environmentSlice(environment)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run %s: %v", executable, err)
		}
		exitCode = exitError.ExitCode()
	}
	return commandResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

func environmentMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, item, ok := strings.Cut(value, "=")
		if ok {
			result[key] = item
		}
	}
	return result
}

func environmentSlice(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func mustWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustRead(path string) string {
	return mustReadFile(path)
}

func mustReadFile(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("read %s: %v", path, err))
	}
	return string(content)
}

func readLines(path string) []string {
	content := strings.TrimSuffix(mustRead(path), "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mustMkdir(path string) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		panic(fmt.Sprintf("mkdir %s: %v", path, err))
	}
}
