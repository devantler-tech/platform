package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// Independent caller inventory: do not derive fixtures from the producer's
// list, which would conceal an omitted operational workflow.
var workflowPinFixtures = []string{
	".github/workflows/ci.yaml", ".github/workflows/cd.yaml", ".github/actions/deploy-prod/action.yml",
	".github/workflows/validate-main.yaml", ".github/workflows/dr-rebuild.yaml",
	".github/workflows/validate-image-verifier-liveness.yaml", ".github/workflows/prove-scoped-package-access.yaml",
}

func writeWorkflowPins(t *testing.T, version string) {
	t.Helper()
	for _, path := range workflowPinFixtures {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pin(version), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A new installation workflow must enter the independently exercised caller
// inventory; otherwise every existing fixture could pass while it was omitted.
// Discovery only asks whether an explicit pin key occurs. Unrelated workflow
// aliases are valid; strict ambiguity checks still apply to actual pin callers.
func hasWorkflowPin(data []byte) (bool, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var walk func(*yaml.Node) bool
	walk = func(node *yaml.Node) bool {
		if node.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value == "KSAIL_VERSION" {
					return true
				}
			}
		}
		// Content represents the written document, not an alias target. Never
		// follow Alias pointers, which could be cyclic.
		for _, child := range node.Content {
			if walk(child) {
				return true
			}
		}
		return false
	}
	found := false
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				return found, nil
			}
			return false, err
		}
		found = walk(&document) || found
	}
}

func discoverWorkflowPins(root string) (map[string]bool, error) {
	seen := map[string]bool{}
	for _, directory := range []string{".github/workflows", ".github/actions"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			caller, err := hasWorkflowPin(data)
			if err != nil {
				return fmt.Errorf("unreadable workflow inventory: %w", err)
			}
			if !caller {
				return nil
			}
			if _, err := verifyPins(data); err != nil {
				return fmt.Errorf("unreadable workflow pin inventory: %w", err)
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			seen[filepath.ToSlash(relative)] = true
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return seen, nil
}

func TestInventoryAllowsUnrelatedAnchoredWorkflow(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, ".github", "workflows")
	for _, path := range []string{directory, filepath.Join(root, ".github", "actions")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string][]byte{
		"caller.yaml":    pin("7.195.0"),
		"unrelated.yaml": []byte("env: &shared\n  EXAMPLE: safe\njobs:\n  first:\n    env: *shared\n"),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seen, err := discoverWorkflowPins(root)
	if err != nil || len(seen) != 1 || !seen[".github/workflows/caller.yaml"] {
		t.Fatalf("unrelated valid YAML blocked caller discovery: %v, %v", seen, err)
	}
}

func TestWorkflowDiscoveryReadsEveryDocument(t *testing.T) {
	for _, test := range []struct {
		name, data      string
		caller, invalid bool
	}{
		{"no caller", "env: &shared {EXAMPLE: safe}\njobs: {first: {env: *shared}}\n", false, false},
		{"later caller", "name: harmless\n---\nenv: {KSAIL_VERSION: '7.195.0'}\n", true, false},
		{"text is not a key", "name: KSAIL_VERSION\n", false, false},
		{"malformed unrelated suffix", "name: harmless\n---\n[", false, true},
		{"malformed caller suffix", "env: {KSAIL_VERSION: '7.195.0'}\n---\n[", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller, err := hasWorkflowPin([]byte(test.data))
			if (err != nil) != test.invalid || caller != test.caller {
				t.Fatalf("caller = %t, error = %v", caller, err)
			}
		})
	}
}

func TestPinCallerStillRejectsAliases(t *testing.T) {
	data := []byte("env: &shared {KSAIL_VERSION: '7.195.0'}\njobs: {first: {env: *shared}}\n")
	caller, err := hasWorkflowPin(data)
	if err != nil || !caller {
		t.Fatalf("pin caller not discovered: %v", err)
	}
	if _, err := verifyPins(data); err == nil || !strings.Contains(err.Error(), "aliases are unsupported") {
		t.Fatalf("real pin caller ambiguity passed: %v", err)
	}
}

func TestCurrentWorkflowPinInventoryIsComplete(t *testing.T) {
	root := filepath.Join("..", "..")
	seen, err := discoverWorkflowPins(root)
	if err != nil {
		t.Fatal(err)
	}
	// This non-Talos, disposable Cilium proof has a separately audited CLI /
	// image pairing. It is accounted for, not adopted by the deployment fold.
	const ciliumProof = ".github/workflows/prove-cilium-default-deny.yaml"
	data, err := os.ReadFile(filepath.Join(root, ciliumProof))
	if err != nil {
		t.Fatal(err)
	}
	version, err := verifyPins(data)
	if err != nil || version != "7.193.8" || !seen[ciliumProof] {
		t.Fatalf("independent Cilium proof profile changed: %s, %v", version, err)
	}
	delete(seen, ciliumProof)
	if len(seen) != len(workflowPinFixtures) {
		t.Fatalf("actual caller inventory differs from exercised fixtures: %v", seen)
	}
	for _, path := range workflowPinFixtures {
		if !seen[path] {
			t.Fatalf("unexercised or missing operational workflow %s", path)
		}
	}
}

func TestOperationalWorkflowDriftStopsTheActualFoldBeforeWrites(t *testing.T) {
	for _, path := range workflowPinFixtures {
		t.Run(path, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeWorkflowPins(t, "7.195.0")
			if err := os.WriteFile(path, pin("7.194.10"), 0o600); err != nil {
				t.Fatal(err)
			}
			for path, data := range map[string]string{"cluster.yaml": extensions, "cp.yaml": argsConfig, "worker.yaml": plainConfig} {
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := run([]string{"cluster.yaml", "cp.yaml", "worker.yaml"}); err == nil || !strings.Contains(err.Error(), "divergent KSail deployment pins") {
				t.Fatalf("operational workflow drift reached fold: %v", err)
			}
			for path, want := range map[string]string{"cp.yaml": argsConfig, "worker.yaml": plainConfig} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("divergent caller changed role %s", path)
				}
			}
		})
	}
}

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

