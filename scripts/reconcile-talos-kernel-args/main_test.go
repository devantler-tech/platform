package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const extensions = "spec:\n  cluster:\n    talos:\n      extensions: [' siderolabs/iscsi-tools ', '', siderolabs/iscsi-tools]\n"
const argsConfig = "version: v1alpha1\nmachine:\n  install:\n    image: example.invalid/installer:v1.13.10\n    disk: /dev/sda\n    extraKernelArgs: [' lsm=apparmor ', '', lsm=apparmor]\n    grubUseUKICmdline: false\ncluster:\n  clusterName: preserved\n"
const plainConfig = "version: v1alpha1\nmachine:\n  install:\n    image: example.invalid/installer:v1.13.10\n    disk: /dev/sda\n    grubUseUKICmdline: false\n"

func assertFolded(t *testing.T, data []byte) {
	t.Helper()
	var got struct {
		Machine struct {
			Install struct {
				Args  []string `yaml:"extraKernelArgs"`
				UKI   bool     `yaml:"grubUseUKICmdline"`
				Disk  string   `yaml:"disk"`
				Image string   `yaml:"image"`
			} `yaml:"install"`
		} `yaml:"machine"`
	}
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Machine.Install.Args) != 0 || !got.Machine.Install.UKI || got.Machine.Install.Disk != "/dev/sda" || got.Machine.Install.Image != "example.invalid/installer:v1.13.10" {
		t.Fatalf("fold lost installer data or retained arguments: %+v", got)
	}
}

func TestFoldBothRolesFromEitherRole(t *testing.T) {
	for _, role := range []string{"control-plane", "worker"} {
		t.Run(role, func(t *testing.T) {
			cp, worker := plainConfig, plainConfig
			if role == "worker" {
				worker = argsConfig
			} else {
				cp = argsConfig
			}
			outCP, outWorker, err := fold([]byte(extensions), []byte(cp), []byte(worker))
			if err != nil {
				t.Fatal(err)
			}
			assertFolded(t, outCP)
			assertFolded(t, outWorker)
		})
	}
}

func TestFoldNoopPreservesBytes(t *testing.T) {
	for _, test := range []struct{ name, config, cp, worker string }{
		{"no extensions", "spec: {}\n", argsConfig, argsConfig},
		{"empty normalized extensions", "spec:\n  cluster:\n    talos:\n      extensions: ['', '   ']\n", argsConfig, argsConfig},
		{"no arguments", extensions, plainConfig, plainConfig},
		{"empty normalized arguments", extensions, strings.Replace(argsConfig, "[' lsm=apparmor ', '', lsm=apparmor]", "['', '   ']", 1), plainConfig},
	} {
		t.Run(test.name, func(t *testing.T) {
			cp, worker, err := fold([]byte(test.config), []byte(test.cp), []byte(test.worker))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(cp, []byte(test.cp)) || !bytes.Equal(worker, []byte(test.worker)) {
				t.Fatal("inactive fold changed bytes")
			}
		})
	}
}

func TestExplicitSchematicSelectionBoundary(t *testing.T) {
	for _, test := range []struct {
		name, id string
		folded   bool
	}{
		{"explicit schematic", "explicit-id", false},
		{"normalized explicit schematic", " explicit-id ", false},
		{"blank schematic", "", true},
		{"whitespace schematic", "   ", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := strings.Replace(extensions, "      extensions:", "      schematicId: '"+test.id+"'\n      extensions:", 1)
			if config == extensions {
				t.Fatal("schematic selection fixture did not change")
			}
			cp, worker, err := fold([]byte(config), []byte(argsConfig), []byte(argsConfig))
			if err != nil {
				t.Fatal(err)
			}
			if test.folded {
				assertFolded(t, cp)
				assertFolded(t, worker)
			} else if !bytes.Equal(cp, []byte(argsConfig)) || !bytes.Equal(worker, []byte(argsConfig)) {
				t.Fatal("explicit schematic must preserve both role files byte-for-byte")
			}
		})
	}
}

