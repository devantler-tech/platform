package arcstaging_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const repoRoot = "../../.."

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func field(t *testing.T, value any, path ...string) any {
	t.Helper()
	for _, key := range path {
		mapping, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s: expected a mapping, got %T", strings.Join(path, "."), value)
		}
		value, ok = mapping[key]
		if !ok {
			t.Fatalf("missing %s", strings.Join(path, "."))
		}
	}
	return value
}

func equal(t *testing.T, value, want any) {
	t.Helper()
	if value != want {
		t.Fatalf("got %#v, want %#v", value, want)
	}
}

func TestReleasesUsePinnedCharts(t *testing.T) {
	for _, component := range []string{
		"k8s/bases/infrastructure/controllers/actions-runner-controller",
		"k8s/bases/infrastructure/ksail-analysis-runners",
	} {
		t.Run(filepath.Base(component), func(t *testing.T) {
			release := readYAML(t, component+"/helm-release.yaml")
			equal(t, field(t, release, "spec", "chartRef", "kind"), "OCIRepository")
			source := readYAML(t, component+"/oci-repository.yaml")
			equal(t, field(t, source, "spec", "ref", "tag"), "0.15.0")
			digest, ok := field(t, source, "spec", "ref", "digest").(string)
			if !ok || !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
				t.Fatalf("missing immutable chart digest: %#v", digest)
			}
		})
	}
}

func TestRepositoryScopedPoolCannotCreateUnboundedOrPrivilegedRunners(t *testing.T) {
	release := readYAML(t, "k8s/bases/infrastructure/ksail-analysis-runners/helm-release.yaml")
	values := field(t, release, "spec", "values")
	equal(t, field(t, values, "githubConfigUrl"), "https://github.com/devantler-tech/ksail")
	equal(t, field(t, values, "githubConfigSecret"), "arc-ksail-app")
	equal(t, field(t, values, "runnerScaleSetName"), "ksail-code-quality")
	equal(t, field(t, values, "minRunners"), 0)
	equal(t, field(t, values, "maxRunners"), 1)
	if _, exists := values.(map[string]any)["containerMode"]; exists {
		t.Fatal("container hooks and Docker-in-Docker are outside this pool's scope")
	}
	spec := field(t, values, "template", "spec")
	equal(t, field(t, spec, "automountServiceAccountToken"), false)
	equal(t, field(t, spec, "nodeSelector", "platform.devantler.tech/ksail-analysis"), "enabled")
	containers, ok := field(t, spec, "containers").([]any)
	if !ok || len(containers) != 1 {
		t.Fatal("runner must contain exactly one container")
	}
	runner := containers[0]
	equal(t, field(t, runner, "name"), "runner")
	image, ok := field(t, runner, "image").(string)
	if !ok || (!strings.HasPrefix(image, "ghcr.io/actions/actions-runner@sha256:") &&
		!strings.HasPrefix(image, analysisRepository+"@sha256:")) {
		t.Fatal("runner image must be digest-pinned")
	}
	for _, name := range []string{"privileged", "allowPrivilegeEscalation"} {
		equal(t, field(t, runner, "securityContext", name), false)
	}
	equal(t, field(t, runner, "resources", "requests", "memory"), "12Gi")
	equal(t, field(t, runner, "resources", "limits", "memory"), "14Gi")
	assertRunnerStorage(t, spec)
	listener := field(t, values, "listenerTemplate", "spec", "containers").([]any)
	if len(listener) != 1 {
		t.Fatal("listener must be separately bounded")
	}
	equal(t, field(t, listener[0], "name"), "listener")
	equal(t, field(t, listener[0], "resources", "limits", "memory"), "512Mi")
}

func TestControllerIsNamespaceScopedAndCredentialsAreExternal(t *testing.T) {
	controller := readYAML(t, "k8s/bases/infrastructure/controllers/actions-runner-controller/helm-release.yaml")
	equal(t, field(t, controller, "spec", "values", "flags", "watchSingleNamespace"), "arc-ksail-analysis")
	secret := readYAML(t, "k8s/bases/infrastructure/ksail-analysis-runners/external-secret.yaml")
	equal(t, field(t, secret, "kind"), "ExternalSecret")
	equal(t, field(t, secret, "spec", "secretStoreRef", "name"), "openbao")
	equal(t, field(t, secret, "spec", "secretStoreRef", "kind"), "SecretStore")
	equal(t, field(t, secret, "spec", "target", "name"), "arc-ksail-app")
	entries := field(t, secret, "spec", "data").([]any)
	if len(entries) != 3 {
		t.Fatal("only the three App authentication keys are expected")
	}
	properties := map[string]string{
		"github_app_id":              "app_id",
		"github_app_installation_id": "installation_id",
		"github_app_private_key":     "pem",
	}
	for _, entry := range entries {
		key, ok := field(t, entry, "secretKey").(string)
		if !ok {
			t.Fatal("App credential target key must be a string")
		}
		property, ok := properties[key]
		if !ok {
			t.Fatalf("unexpected or duplicate App credential target key %q", key)
		}
		equal(t, field(t, entry, "remoteRef", "key"), "infrastructure/github/app")
		equal(t, field(t, entry, "remoteRef", "property"), property)
		delete(properties, key)
	}
	if len(properties) != 0 {
		t.Fatalf("missing App credential target keys: %v", properties)
	}
}