func init() {
	// Failed fetches are retried; the tests that provoke them need not wait.
	fetchBackoff = 0
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

func TestReleasedImageRecoveryFoldInputsAreAccepted(t *testing.T) {
	// Read independently from the v7.202.3 tag (f9172ab810fdcb94b47351d04d70708f6878e10a).
	// Rejecting this audited source
	// prevents adoption of the released same-version boot-image recovery.
	inputs := map[string]string{
		"pkg/fsutil/configmanager":   "ac334b6ba5b7e474bfa88e13486a1ab0d1d6feea",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "83982b42d9ec0b34d159783969c39ff8832e6714",
		"charts":                     "c4ed7098f7dce6dca0c65649cd8ab3aaeccdf6f4",
		"go.mod":                     "3ef4674d6c9d1507ec198883c78712ce74819866",
		"go.sum":                     "c780001120d55f3eb85838940f5f149e62202fab",
	}
	_, err := verifyFoldInputs("7.202.3", func(string) (map[string]string, error) { return inputs, nil })
	if err != nil {
		t.Fatalf("released image recovery source was rejected: %v", err)
	}
	// A near match is not the reviewed source. Do not let this audit accept the
	// newer config manager with a dependency blob from a different release.
	inputs["go.mod"] = "e7265f2eee822033c9047eb50e7720872756aa03"
	if _, err := verifyFoldInputs("7.202.3", func(string) (map[string]string, error) { return inputs, nil }); err == nil {
		t.Fatal("unaudited mix of releases accepted")
	}
}

func TestReleasedOwnershipRecoveryFoldInputsAreAccepted(t *testing.T) {
	// Read independently from the v7.202.28 tag. This release carries the
	// ownership repair required by production recovery, without changing the fold.
	inputs := map[string]string{
		"pkg/fsutil/configmanager":   "8c46721a9c9703046eb099dd29ccb693dc166d42",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "83982b42d9ec0b34d159783969c39ff8832e6714",
		"charts":                     "a3fec71c3c9ba1d8ecf78fe9b95943f32e5499ab",
		"go.mod":                     "aa4556d3ce50ca3a2f7397a5f0125d302bb9431f",
		"go.sum":                     "5c4e0d2343d48b4844bbbe1fd7e706b0e767439a",
	}
	_, err := verifyFoldInputs("7.202.28", func(string) (map[string]string, error) { return inputs, nil })
	if err != nil {
		t.Fatalf("released ownership recovery source was rejected: %v", err)
	}
}

func TestReleasedFactoryRetryFoldInputsAreAccepted(t *testing.T) {
	// Independently read from v7.202.56 (5e387655892643dcf322a03d2ceb6d9ca6030979).
	// This source must pass before production can adopt its bounded Factory retry.
	inputs := map[string]string{
		"pkg/fsutil/configmanager":   "88779a5a93cc49f116d4f5eda8389becc14d1cd8",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "83982b42d9ec0b34d159783969c39ff8832e6714",
		"charts":                     "4351b93273821f333e9f0641e323ba99387844bb",
		"go.mod":                     "b1d9e18582954ce6a9851a64bdd1c7344d36067a",
		"go.sum":                     "b11643f42c4e3950f2513b6370c4ede1a99f3a0e",
	}
	if _, err := verifyFoldInputs("7.202.56", func(string) (map[string]string, error) { return inputs, nil }); err != nil {
		t.Fatalf("released Factory retry source was rejected: %v", err)
	}
	inputs["go.mod"] = "aa4556d3ce50ca3a2f7397a5f0125d302bb9431f"
	if _, err := verifyFoldInputs("7.202.56", func(string) (map[string]string, error) { return inputs, nil }); err == nil {
		t.Fatal("new release mixed with an older dependency input was accepted")
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
	if err == nil || !strings.Contains(err.Error(), "charts, go.mod, go.sum, pkg/apis, pkg/fsutil/configmanager, pkg/fsutil/generator/talos") {
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
		"pkg/fsutil/generator/talos/firewall.go":    "package talos\n",
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

// TestResolveFoldInputsRejectsCleanupFailure proves cleanup cannot leave an accepted source result.
func TestResolveFoldInputsRejectsCleanupFailure(t *testing.T) {
	repository, git := sourceRepository(t)
	if err := os.Remove(filepath.Join(repository, "go.sum")); err != nil {
		t.Fatal(err)
	}
	git("commit", "--quiet", "--all", "-m", "missing source input")
	git("tag", "v1.2.4")
	cleanupErr := errors.New("injected source cleanup failure")
	for _, tc := range []struct {
		name, version, sourceError string
	}{
		{"valid source", "1.2.3", ""},
		{"unread source", "1.2.4", "has no go.sum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, err := resolveFoldInputsWithCleanup(repository, tc.version, func(dir string) error {
				t.Cleanup(func() {
					if cleanup := os.RemoveAll(dir); cleanup != nil {
						t.Error(cleanup)
					}
				})
				return cleanupErr
			})
			if ids != nil || !errors.Is(err, cleanupErr) || !strings.Contains(err.Error(), tc.sourceError) {
				t.Fatalf("cleanup failure produced accepted source identities or lost an error: %d identities, %v", len(ids), err)
			}
		})
	}
}

// unrelatedToolCache stands in for a file another tool leaves in the temporary
// directory. A macOS sandbox writes xcrun's cache there (#5221); it is not a
// source checkout, so it must not read as one.
const unrelatedToolCache = "xcrun_db"

// plantUnrelatedToolCache writes that file into parent.
func plantUnrelatedToolCache(t *testing.T, parent string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(parent, unrelatedToolCache), []byte("cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ownedCheckouts lists the entries in parent that this helper's checkouts
// would be named as, so a cleanup check judges only what the helper creates.
func ownedCheckouts(parent string) ([]string, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	var owned []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), foldInputsCheckoutPrefix) {
			owned = append(owned, entry.Name())
		}
	}
	return owned, nil
}

