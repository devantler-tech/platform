package arcstaging_test

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

const controllerAggregate = "k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml"
const runnerAggregate = "k8s/providers/hetzner/infrastructure/kustomization.yaml"
const controllerReference = "../../../../bases/infrastructure/controllers/actions-runner-controller/"
const runnerReference = "../../../bases/infrastructure/ksail-analysis-runners/"
const controllerRelease = "k8s/bases/infrastructure/controllers/actions-runner-controller/helm-release.yaml"
const runnerRelease = "k8s/bases/infrastructure/ksail-analysis-runners/helm-release.yaml"
const runtimeAction = ".github/actions/deploy-prod/action.yml"

func TestDeploymentAggregatesKeepARCInsideActivationEnvelope(t *testing.T) {
	if err := validateARCActivation(os.DirFS(repoRoot)); err != nil {
		t.Fatal(err)
	}
}

func activationFixture(active bool) fstest.MapFS {
	suspended, controller, runner := "true", "[]", "[]"
	image := "ghcr.io/actions/actions-runner@sha256:" + strings.Repeat("a", 64)
	if active {
		suspended = "false"
		controller = fmt.Sprintf("[%q]", controllerReference)
		runner = fmt.Sprintf("[%q]", runnerReference)
		image = analysisRepository + "@sha256:" + strings.Repeat("b", 64)
	}
	files := fstest.MapFS{}
	for name, data := range map[string]string{
		controllerAggregate: "resources: " + controller + "\n",
		runnerAggregate:     "resources: " + runner + "\n",
		controllerRelease:   "spec:\n  suspend: " + suspended + "\n",
		runnerRelease: "spec:\n  suspend: " + suspended + "\n  values:\n    template:\n      spec:\n" +
			"        containers: [{image: " + image + "}]\n        initContainers: [{image: " + image + "}]\n",
		runtimeAction: "runs:\n  steps:\n    - name: Verify the deployed KSail ARC canary\n" +
			"      if: >-\n        !cancelled() && steps.wait_flux_revision.outcome == 'success' && " +
			"( steps.cluster_update.outcome == 'skipped' || steps.wait_prod_api_stability.outcome == 'success' )\n" +
			"      shell: bash\n      env:\n        PLATFORM_MANIFEST_DIGEST: ${{ steps.publish_platform_manifest.outputs.digest }}\n" +
			"      run: |\n        export KUBECONFIG=\"${HOME}/.kube/config\"\n        bash scripts/verify-ksail-arc-runtime.sh --if-active\n",
	} {
		files[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return files
}

func TestActivationEnvelopeRejectsUnboundedOrUnverifiedStates(t *testing.T) {
	for _, active := range []bool{false, true} {
		if err := validateARCActivation(activationFixture(active)); err != nil {
			t.Fatalf("valid active=%t: %v", active, err)
		}
	}
	tests := []struct {
		name, file, from, to string
	}{
		{"controller only", runnerAggregate, runnerReference, "unrelated"},
		{"runner only", controllerAggregate, controllerReference, "unrelated"},
		{"duplicate activation", runnerAggregate, "resources:", "components: [\"" + runnerReference + "\"]\nresources:"},
		{"component activation", runnerAggregate, "resources:", "components:"},
		{"legacy base activation", runnerAggregate, "resources:", "bases:"},
		{"suspended controller", controllerRelease, "suspend: false", "suspend: true"},
		{"suspended runner", runnerRelease, "suspend: false", "suspend: true"},
		{"upstream image active", runnerRelease, analysisRepository, "ghcr.io/actions/actions-runner"},
		{"mutable image", runnerRelease, "@sha256:" + strings.Repeat("b", 64), ":latest"},
		{"zero image digest", runnerRelease, strings.Repeat("b", 64), strings.Repeat("0", 64)},
		{"missing canary", runtimeAction, "Verify the deployed KSail ARC canary", "Unrelated step"},
		{"skipped canary", runtimeAction, "!cancelled() &&", "false &&"},
		{"waived canary", runtimeAction, "      shell: bash", "      continue-on-error: true\n      shell: bash"},
		{"wrong deployment digest", runtimeAction, "steps.publish_platform_manifest.outputs.digest", "github.sha"},
		{"unexpected proof environment", runtimeAction, "      env:", "      env:\n        PATH: /unverified"},
		{"missing suspension declaration", runnerRelease, "suspend: false", "unrelated: false"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := activationFixture(true)
			files[test.file].Data = []byte(strings.ReplaceAll(string(files[test.file].Data), test.from, test.to))
			if err := validateARCActivation(files); err == nil {
				t.Fatal("accepted invalid activation")
			}
		})
	}
	for _, test := range []struct{ name, path string }{
		{"other cluster", "k8s/clusters/local/kustomization.yaml"},
		{"other production layer", "k8s/providers/hetzner/apps/kustomization.yaml"},
		{"alternate yaml filename", "k8s/clusters/local/kustomization.yml"},
		{"alternate canonical filename", "k8s/clusters/local/Kustomization"},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := activationFixture(true)
			files[test.path] = &fstest.MapFile{Data: []byte("resources: [\"" + runnerReference + "\"]\n")}
			if err := validateARCActivation(files); err == nil {
				t.Fatal("accepted activation from another aggregate")
			}
		})
	}
	files := activationFixture(false)
	files[runnerRelease].Data = []byte(strings.ReplaceAll(string(files[runnerRelease].Data), "suspend: true", "suspend: false"))
	if err := validateARCActivation(files); err == nil {
		t.Fatal("accepted unsuspended inactive runner")
	}
}