func TestFoldPreservesOtherDocumentsAndAbsentInstall(t *testing.T) {
	other := "---\napiVersion: v1alpha1\nkind: HostnameConfig\nhostname: unchanged\n"
	cp, worker, err := fold([]byte(extensions), []byte(argsConfig+other), []byte("version: v1alpha1\nmachine:\n  type: worker\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cp), "hostname: unchanged") || !strings.Contains(string(cp), "clusterName: preserved") || strings.Contains(string(worker), "install:") {
		t.Fatal("unrelated documents changed or install manufactured")
	}
	_, worker, err = fold([]byte(extensions), []byte(argsConfig), []byte("version: v1alpha1\ncluster: {}\n"))
	if err != nil || strings.Contains(string(worker), "machine:") {
		t.Fatal("missing machine was manufactured", err)
	}
}

func TestFoldRejectsAmbiguousOrMalformedInputs(t *testing.T) {
	for _, test := range []struct{ name, config, cp, worker string }{
		{"duplicate key", extensions, argsConfig + "machine: {}\n", plainConfig},
		{"duplicate alpha document", extensions, argsConfig + "---\n" + plainConfig, plainConfig},
		{"malformed trailing document", extensions, argsConfig + "---\n[", plainConfig},
		{"worker malformed", extensions, argsConfig, "version: v1alpha1\nmachine: ["},
		{"install wrong type", extensions, argsConfig, "version: v1alpha1\nmachine:\n  install: bad\n"},
		{"args wrong type", extensions, strings.Replace(argsConfig, "[' lsm=apparmor ', '', lsm=apparmor]", "bad", 1), plainConfig},
		{"numeric args", extensions, strings.Replace(argsConfig, "[' lsm=apparmor ', '', lsm=apparmor]", "[123]", 1), plainConfig},
		{"numeric extension", "spec:\n  cluster:\n    talos:\n      extensions: [123]\n", argsConfig, plainConfig},
		{"numeric schematic", strings.Replace(extensions, "      extensions:", "      schematicId: 123\n      extensions:", 1), argsConfig, plainConfig},
		{"multiple cluster configs", extensions + "---\n" + extensions, argsConfig, plainConfig},
	} {
		t.Run(test.name, func(t *testing.T) {
			cp, worker, err := fold([]byte(test.config), []byte(test.cp), []byte(test.worker))
			if err == nil || cp != nil || worker != nil {
				t.Fatal("invalid evidence returned writable configs")
			}
		})
	}
}

func pin(version string) []byte {
	return []byte("env:\n  KSAIL_VERSION: '" + version + "'\n")
}

func TestPinsMustBeUniformExplicitReleaseVersions(t *testing.T) {
	ci := "jobs:\n  deploy-prod:\n    steps:\n      - env:\n          KSAIL_VERSION: '7.193.6'\n      - env:\n          KSAIL_VERSION: '7.193.6'\n"
	cd := "jobs:\n  deploy:\n    steps:\n      - env:\n          KSAIL_VERSION: '7.193.6'\n"
	if version, err := verifyPins([]byte(ci), []byte(cd)); err != nil || version != "7.193.6" {
		t.Fatalf("uniform pins: version %q, error %v", version, err)
	}
	for _, test := range []struct {
		name    string
		pins    [][]byte
		wantErr string
	}{
		{"no evidence", nil, "deployment pin evidence absent"},
		{"one pin of several differs", [][]byte{[]byte(strings.Replace(ci, "7.193.6", "7.193.8", 1)), []byte(cd)}, "divergent KSail deployment pins"},
		{"mixed CI pin", [][]byte{pin("7.194.10"), pin("7.195.0"), pin("7.195.0")}, "divergent KSail deployment pins"},
		{"mixed CD pin", [][]byte{pin("7.195.0"), pin("7.194.10"), pin("7.195.0")}, "divergent KSail deployment pins"},
		{"mixed action pin", [][]byte{pin("7.195.0"), pin("7.195.0"), pin("7.194.10")}, "divergent KSail deployment pins"},
		{"missing pin", [][]byte{[]byte("jobs: {}\n"), []byte(cd)}, "missing explicit KSAIL_VERSION"},
		{"malformed document", [][]byte{[]byte("jobs: ["), []byte(cd)}, "invalid deployment pin document"},
		{"unquoted number", [][]byte{[]byte("env:\n  KSAIL_VERSION: 7.195\n")}, "not an explicit release version"},
		{"floating version", [][]byte{pin("latest")}, "not an explicit release version"},
		{"prefixed version", [][]byte{pin("v7.195.0")}, "not an explicit release version"},
		{"version with a ref suffix", [][]byte{pin("7.195.0^{}")}, "not an explicit release version"},
		{"version with a path", [][]byte{pin("7.195.0/../../heads/main")}, "not an explicit release version"},
		{"version with a trailing newline", [][]byte{[]byte("env:\n  KSAIL_VERSION: \"7.195.0\\n\"\n")}, "not an explicit release version"},
		{"empty version", [][]byte{pin("")}, "not an explicit release version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if version, err := verifyPins(test.pins...); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("verifyPins = %q, %v; want error containing %q", version, err, test.wantErr)
			}
		})
	}
}