// TestResolveFoldInputsRemovesItsCheckout checks real removal on successful and failed reads.
func TestResolveFoldInputsRemovesItsCheckout(t *testing.T) {
	repository, git := sourceRepository(t)
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	plantUnrelatedToolCache(t, parent)
	want := git("rev-parse", "v1.2.3:go.mod")
	if err := os.Remove(filepath.Join(repository, "go.sum")); err != nil {
		t.Fatal(err)
	}
	git("commit", "--quiet", "--all", "-m", "missing source input")
	git("tag", "v1.2.4")
	for _, version := range []string{"1.2.3", "1.2.4"} {
		ids, err := resolveFoldInputs(repository, version)
		if version == "1.2.3" && (err != nil || ids["go.mod"] != want || len(ids) != 6) {
			t.Fatalf("valid source lost accepted identities: %v, %v", ids, err)
		}
		if version == "1.2.4" && (err == nil || ids != nil) {
			t.Fatalf("unread source was accepted: %v, %v", ids, err)
		}
		owned, readErr := ownedCheckouts(parent)
		if readErr != nil || len(owned) != 0 {
			t.Fatalf("temporary source checkout remains after reading %s: %v, %v", version, owned, readErr)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, unrelatedToolCache)); err != nil {
		t.Fatalf("the unrelated tool cache must be left alone: %v", err)
	}
}