func validateARCActivation(files fs.FS) error {
	counts := map[string]int{controllerAggregate: 0, runnerAggregate: 0}
	allowed := map[string]string{controllerAggregate: controllerReference, runnerAggregate: runnerReference}
	err := fs.WalkDir(files, "k8s", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (entry.Name() != "kustomization.yaml" && entry.Name() != "kustomization.yml" && entry.Name() != "Kustomization") ||
			strings.HasPrefix(name, strings.TrimSuffix(controllerRelease, "/helm-release.yaml")+"/") ||
			strings.HasPrefix(name, strings.TrimSuffix(runnerRelease, "/helm-release.yaml")+"/") {
			return nil
		}
		var aggregate struct {
			Resources  []string `yaml:"resources"`
			Components []string `yaml:"components"`
			Bases      []string `yaml:"bases"`
		}
		if err := activationYAML(files, name, &aggregate); err != nil {
			return err
		}
		for _, reference := range append(aggregate.Components, aggregate.Bases...) {
			if strings.Contains(reference, "actions-runner-controller") || strings.Contains(reference, "ksail-analysis-runners") {
				return fmt.Errorf("ARC activation must be an explicit resource, not a component or legacy base: %s", name)
			}
		}
		for _, reference := range aggregate.Resources {
			if !strings.Contains(reference, "actions-runner-controller") && !strings.Contains(reference, "ksail-analysis-runners") {
				continue
			}
			if expected, ok := allowed[name]; !ok || reference != expected {
				return fmt.Errorf("ARC reference outside approved aggregate: %s", name)
			}
			counts[name]++
		}
		return nil
	})
	if err != nil {
		return err
	}
	active := counts[controllerAggregate] == 1 && counts[runnerAggregate] == 1
	if !active && (counts[controllerAggregate] != 0 || counts[runnerAggregate] != 0) {
		return fmt.Errorf("partial or duplicate ARC activation")
	}
	var image string
	for _, name := range []string{controllerRelease, runnerRelease} {
		var release struct {
			Spec struct {
				Suspend *bool `yaml:"suspend"`
				Values  struct {
					Template struct {
						Spec struct {
							Containers     []struct{ Image string } `yaml:"containers"`
							InitContainers []struct{ Image string } `yaml:"initContainers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"values"`
			} `yaml:"spec"`
		}
		if err := activationYAML(files, name, &release); err != nil {
			return err
		}
		if release.Spec.Suspend == nil || *release.Spec.Suspend == active {
			return fmt.Errorf("ARC release suspension does not match aggregate activation: %s", name)
		}
		if name == runnerRelease {
			containers := release.Spec.Values.Template.Spec.Containers
			init := release.Spec.Values.Template.Spec.InitContainers
			if len(containers) != 1 || len(init) != 1 || containers[0].Image != init[0].Image {
				return fmt.Errorf("runner and init images must have one identical immutable pin")
			}
			image = containers[0].Image
		}
	}
	repository := regexp.QuoteMeta(analysisRepository)
	if !active {
		repository += "|" + regexp.QuoteMeta("ghcr.io/actions/actions-runner")
	}
	if !regexp.MustCompile("^("+repository+")@sha256:[0-9a-f]{64}$").MatchString(image) || strings.HasSuffix(image, strings.Repeat("0", 64)) {
		return fmt.Errorf("ARC image is outside the immutable publication envelope")
	}
	return validateARCRuntimeHook(files)
}

func activationYAML(files fs.FS, name string, target any) error {
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, target)
}

func validateARCRuntimeHook(files fs.FS) error {
	var action struct {
		Runs struct {
			Steps []map[string]any `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := activationYAML(files, runtimeAction, &action); err != nil {
		return err
	}
	count := 0
	condition := "!cancelled() && steps.wait_flux_revision.outcome == 'success' && ( steps.cluster_update.outcome == 'skipped' || steps.wait_prod_api_stability.outcome == 'success' )"
	command := "export KUBECONFIG=\"${HOME}/.kube/config\"\nbash scripts/verify-ksail-arc-runtime.sh --if-active"
	for _, step := range action.Runs.Steps {
		if step["name"] != "Verify the deployed KSail ARC canary" {
			continue
		}
		count++
		_, waived := step["continue-on-error"]
		guard, ok := step["if"].(string)
		run, runOK := step["run"].(string)
		env, envOK := step["env"].(map[string]any)
		if waived || !ok || strings.Join(strings.Fields(guard), " ") != condition || !runOK || strings.TrimSpace(run) != command ||
			step["shell"] != "bash" || !envOK || len(env) != 1 || env["PLATFORM_MANIFEST_DIGEST"] != "${{ steps.publish_platform_manifest.outputs.digest }}" {
			return fmt.Errorf("ARC runtime proof is skipped, waived, or bound to the wrong deployment")
		}
	}
	if count != 1 {
		return fmt.Errorf("exactly one protected ARC runtime proof is required")
	}
	return nil
}
