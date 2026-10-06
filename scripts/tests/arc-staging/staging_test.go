package arcstaging_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

func TestStagedReleasesAreSuspendedAndPinned(t *testing.T) {
	for _, component := range []string{
		"k8s/bases/infrastructure/controllers/actions-runner-controller",
		"k8s/bases/infrastructure/actions-runners",
	} {
		t.Run(filepath.Base(component), func(t *testing.T) {
			release := readYAML(t, component+"/helm-release.yaml")
			equal(t, field(t, release, "spec", "suspend"), true)
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

func TestOrganizationRegistrationUsesAnExplicitOptInRunnerGroup(t *testing.T) {
	release := readYAML(t, "k8s/bases/infrastructure/actions-runners/helm-release.yaml")
	values := field(t, release, "spec", "values")
	equal(t, field(t, values, "githubConfigUrl"), "https://github.com/devantler-tech")
	equal(t, field(t, values, "runnerGroup"), "platform")
	equal(t, field(t, values, "runnerScaleSetName"), "platform-linux")
}

func TestPoolCannotCreateUnboundedOrPrivilegedRunners(t *testing.T) {
	release := readYAML(t, "k8s/bases/infrastructure/actions-runners/helm-release.yaml")
	values := field(t, release, "spec", "values")
	equal(t, field(t, values, "minRunners"), 0)
	equal(t, field(t, values, "maxRunners"), 1)
	valueMap, ok := values.(map[string]any)
	if !ok {
		t.Fatal("runner values must be a mapping")
	}
	if _, exists := valueMap["containerMode"]; exists {
		t.Fatal("container hooks and Docker-in-Docker are outside this pool's scope")
	}
	spec := field(t, values, "template", "spec")
	equal(t, field(t, spec, "automountServiceAccountToken"), false)
	equal(t, field(t, spec, "nodeSelector", "platform.devantler.tech/ci-runner"), "enabled")
	tolerations := field(t, spec, "tolerations").([]any)
	if len(tolerations) != 1 {
		t.Fatal("runner scheduling must stay on the single isolated CI capacity pool")
	}
	equal(t, field(t, tolerations[0], "key"), "platform.devantler.tech/ci-runner")
	equal(t, field(t, tolerations[0], "operator"), "Equal")
	equal(t, field(t, tolerations[0], "value"), "enabled")
	equal(t, field(t, tolerations[0], "effect"), "NoSchedule")
	containers, ok := field(t, spec, "containers").([]any)
	if !ok || len(containers) != 1 {
		t.Fatal("runner must contain exactly one container")
	}
	runner := containers[0]
	equal(t, field(t, runner, "name"), "runner")
	image, ok := field(t, runner, "image").(string)
	if !ok || !strings.HasPrefix(image, "ghcr.io/actions/actions-runner@sha256:") {
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
	equal(t, field(t, controller, "spec", "values", "flags", "watchSingleNamespace"), "arc-runners")
	pool := readYAML(t, "k8s/bases/infrastructure/actions-runners/helm-release.yaml")
	equal(t, field(t, pool, "metadata", "namespace"), "arc-runners")
	secret := readYAML(t, "k8s/bases/infrastructure/actions-runners/external-secret.yaml")
	equal(t, field(t, secret, "kind"), "ExternalSecret")
	equal(t, field(t, secret, "spec", "secretStoreRef", "name"), "openbao")
	equal(t, field(t, secret, "metadata", "namespace"), "arc-runners")
	equal(t, field(t, secret, "spec", "target", "name"), field(t, pool, "spec", "values", "githubConfigSecret"))
}

func TestPlatformRuntimeAppFieldsAreMappedToARCAuthenticationKeys(t *testing.T) {
	secret := readYAML(t, "k8s/bases/infrastructure/actions-runners/external-secret.yaml")
	entries, ok := field(t, secret, "spec", "data").([]any)
	if !ok || len(entries) != 3 {
		t.Fatal("only the three App authentication keys are expected")
	}
	properties := map[string]string{
		"github_app_id":              "app_id",
		"github_app_installation_id": "installation_id",
		"github_app_private_key":     "pem",
	}
	for _, entry := range entries {
		key := field(t, entry, "secretKey").(string)
		property, ok := properties[key]
		if !ok {
			t.Fatalf("unexpected or duplicate App authentication key %q", key)
		}
		// This entry belongs to the platform runtime App. The GitHub-management
		// provider's entry is a different App and must never authenticate ARC.
		equal(t, field(t, entry, "remoteRef", "key"), "infrastructure/arc/github-app")
		equal(t, field(t, entry, "remoteRef", "property"), property)
		delete(properties, key)
	}
}

// Only literal key=value arguments are supported in these selected writes.
// Reject shell syntax and duplicate keys instead of evaluating or overwriting
// configuration while checking the credential boundary.
func parseBootstrapParameters(write, path string) (map[string]string, error) {
	arguments := strings.FieldsFunc(strings.ReplaceAll(write, "\\\n", ""), func(character rune) bool {
		return character == ' ' || character == '\t' || character == '\n'
	})
	literalPath := regexp.MustCompile(`^auth/kubernetes/role/[a-zA-Z0-9_-]+$`)
	if !literalPath.MatchString(path) || len(arguments) < 4 || arguments[0] != "bao" || arguments[1] != "write" || arguments[2] != path {
		return nil, fmt.Errorf("invalid bootstrap write header")
	}
	literal := regexp.MustCompile(`^[a-zA-Z0-9_./,:-]+$`)
	parameters := map[string]string{}
	for _, argument := range arguments[3:] {
		key, value, ok := strings.Cut(argument, "=")
		if !ok || !literal.MatchString(key) || !literal.MatchString(value) {
			return nil, fmt.Errorf("non-literal bootstrap argument %q", argument)
		}
		if _, exists := parameters[key]; exists {
			return nil, fmt.Errorf("duplicate bootstrap parameter %q", key)
		}
		parameters[key] = value
	}
	return parameters, nil
}

// A field mapping is not usable if its authentication role cannot read the
// credential. Follow the actual store/identity/bootstrap chain, recording only
// the narrowly selected configuration writes (never run the bootstrap job).
func TestAppLookupHasDedicatedReadOnlyAuthentication(t *testing.T) {
	component := "k8s/bases/infrastructure/actions-runners/"
	secret := readYAML(t, component+"external-secret.yaml")
	equal(t, field(t, secret, "spec", "secretStoreRef", "kind"), "SecretStore")
	store := readYAML(t, component+"secret-store.yaml")
	equal(t, field(t, store, "metadata", "name"), field(t, secret, "spec", "secretStoreRef", "name"))
	equal(t, field(t, store, "metadata", "namespace"), "arc-runners")
	vault := field(t, store, "spec", "provider", "vault")
	equal(t, field(t, vault, "path"), "secret")
	equal(t, field(t, vault, "version"), "v2")
	identity := field(t, vault, "auth", "kubernetes")
	equal(t, field(t, identity, "mountPath"), "kubernetes")
	role := field(t, identity, "role").(string)
	account := readYAML(t, component+"service-account.yaml")
	equal(t, field(t, account, "metadata", "name"), field(t, identity, "serviceAccountRef", "name"))
	equal(t, field(t, account, "metadata", "namespace"), "arc-runners")
	equal(t, field(t, account, "automountServiceAccountToken"), false)
	resources := field(t, readYAML(t, component+"kustomization.yaml"), "resources").([]any)
	for _, needed := range []string{"secret-store.yaml", "service-account.yaml"} {
		found := false
		for _, resource := range resources {
			found = found || resource == needed
		}
		if !found {
			t.Fatalf("credential authentication resource %s is not deployed with the pool", needed)
		}
	}
	job := readYAML(t, "k8s/bases/infrastructure/vault-config/job.yaml")
	containers := field(t, job, "spec", "template", "spec", "containers").([]any)
	command := field(t, containers[0], "command").([]any)
	script := command[len(command)-1].(string)
	roleWrites := regexp.MustCompile(`(?m)^bao write auth/kubernetes/role/`+regexp.QuoteMeta(role)+` \\\n(?:[^\n]*\\\n)*[^\n]*`).FindAllString(script, -1)
	if len(roleWrites) != 1 {
		t.Fatalf("expected one bootstrap write for auth role %s, got %d", role, len(roleWrites))
	}
	parameters, err := parseBootstrapParameters(roleWrites[0], "auth/kubernetes/role/"+role)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, parameters["bound_service_account_names"], field(t, account, "metadata", "name"))
	equal(t, parameters["bound_service_account_namespaces"], "arc-runners")
	policy := parameters["policies"]
	if policy == "" || strings.Contains(policy, ",") {
		t.Fatal("ARC identity must carry exactly one dedicated policy")
	}
	policyWrites := regexp.MustCompile(`(?m)^bao policy write `+regexp.QuoteMeta(policy)+` - <<'POLICY'\n[\s\S]*?^POLICY$`).FindAllString(script, -1)
	if len(policyWrites) != 1 {
		t.Fatalf("expected one bootstrap write for policy %s, got %d", policy, len(policyWrites))
	}
	policyLines := strings.Split(policyWrites[0], "\n")
	if len(policyLines) < 3 || policyLines[0] != "bao policy write "+policy+" - <<'POLICY'" || policyLines[len(policyLines)-1] != "POLICY" {
		t.Fatal("invalid policy write header or delimiter")
	}
	policyBody := strings.Join(policyLines[1:len(policyLines)-1], "\n")
	// The App path is sufficient; wildcard GitHub paths, writes, and other
	// infrastructure/application credentials are not part of this identity.
	access := regexp.MustCompile(`^\s*path\s+"([^"]+)"\s*\{\s*capabilities\s*=\s*\[\s*"([^"]+)"\s*\]\s*\}\s*$`).FindStringSubmatch(policyBody)
	if len(access) != 3 {
		t.Fatal("ARC policy must grant one path and one capability, with no other access")
	}
	equal(t, access[1], "secret/data/infrastructure/arc/github-app")
	equal(t, access[2], "read")
	sharedWrite := regexp.MustCompile(`(?m)^bao write auth/kubernetes/role/external-secrets \\\n(?:[^\n]*\\\n)*[^\n]*`).FindAllString(script, -1)
	if len(sharedWrite) != 1 {
		t.Fatal("shared ESO auth role must remain independently configured")
	}
	sharedParameters, err := parseBootstrapParameters(sharedWrite[0], "auth/kubernetes/role/external-secrets")
	if err != nil {
		t.Fatal(err)
	}
	if sharedParameters["policies"] == "" {
		t.Fatal("shared ESO policy bundle must remain explicitly configured")
	}
	for _, name := range strings.Split(sharedParameters["policies"], ",") {
		if name == policy || name == "infra-github-readonly" {
			t.Fatal("shared ESO identity must not acquire GitHub App access")
		}
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
	pool := readYAML(t, "k8s/bases/infrastructure/actions-runners/kustomization.yaml")
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

func TestNoDeploymentAggregateActivatesARC(t *testing.T) {
	err := filepath.WalkDir(filepath.Join(repoRoot, "k8s"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "kustomization.yaml" || strings.Contains(path, "/actions-runner-controller/") || strings.Contains(path, "/actions-runners/") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var document struct {
			Resources  []string `yaml:"resources"`
			Components []string `yaml:"components"`
		}
		if err := yaml.Unmarshal(data, &document); err != nil {
			return err
		}
		for _, reference := range append(document.Resources, document.Components...) {
			if strings.Contains(reference, "actions-runner-controller") || strings.Contains(reference, "actions-runners") {
				t.Errorf("%s activates ARC through %q", path, reference)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDNSAccessIsLimitedToTheDeclaredExternalDependencies(t *testing.T) {
	for _, path := range []string{
		"k8s/bases/infrastructure/controllers/actions-runner-controller/cilium-network-policy.yaml",
		"k8s/bases/infrastructure/actions-runners/cilium-network-policy-runner.yaml",
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
