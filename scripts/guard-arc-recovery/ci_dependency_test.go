package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type nativeCIStep struct {
	Env map[string]string `yaml:"env"`
	Run string            `yaml:"run"`
}

// The real proxy test must exercise the same verified binary as production
// authorization, after its installation and client-only startup check.
func nativeKubectlDependency(steps, authorization []nativeCIStep) error {
	var approved map[string]string
	for _, step := range authorization {
		if step.Env["KUBECTL_VERSION"] != "" && step.Env["KUBECTL_SHA256"] != "" {
			approved = step.Env
			break
		}
	}
	if approved == nil {
		return errors.New("production authorization declares no verified kubectl")
	}
	installed, native := -1, -1
	for index, step := range steps {
		if strings.TrimSpace(step.Run) == "go test -timeout 20m ./..." {
			native = index
		}
		if step.Env["KUBECTL_VERSION"] != approved["KUBECTL_VERSION"] || step.Env["KUBECTL_SHA256"] != approved["KUBECTL_SHA256"] {
			continue
		}
		verify, install := strings.Index(step.Run, "sha256sum --check"), strings.Index(step.Run, "sudo install")
		startup := strings.Index(step.Run, "kubectl version --client")
		path := strings.Index(step.Run, `echo /usr/local/bin >> "$GITHUB_PATH"`)
		if strings.Contains(step.Run, "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl") && verify >= 0 && install > verify && path > install && startup > path && strings.Contains(step.Run, `export PATH="/usr/local/bin:$PATH"`) {
			installed = index
		}
	}
	if installed < 0 || native <= installed {
		return errors.New("native suite needs the verified kubectl and client startup check first")
	}
	return nil
}

func TestNativeSuiteInstallsTheApprovedKubectlFirst(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct{ Steps []nativeCIStep } `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	steps := workflow.Jobs["validate-go-regressions"].Steps
	authorization := workflow.Jobs["validate-eks-authorization"].Steps
	if err := nativeKubectlDependency(steps, authorization); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"remove", "late", "checksum", "no startup", "no path"} {
		t.Run(mutation, func(t *testing.T) {
			changed := append([]nativeCIStep(nil), steps...)
			for index, step := range changed {
				if step.Env["KUBECTL_VERSION"] == "" {
					continue
				}
				switch mutation {
				case "remove":
					changed = append(changed[:index], changed[index+1:]...)
				case "late":
					changed = append(append(changed[:index], changed[index+1:]...), step)
				case "checksum":
					changed[index].Env = map[string]string{"KUBECTL_VERSION": step.Env["KUBECTL_VERSION"], "KUBECTL_SHA256": "unapproved"}
				case "no startup":
					changed[index].Run = strings.ReplaceAll(step.Run, "kubectl version --client", "echo untested")
				case "no path":
					changed[index].Run = strings.ReplaceAll(step.Run, `"$GITHUB_PATH"`, `"/dev/null"`)
				}
				break
			}
			if err := nativeKubectlDependency(changed, authorization); err == nil {
				t.Fatal("accepted an absent, late, unverified or uninitialized native dependency")
			}
		})
	}
}
