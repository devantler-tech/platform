package arcstaging_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"text/template"
)

const runtimeProviderName = "arc-runtime-platform-app"
const acceptedWorkflow = "devantler-tech/ksail/.github/workflows/verify-ksail-arc-delivery.yaml@refs/heads/main"

func TestRunnerGroupUsesOnlyTheRuntimeAppInsideItsNamespace(t *testing.T) {
	const component = "k8s/bases/infrastructure/actions-runners/"
	config := readYAML(t, component+"provider-config.yaml")
	equal(t, field(t, config, "apiVersion"), "github.m.upbound.io/v1beta1")
	equal(t, field(t, config, "kind"), "ProviderConfig")
	equal(t, field(t, config, "metadata", "name"), runtimeProviderName)
	equal(t, field(t, config, "metadata", "namespace"), "arc-runners")
	credentials := field(t, config, "spec", "credentials")
	equal(t, field(t, credentials, "source"), "Secret")
	equal(t, field(t, credentials, "secretRef", "namespace"), "arc-runners")
	equal(t, field(t, credentials, "secretRef", "name"), "arc-github-app")
	equal(t, field(t, credentials, "secretRef", "key"), "provider-credentials")
	group := readYAML(t, component+"runner-group.yaml")
	equal(t, field(t, group, "apiVersion"), "actions.github.m.upbound.io/v1alpha1")
	equal(t, field(t, group, "kind"), "RunnerGroup")
	equal(t, field(t, group, "metadata", "name"), "platform")
	equal(t, field(t, group, "metadata", "namespace"), "arc-runners")
	if !reflect.DeepEqual(field(t, group, "spec", "managementPolicies"), []any{"Observe", "Create", "Update"}) {
		t.Fatal("runner-group ownership must exclude deletion and late initialization")
	}
	if !reflect.DeepEqual(field(t, group, "spec", "providerConfigRef"), map[string]any{"name": runtimeProviderName, "kind": "ProviderConfig"}) {
		t.Fatal("runner group must use its distinct runtime-App provider")
	}
	parameters := field(t, group, "spec", "forProvider")
	if !reflect.DeepEqual(parameters, map[string]any{
		"name": "platform", "visibility": "selected", "allowsPublicRepositories": true,
		"restrictedToWorkflows": true, "selectedRepositoryIds": []any{737584922},
		"selectedWorkflows": []any{acceptedWorkflow},
	}) {
		t.Fatal("initial access must admit only KSail's main-only delivery preflight")
	}
	resources := field(t, readYAML(t, component+"kustomization.yaml"), "resources").([]any)
	for _, required := range []string{"provider-config.yaml", "runner-group.yaml"} {
		count := 0
		for _, resource := range resources {
			if resource == required {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s must be included exactly once", required)
		}
	}
}

func TestProviderCredentialTemplatePreservesARCKeysAndEscapesTheExistingPEM(t *testing.T) {
	secret := readYAML(t, "k8s/bases/infrastructure/actions-runners/external-secret.yaml")
	configuration := field(t, secret, "spec", "target", "template")
	equal(t, field(t, configuration, "engineVersion"), "v2")
	equal(t, field(t, configuration, "mergePolicy"), "Merge")
	data := field(t, configuration, "data").(map[string]any)
	if len(data) != 1 {
		t.Fatal("template must add only the provider's JSON credential key")
	}
	source := field(t, data, "provider-credentials").(string)
	parsed, err := template.New("credentials").Option("missingkey=error").Funcs(template.FuncMap{
		"toJson": func(value any) (string, error) {
			encoded, err := json.Marshal(value)
			return string(encoded), err
		},
	}).Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, pem := range []string{"synthetic-key\nsecond-line\n", "synthetic\"key\\with\ncontrols\t"} {
		inputs := map[string]string{
			"github_app_id": "123", "github_app_installation_id": "456", "github_app_private_key": pem,
		}
		var output bytes.Buffer
		if err := parsed.Execute(&output, inputs); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(output.Bytes(), &value); err != nil {
			t.Fatalf("credential JSON cannot round-trip a PEM: %v", err)
		}
		want := map[string]any{"owner": "devantler-tech", "app_auth": []any{
			map[string]any{"id": "123", "installation_id": "456", "pem_file": pem},
		}}
		if !reflect.DeepEqual(value, want) {
			t.Fatal("provider credentials changed the existing App fields")
		}
	}
	var output bytes.Buffer
	if err := parsed.Execute(&output, map[string]string{}); err == nil || !strings.Contains(err.Error(), "github_app_") {
		t.Fatal("missing existing App fields must fail the credential template")
	}
}