func latestAudited() map[string]string {
	ids := map[string]string{}
	for path, id := range auditedFoldInputs[len(auditedFoldInputs)-1].ids {
		ids[path] = id
	}
	return ids
}

func TestEveryAuditedSetIsCompleteAndDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, set := range auditedFoldInputs {
		if len(set.ids) != len(foldInputs) {
			t.Fatalf("set audited at %s records %d inputs, want %d", set.audit, len(set.ids), len(foldInputs))
		}
		key := ""
		for _, input := range foldInputs {
			if !gitObjectID.MatchString(set.ids[input.path]) {
				t.Fatalf("set audited at %s has no object ID for %s", set.audit, input.path)
			}
			key += set.ids[input.path]
		}
		if other, dup := seen[key]; dup {
			t.Fatalf("sets audited at %s and %s are identical", other, set.audit)
		}
		seen[key] = set.audit
		audit, err := verifyFoldInputs("1.2.3", func(string) (map[string]string, error) { return set.ids, nil })
		if err != nil || audit != set.audit {
			t.Fatalf("set audited at %s: matched %q, error %v", set.audit, audit, err)
		}
	}
}

func TestUnchangedFoldInputsPassWhateverTheVersion(t *testing.T) {
	// A later release with the audited inputs needs no edit here: the check
	// compares content, not the version's name.
	asked := ""
	audit, err := verifyFoldInputs("7.999.0", func(version string) (map[string]string, error) {
		asked = version
		return latestAudited(), nil
	})
	if err != nil || audit != auditedFoldInputs[len(auditedFoldInputs)-1].audit {
		t.Fatalf("audited inputs rejected: %q, %v", audit, err)
	}
	if asked != "7.999.0" {
		t.Fatalf("inputs resolved for %q, not the pinned release", asked)
	}
}

func TestAnyChangedFoldInputFailsAndIsNamed(t *testing.T) {
	const other = "0123456789abcdef0123456789abcdef01234567"
	for _, input := range foldInputs {
		t.Run(input.path, func(t *testing.T) {
			ids := latestAudited()
			ids[input.path] = other
			_, err := verifyFoldInputs("7.999.0", func(string) (map[string]string, error) { return ids, nil })
			if err == nil || !strings.Contains(err.Error(), "requires a new source audit") {
				t.Fatalf("changed %s accepted: %v", input.path, err)
			}
			if !strings.Contains(err.Error(), "fold input(s) "+input.path+" since") {
				t.Fatalf("error does not name exactly the changed input %s: %v", input.path, err)
			}
		})
	}
	// Inputs taken from two different audited sets are not an audited set.
	mixed := latestAudited()
	mixed["pkg/apis"] = auditedFoldInputs[0].ids["pkg/apis"]
	if _, err := verifyFoldInputs("7.999.0", func(string) (map[string]string, error) { return mixed, nil }); err == nil || !strings.Contains(err.Error(), "fold input(s) pkg/apis since") {
		t.Fatalf("inputs mixed from two audited sets: %v", err)
	}
	all := map[string]string{}
	for _, input := range foldInputs {
		all[input.path] = other
	}
	_, err := verifyFoldInputs("7.999.0", func(string) (map[string]string, error) { return all, nil })
	if err == nil || !strings.Contains(err.Error(), "charts, go.mod, go.sum, pkg/apis, pkg/fsutil/configmanager") {
		t.Fatalf("fully changed inputs: %v", err)
	}
}

