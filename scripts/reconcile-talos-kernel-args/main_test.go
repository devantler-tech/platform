package main

import (
	"bytes"
	"os"
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

func TestVersionContractRejectsUnreviewedOrDivergentPins(t *testing.T) {
	ci := "jobs:\n  deploy-prod:\n    steps:\n      - env:\n          KSAIL_VERSION: '7.193.6'\n      - env:\n          KSAIL_VERSION: '7.193.6'\n"
	cd := "jobs:\n  deploy:\n    steps:\n      - env:\n          KSAIL_VERSION: '7.193.6'\n"
	if err := verifyPins([]byte(ci), []byte(cd)); err != nil {
		t.Fatal(err)
	}
	// Both exact releases were source-audited, but a mixed set of pins is not
	// a coherent caller even when every individual version is approved.
	if err := verifyPins([]byte(strings.ReplaceAll(ci, "7.193.6", "7.193.8")), []byte(strings.ReplaceAll(cd, "7.193.6", "7.193.8"))); err != nil {
		t.Fatal("audited 7.193.8 pins rejected", err)
	}
	for _, bad := range []string{strings.Replace(ci, "7.193.6", "7.193.7", 1), strings.ReplaceAll(ci, "7.193.6", "7.193.7"), strings.Replace(ci, "7.193.6", "7.193.8", 1), strings.ReplaceAll(ci, "7.193.6", "7.193.8"), "jobs: {}\n", "jobs: ["} {
		if err := verifyPins([]byte(bad), []byte(cd)); err == nil {
			t.Fatal("unreviewed, missing or malformed pin accepted")
		}
	}
}

func TestVersionContractForAudited71940(t *testing.T) {
	for _, test := range []struct {
		name, ci, cd string
		wantErr      bool
	}{
		{"uniform audited release", "7.194.0", "7.194.0", false},
		{"mixed audited releases", "7.194.0", "7.193.8", true},
		{"mixed audited releases reversed", "7.193.8", "7.194.0", true},
		{"unaudited patch release", "7.194.1", "7.194.1", true},
		{"unaudited minor release", "7.195.0", "7.195.0", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ci := []byte("env:\n  KSAIL_VERSION: '" + test.ci + "'\n")
			cd := []byte("env:\n  KSAIL_VERSION: '" + test.cd + "'\n")
			if err := verifyPins(ci, cd); (err != nil) != test.wantErr {
				t.Fatalf("verifyPins(%q, %q) error = %v, wantErr %v", test.ci, test.cd, err, test.wantErr)
			}
		})
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