func TestControllerLayerCreatesBothNamespacesBeforeItsScopedRBAC(t *testing.T) {
	component := "k8s/bases/infrastructure/controllers/actions-runner-controller"
	kustomization := readYAML(t, component+"/kustomization.yaml")
	resources := field(t, kustomization, "resources").([]any)
	for _, name := range []string{"controller", "runners"} {
		path := "namespace-" + name + ".yaml"
		found := false
		for _, resource := range resources {
			found = found || resource == path
		}
		if !found {
			t.Fatalf("controller layer must create %s before Helm installs its scoped RBAC", path)
		}
		namespace := readYAML(t, component+"/"+path)
		equal(t, field(t, namespace, "kind"), "Namespace")
		equal(t, field(t, namespace, "metadata", "annotations", "kustomize.toolkit.fluxcd.io/prune"), "disabled")
		equal(t, field(t, namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce"), "restricted")
	}
	pool := readYAML(t, "k8s/bases/infrastructure/ksail-analysis-runners/kustomization.yaml")
	for _, resource := range field(t, pool, "resources").([]any) {
		if strings.HasPrefix(resource.(string), "namespace") {
			t.Fatal("pool must not duplicate controller-owned namespaces")
		}
	}
}

func TestStagingGuardRunsUnconditionallyOnPullRequestsAndMergeGroups(t *testing.T) {
	workflow := readYAML(t, ".github/workflows/ci.yaml")
	events := field(t, workflow, "on").(map[string]any)
	for _, event := range []string{"pull_request", "merge_group"} {
		if _, ok := events[event]; !ok {
			t.Fatalf("missing %s trigger", event)
		}
	}
	changes := field(t, workflow, "jobs", "changes")
	if _, conditional := changes.(map[string]any)["if"]; conditional {
		t.Fatal("staging guard must run even when no deployment path changes")
	}
	for _, step := range field(t, changes, "steps").([]any) {
		mapping := step.(map[string]any)
		if mapping["run"] == "go test ./scripts/tests/arc-staging" {
			if _, conditional := mapping["if"]; conditional {
				t.Fatal("staging guard cannot be conditional")
			}
			return
		}
	}
	t.Fatal("missing unconditional staging guard")
}

func TestDNSAccessIsLimitedToTheDeclaredExternalDependencies(t *testing.T) {
	for _, path := range []string{
		"k8s/bases/infrastructure/controllers/actions-runner-controller/cilium-network-policy.yaml",
		"k8s/bases/infrastructure/ksail-analysis-runners/cilium-network-policy-runner.yaml",
	} {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			policy := readYAML(t, path)
			egress := field(t, policy, "spec", "egress").([]any)
			dependencies := map[string]bool{}
			dnsAllowed := map[string]bool{}
			for _, rule := range egress {
				mapping := rule.(map[string]any)
				if fqdnRules, ok := mapping["toFQDNs"].([]any); ok {
					for _, target := range fqdnRules {
						for _, name := range target.(map[string]any) {
							dependencies[name.(string)] = true
						}
					}
				}
				if ports, ok := mapping["toPorts"].([]any); ok {
					for _, port := range ports {
						rules, ok := port.(map[string]any)["rules"].(map[string]any)
						if !ok {
							continue
						}
						for _, dnsRule := range rules["dns"].([]any) {
							for _, name := range dnsRule.(map[string]any) {
								if name == "*" {
									t.Fatal("unrestricted DNS permits undeclared outbound traffic")
								}
								dnsAllowed[name.(string)] = true
							}
						}
					}
				}
			}
			if len(dependencies) == 0 || len(dependencies) != len(dnsAllowed) {
				t.Fatal("DNS access must match the finite external dependency list")
			}
			for name := range dependencies {
				if !dnsAllowed[name] {
					t.Errorf("missing DNS rule for %s", name)
				}
			}
		})
	}
}