// TestOwnedCheckoutsFindsARetainedCheckout is the control for the cleanup check
// above: a checkout the helper keeps is found beside an unrelated tool cache.
func TestOwnedCheckoutsFindsARetainedCheckout(t *testing.T) {
	repository, _ := sourceRepository(t)
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	plantUnrelatedToolCache(t, parent)
	var kept string
	ids, err := resolveFoldInputsWithCleanup(repository, "1.2.3", func(dir string) error {
		kept = dir
		return nil
	})
	if err != nil || len(ids) != 6 {
		t.Fatalf("valid source lost accepted identities: %v, %v", ids, err)
	}
	owned, err := ownedCheckouts(parent)
	if err != nil || len(owned) != 1 || owned[0] != filepath.Base(kept) {
		t.Fatalf("a retained checkout %q was not the one owned checkout found: %v, %v", kept, owned, err)
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

// checkPinsAt runs the command's --check-pins mode with every deployment pin at
// version and repository standing in for the public KSail source.
func checkPinsAt(t *testing.T, version, repository string) error {
	t.Helper()
	original := ksailRepository
	ksailRepository = repository
	t.Cleanup(func() { ksailRepository = original })
	t.Chdir(t.TempDir())
	writeWorkflowPins(t, version)
	return run([]string{"--check-pins"})
}

func TestCheckPinsDecidesOnTheSourceItRead(t *testing.T) {
	if ksailRepository != "https://github.com/devantler-tech/ksail.git" {
		t.Fatalf("the check reads %q, not the public KSail source", ksailRepository)
	}
	dir, git := sourceRepository(t)
	// The local source is real but unaudited, so only a read that reached the
	// comparison can produce this error.
	if err := checkPinsAt(t, "1.2.3", dir); err == nil || !strings.Contains(err.Error(), "requires a new source audit") {
		t.Fatalf("unaudited source accepted: %v", err)
	}
	if err := checkPinsAt(t, "9.9.9", dir); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("unreadable release accepted: %v", err)
	}
	if err := checkPinsAt(t, "1.2.3", filepath.Join(dir, "absent")); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("absent source accepted: %v", err)
	}
	if err := checkPinsAt(t, "not-a-release", dir); err == nil || !strings.Contains(err.Error(), "not an explicit release version") {
		t.Fatalf("unresolvable pin accepted: %v", err)
	}
	// Record the local source as audited: the same command, reading the same
	// way, must now pass, and only for the release whose inputs were recorded.
	ids := map[string]string{}
	for _, input := range foldInputs {
		ids[input.path] = git("rev-parse", "v1.2.3:"+input.path)
	}
	original := auditedFoldInputs
	auditedFoldInputs = append(append(original[:0:0], original...), struct {
		audit string
		ids   map[string]string
	}{"1.2.3", ids})
	t.Cleanup(func() { auditedFoldInputs = original })
	if err := checkPinsAt(t, "1.2.3", dir); err != nil {
		t.Fatalf("audited source rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.invalid/changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("commit", "--quiet", "--all", "-m", "module change")
	git("tag", "v1.3.0")
	if err := checkPinsAt(t, "1.3.0", dir); err == nil || !strings.Contains(err.Error(), "fold input(s) go.mod since the set audited at 1.2.3") {
		t.Fatalf("changed module file at the next release: %v", err)
	}
}

func TestFetchIsRetriedBeforeItFails(t *testing.T) {
	dir, _ := sourceRepository(t)
	bin := t.TempDir()
	count := filepath.Join(bin, "fetches")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// Fail the first fetch, then hand over to git.
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = fetch ]; then echo x >> '" + count + "'; if [ \"$(wc -l < '" + count + "')\" -lt 2 ]; then echo transient >&2; exit 128; fi; fi; done\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := resolveFoldInputs(dir, "1.2.3"); err != nil {
		t.Fatalf("one transient fetch failure was not retried: %v", err)
	}
	if _, err := resolveFoldInputs(dir, "9.9.9"); err == nil {
		t.Fatal("a fetch that keeps failing passed")
	}
	fetches, err := os.ReadFile(count)
	if err != nil || strings.Count(string(fetches), "x") != 2+fetchAttempts {
		t.Fatalf("fetch ran %d times, want %d: %v", strings.Count(string(fetches), "x"), 2+fetchAttempts, err)
	}
}

func TestCommandDoesNotWriteEitherRoleAfterInvalidInput(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeWorkflowPins(t, "7.193.6")
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
