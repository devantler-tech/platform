package recovery_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// readbackSourceRoot locates the checkout or an explicitly selected test fixture.
func readbackSourceRoot() string {
	if root := os.Getenv("BACKUP_SOURCE_ROOT"); root != "" {
		return root
	}
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

// mirrorProgram reads the exact program embedded in each snapshot workload.
func mirrorProgram(t *testing.T, filename string) string {
	t.Helper()
	query := `.spec.template.spec.containers[] | select(.name == "mirror") | .command[-1]`
	if filename == "cron-job.yaml" {
		query = `.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "mirror") | .command[-1]`
	}
	out, err := exec.Command("yq", "-o=json", query, filepath.Join(readbackSourceRoot(), "k8s/bases/infrastructure/vault-backup", filename)).Output()
	if err != nil {
		t.Fatal("cannot read the actual declared mirror program", err)
	}
	var script string
	if err := json.Unmarshal(out, &script); err != nil || script == "" {
		t.Fatal("missing declared mirror program")
	}
	return script
}

// exerciseMirror exercises the declared program with synthetic storage responses.
func exerciseMirror(t *testing.T, filename, reply string, missingCredentials, empty, uploadFailure bool) (string, []string, error) {
	t.Helper()
	return exerciseMirrorMode(t, filename, reply, missingCredentials, empty, uploadFailure, false)
}

// exerciseMirrorMode also supports the real installer and mirror image without networking.
func exerciseMirrorMode(t *testing.T, filename, reply string, missingCredentials, empty, uploadFailure, native bool) (string, []string, error) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"bin", "r2", "snapshots", "tools", "config"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := filepath.Join(dir, "snapshots/openbao-20260101-000000.snap")
	if !empty {
		if err := os.WriteFile(snapshot, []byte("synthetic-snapshot-bytes\x00preserve-verbatim\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if reply == "matching-two" || reply == "missing-second" {
		if err := os.WriteFile(filepath.Join(dir, "snapshots/openbao-20260101-000001.snap"), []byte("different second snapshot\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !missingCredentials {
		for _, name := range []string{"access_key_id", "secret_access_key"} {
			if err := os.WriteFile(filepath.Join(dir, "r2", name), []byte("synthetic-credential"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	shim := `#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$FIXTURE_TRACE"
case "$1" in
  alias|ls) exit 0;;
  rm) [ "$FIXTURE_REPLY" != retention-failed ];;
  mirror) [ "$FIXTURE_UPLOAD_FAILURE" != true ];;
  cat)
    name=$(basename "$2")
    [ "$2" = "backup/fixture-bucket/openbao-snapshots/$name" ] || exit 95
    case "$FIXTURE_REPLY" in
      matching|matching-two|retention-failed) cat "$FIXTURE_SOURCE_DIR/$name";;
      missing-second) [ "$name" != openbao-20260101-000001.snap ] || exit 1; cat "$FIXTURE_SOURCE_DIR/$name";;
      corrupt) printf 'wrong bytes';;
      failed) exit 1;;
      full-then-error) cat "$FIXTURE_SNAPSHOT"; exit 1;;
      *) exit 93;;
    esac;;
  *) exit 94;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "bin/mc"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	// Use a real native SHA-256 implementation, including on macOS. The
	// production static tooling is separately exercised by Linux CI.
	hasher, err := exec.LookPath("sha256sum")
	if !native && err == nil {
		if err := os.Symlink(hasher, filepath.Join(dir, "tools/sha256sum")); err != nil {
			t.Fatal(err)
		}
	} else if !native {
		if err := os.WriteFile(filepath.Join(dir, "tools/sha256sum"), []byte("#!/bin/bash\nexec shasum -a 256 \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"basename", "cmp"} {
		if native {
			continue
		}
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, "tools", name)); err != nil {
			t.Fatal(err)
		}
	}
	script := mirrorProgram(t, filename)
	for original, replacement := range map[string]string{
		"/snapshots/":    filepath.Join(dir, "snapshots") + "/",
		"/r2/":           filepath.Join(dir, "r2") + "/",
		"/tools/":        filepath.Join(dir, "tools") + "/",
		"${r2_endpoint}": "https://fixture.invalid",
		"${r2_bucket}":   "fixture-bucket",
	} {
		script = strings.ReplaceAll(script, original, replacement)
	}
	trace := filepath.Join(dir, "trace")
	environment := []string{
		"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + dir,
		"MC_CONFIG_DIR=" + filepath.Join(dir, "config"), "REQUIRE_REMOTE_SNAPSHOT=true",
		"FIXTURE_TRACE=" + trace, "FIXTURE_REPLY=" + reply, "FIXTURE_SNAPSHOT=" + snapshot,
		"FIXTURE_SOURCE_DIR=" + filepath.Join(dir, "snapshots"),
		"FIXTURE_UPLOAD_FAILURE=" + map[bool]string{true: "true", false: "false"}[uploadFailure],
	}
	command := exec.Command("bash", "-eu", "-c", script)
	if native {
		if runtime.GOOS != "linux" || os.Getuid() == 0 {
			t.Fatal("native tooling proof requires the nonroot Linux CI host")
		}
		query := `.spec.template.spec`
		if filename == "cron-job.yaml" {
			query = `.spec.jobTemplate.spec.template.spec`
		}
		query += ` | {"installer": (.initContainers[] | select(.name == "install-mirror-tools")), "mirror": (.containers[] | select(.name == "mirror"))}`
		data, err := exec.Command("yq", "-o=json", query, filepath.Join(readbackSourceRoot(), "k8s/bases/infrastructure/vault-backup", filename)).Output()
		var spec struct {
			Installer, Mirror struct {
				Image   string
				Command []string
			}
		}
		if err != nil || json.Unmarshal(data, &spec) != nil || !strings.HasPrefix(spec.Installer.Image, "docker.io/library/busybox:") || !strings.Contains(spec.Installer.Image, "@sha256:") || !strings.HasPrefix(spec.Mirror.Image, "quay.io/minio/aistor/mc:") || !strings.Contains(spec.Mirror.Image, "@sha256:") || len(spec.Installer.Command) != 3 || len(spec.Mirror.Command) != 3 || spec.Mirror.Command[0] != "/tools/sh" {
			t.Fatal("native proof must use the declared pinned installer and actual mirror image")
		}
		// Execute the real installation, then use the copied bytes in the real
		// mirror image. Running only the BusyBox source image would not detect a
		// dynamic-linker incompatibility in the destination image.
		installArgs := []string{"run", "--rm", "--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--memory=64m", "--pids-limit=64", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--mount", "type=bind,source=" + filepath.Join(dir, "tools") + ",target=/tools", "--entrypoint", spec.Installer.Command[0], spec.Installer.Image}
		installArgs = append(installArgs, spec.Installer.Command[1:]...)
		install := exec.Command("docker", installArgs...)
		install.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
		if out, err := install.CombinedOutput(); err != nil {
			t.Fatalf("pinned static-tool installation failed: %v %s", err, out)
		}
		args := []string{"run", "--rm", "--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--memory=64m", "--cpus=0.5", "--pids-limit=64", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--mount", "type=bind,source=" + dir + ",target=" + dir, "--mount", "type=bind,source=" + filepath.Join(dir, "tools") + ",target=/tools,readonly", "--entrypoint", spec.Mirror.Command[0]}
		for _, value := range environment {
			if strings.HasPrefix(value, "PATH=") {
				value = "PATH=" + filepath.Join(dir, "bin") + ":/tools:/bin:/usr/bin"
			}
			args = append(args, "--env", value)
		}
		args = append(args, spec.Mirror.Image, spec.Mirror.Command[1], script)
		command = exec.Command("docker", args...)
	}
	command.Env = environment
	out, runErr := command.CombinedOutput()
	if strings.Contains(string(out), "synthetic-credential") || strings.Contains(string(out), "synthetic-snapshot-bytes") {
		t.Fatal("mirror exposed private inputs")
	}
	verbs, _ := os.ReadFile(trace)
	return string(out), strings.Fields(string(verbs)), runErr
}

// TestPinnedSnapshotReadbackTooling proves cross-image tool compatibility and pipefail.
func TestPinnedSnapshotReadbackTooling(t *testing.T) {
	if os.Getenv("VAULT_BACKUP_TOOLING_NATIVE_TEST") != "1" {
		t.Skip("pinned static BusyBox execution runs in Linux CI")
	}
	for _, filename := range []string{"job.yaml", "cron-job.yaml"} {
		for _, reply := range []string{"matching", "corrupt", "full-then-error"} {
			t.Run(filename+"/"+reply, func(t *testing.T) {
				out, _, err := exerciseMirrorMode(t, filename, reply, false, false, false, true)
				if reply == "matching" {
					if err != nil || !strings.Contains(out, "VAULT_SNAPSHOT_REMOTE=VERIFIED count=1") {
						t.Fatalf("pinned tooling did not verify actual matching bytes: %v %s", err, out)
					}
				} else {
					stage := "integrity"
					if reply == "full-then-error" {
						stage = "readback"
					}
					if err == nil || !strings.Contains(out, "VAULT_SNAPSHOT_REMOTE=FAIL stage="+stage) || strings.Contains(out, "VAULT_SNAPSHOT_REMOTE=VERIFIED") {
						t.Fatalf("pinned shell did not reject the intended failure: %v %s", err, out)
					}
				}
			})
		}
	}
}

// TestRemoteReadbackRequiresDownloadedMatchingBytes rejects metadata-only success.
func TestRemoteReadbackRequiresDownloadedMatchingBytes(t *testing.T) {
	for _, filename := range []string{"job.yaml", "cron-job.yaml"} {
		t.Run(filename, func(t *testing.T) {
			out, verbs, err := exerciseMirror(t, filename, "matching", false, false, false)
			if err != nil || !strings.Contains(out, "VAULT_SNAPSHOT_REMOTE=VERIFIED count=1") || !strings.Contains(strings.Join(verbs, " "), "mirror cat") {
				t.Fatalf("backup needs actual matching GET readback: %v; verbs=%v; %s", err, verbs, out)
			}
		})
		t.Run(filename+"/all snapshots", func(t *testing.T) {
			out, verbs, err := exerciseMirror(t, filename, "matching-two", false, false, false)
			if err != nil || !strings.Contains(out, "VAULT_SNAPSHOT_REMOTE=VERIFIED count=2") || strings.Count(strings.Join(verbs, " "), "cat") != 2 {
				t.Fatalf("every different snapshot needs its own GET verification: %v %v %s", err, verbs, out)
			}
		})
	}
}

// TestRemoteReadbackRejectsUnprovenProductionBackups binds each refusal to its actual stage.
func TestRemoteReadbackRejectsUnprovenProductionBackups(t *testing.T) {
	for _, filename := range []string{"job.yaml", "cron-job.yaml"} {
		for _, test := range []struct {
			name, reply, stage           string
			missing, empty, failedUpload bool
		}{
			{"different bytes", "corrupt", "integrity", false, false, false},
			{"failed download", "failed", "readback", false, false, false},
			{"full body then failure", "full-then-error", "readback", false, false, false},
			{"missing credentials", "matching", "credentials", true, false, false},
			{"empty source", "matching", "source", false, true, false},
			{"failed upload", "matching", "upload", false, false, true},
			{"missing second copy", "missing-second", "readback", false, false, false},
			{"failed retention", "retention-failed", "retention", false, false, false},
		} {
			t.Run(filename+"/"+test.name, func(t *testing.T) {
				out, verbs, err := exerciseMirror(t, filename, test.reply, test.missing, test.empty, test.failedUpload)
				if err == nil || !strings.Contains(out, "VAULT_SNAPSHOT_REMOTE=FAIL stage="+test.stage) || strings.Contains(out, "VAULT_SNAPSHOT_REMOTE=VERIFIED") {
					t.Fatalf("unproven backup reported success: %v; verbs=%v; %s", err, verbs, out)
				}
				if test.stage != "retention" && slices.Contains(verbs, "rm") {
					t.Fatal("retention must wait for successful upload and complete readback")
				}
			})
		}
	}
}