func TestUnreadFoldInputsNeverPass(t *testing.T) {
	failed := fmt.Errorf("network unreachable")
	if _, err := verifyFoldInputs("7.195.0", func(string) (map[string]string, error) { return latestAudited(), failed }); err == nil || !strings.Contains(err.Error(), "could not be read") || !errors.Is(err, failed) {
		t.Fatalf("failed read accepted: %v", err)
	}
	if _, err := verifyFoldInputs("7.195.0", func(string) (map[string]string, error) { return nil, nil }); err == nil {
		t.Fatal("empty read accepted")
	}
	for _, input := range foldInputs {
		for name, value := range map[string]string{"absent": "", "truncated": latestAudited()[input.path][:12], "not an object ID": "MISSING"} {
			t.Run(input.path+" "+name, func(t *testing.T) {
				ids := latestAudited()
				if value == "" {
					delete(ids, input.path)
				} else {
					ids[input.path] = value
				}
				_, err := verifyFoldInputs("7.195.0", func(string) (map[string]string, error) { return ids, nil })
				if err == nil || !strings.Contains(err.Error(), "fold input "+input.path+" was not read") {
					t.Fatalf("partial read accepted: %v", err)
				}
			})
		}
	}
}

// sourceRepository builds a local repository shaped like the KSail source,
// tagged v1.2.3, and returns its location and a runner for further commands.
func sourceRepository(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	for path, data := range map[string]string{
		"pkg/fsutil/configmanager/talos/configs.go": "package talos\n",
		"pkg/apis/cluster/v1alpha1/types.go":        "package v1alpha1\n",
		"charts/ksail/Chart.yaml":                   "name: ksail\n",
		"go.mod":                                    "module example.invalid/ksail\n",
		"go.sum":                                    "example.invalid/dependency v1.0.0 h1:AAAA\n",
		"pkg/unrelated/unrelated.go":                "package unrelated\n",
	} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "--all")
	git("commit", "--quiet", "-m", "release")
	git("tag", "v1.2.3")
	return dir, git
}

func TestResolveFoldInputsReadsTheTaggedSource(t *testing.T) {
	dir, git := sourceRepository(t)
	want := map[string]string{}
	for _, input := range foldInputs {
		want[input.path] = git("rev-parse", "v1.2.3:"+input.path)
	}
	got, err := resolveFoldInputs(dir, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("resolved %d inputs, want %d", len(got), len(want))
	}
	for path, id := range want {
		if got[path] != id {
			t.Fatalf("%s resolved to %q, want %q", path, got[path], id)
		}
	}

	// A change outside the fold inputs leaves every ID as it was; a change
	// inside one moves exactly that input. An annotated tag resolves alike.
	if err := os.WriteFile(filepath.Join(dir, "pkg/unrelated/unrelated.go"), []byte("package unrelated // changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("commit", "--quiet", "--all", "-m", "unrelated")
	git("tag", "--annotate", "-m", "release", "v1.2.4")
	unrelated, err := resolveFoldInputs(dir, "1.2.4")
	if err != nil {
		t.Fatal(err)
	}
	for path, id := range want {
		if unrelated[path] != id {
			t.Fatalf("%s moved after an unrelated change", path)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg/fsutil/configmanager/talos/configs.go"), []byte("package talos // changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("commit", "--quiet", "--all", "-m", "fold change")
	git("tag", "v1.2.5")
	changed, err := resolveFoldInputs(dir, "1.2.5")
	if err != nil {
		t.Fatal(err)
	}
	for path, id := range want {
		if moved := changed[path] != id; moved != (path == "pkg/fsutil/configmanager") {
			t.Fatalf("%s moved=%v after a change to the configuration manager only", path, moved)
		}
	}
}

func TestResolveFoldInputsIgnoresInheritedGitEnvironment(t *testing.T) {
	dir, git := sourceRepository(t)
	want := git("rev-parse", "v1.2.3:go.mod")
	// A caller such as a git hook exports these. Honouring them would point
	// the read, and the fetch that precedes it, at the caller's repository.
	caller := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(caller, ".git"))
	t.Setenv("GIT_WORK_TREE", caller)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+filepath.Join(caller, "elsewhere")+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", dir)
	got, err := resolveFoldInputs(dir, "1.2.3")
	if err != nil || got["go.mod"] != want {
		t.Fatalf("resolved %q, %v; want %q", got["go.mod"], err, want)
	}
	if entries, err := os.ReadDir(caller); err != nil || len(entries) != 0 {
		t.Fatalf("the caller's repository location was written to: %v, %v", entries, err)
	}
}

func TestResolveFoldInputsFailsOnAnythingItCannotRead(t *testing.T) {
	dir, git := sourceRepository(t)
	for name, version := range map[string]string{"absent tag": "9.9.9", "not a version": "main", "ref syntax": "1.2.3^{}"} {
		if ids, err := resolveFoldInputs(dir, version); err == nil {
			t.Fatalf("%s resolved to %v", name, ids)
		}
	}
	if ids, err := resolveFoldInputs(filepath.Join(dir, "absent"), "1.2.3"); err == nil {
		t.Fatalf("absent repository resolved to %v", ids)
	}
	// A branch that merely shares the release's name is not the release tag.
	git("branch", "v3.0.0")
	if ids, err := resolveFoldInputs(dir, "3.0.0"); err == nil {
		t.Fatalf("branch accepted as a release tag: %v", ids)
	}
	git("rm", "--quiet", "go.sum")
	git("commit", "--quiet", "-m", "drop go.sum")
	git("tag", "v2.0.0")
	if _, err := resolveFoldInputs(dir, "2.0.0"); err == nil || !strings.Contains(err.Error(), "go.sum") {
		t.Fatalf("missing input accepted or unnamed: %v", err)
	}
	git("rm", "--quiet", "-r", "charts")
	if err := os.WriteFile(filepath.Join(dir, "charts"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("restored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "--all")
	git("commit", "--quiet", "-m", "charts becomes a file")
	git("tag", "v2.0.1")
	if _, err := resolveFoldInputs(dir, "2.0.1"); err == nil || !strings.Contains(err.Error(), "charts") {
		t.Fatalf("input of the wrong kind accepted or unnamed: %v", err)
	}
}

func TestCheckPinsFailsClosedWithoutReadingAnAuditedSet(t *testing.T) {
	// The command reads the real source only in --check-pins mode. Pointed at
	// pins it cannot resolve, it must fail rather than report a match.
	dir := t.TempDir()
	t.Chdir(dir)
	for _, path := range []string{".github/workflows/ci.yaml", ".github/workflows/cd.yaml", ".github/actions/deploy-prod/action.yml"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pin("not-a-release"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run([]string{"--check-pins"}); err == nil || !strings.Contains(err.Error(), "not an explicit release version") {
		t.Fatalf("unresolvable pin accepted: %v", err)
	}
}

func TestCommandDoesNotWriteEitherRoleAfterInvalidInput(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, path := range []string{".github/workflows/ci.yaml", ".github/workflows/cd.yaml", ".github/actions/deploy-prod/action.yml"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("env:\n  KSAIL_VERSION: '7.193.6'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{"cluster.yaml": extensions, "cp.yaml": argsConfig, "worker.yaml": "version: v1alpha1\nmachine: ["} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run([]string{"cluster.yaml", "cp.yaml", "worker.yaml"}); err == nil {
		t.Fatal("invalid worker accepted")
	}
	for path, want := range map[string]string{"cp.yaml": argsConfig, "worker.yaml": "version: v1alpha1\nmachine: ["} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatal("command changed rendered input before validating both roles", path, err)
		}
	}
}
