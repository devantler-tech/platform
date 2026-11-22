package bookkeeper_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func TestReconciledSecretRolesCannotCrossEnvironments(t *testing.T) {
	directory := captureBookkeeperReconciliation(t, collectorSecretFunction(t))
	for environment, namespace := range map[string]string{"prod": "bookkeeper", "dev": "bookkeeper-dev"} {
		var policy struct {
			Paths map[string]struct {
				Capabilities []string `json:"capabilities"`
			} `json:"path"`
		}
		readJSON(t, directory, "app-bookkeeper-"+environment+"-readonly", &policy)
		if len(policy.Paths) != 3 || !reflect.DeepEqual(policy.Paths["secret/data/apps/bookkeeper/"+environment].Capabilities, []string{"read"}) ||
			!reflect.DeepEqual(policy.Paths["auth/token/lookup-self"].Capabilities, []string{"read"}) ||
			!reflect.DeepEqual(policy.Paths["auth/token/revoke-self"].Capabilities, []string{"update"}) {
			t.Fatalf("%s policy needs only its exact environment entry and own-token lifecycle: %+v", environment, policy)
		}
		var role struct {
			Names      []string `json:"bound_service_account_names"`
			Namespaces []string `json:"bound_service_account_namespaces"`
			Policies   []string `json:"token_policies"`
			NoDefault  bool     `json:"token_no_default_policy"`
			TTL        int      `json:"token_ttl"`
			MaxTTL     int      `json:"token_max_ttl"`
		}
		readJSON(t, directory, "bookkeeper-"+environment, &role)
		if !reflect.DeepEqual(role.Names, []string{"bookkeeper-secret-reader"}) ||
			!reflect.DeepEqual(role.Namespaces, []string{namespace}) ||
			!reflect.DeepEqual(role.Policies, []string{"app-bookkeeper-" + environment + "-readonly"}) ||
			!role.NoDefault || role.TTL != 3600 || role.MaxTTL != 3600 {
			t.Fatalf("%s role grants an unexpected identity, policy or token lifetime: %+v", environment, role)
		}
	}
}

func collectorSecretFunction(t *testing.T) []byte {
	t.Helper()
	objects := render(t, "k8s/bases/infrastructure/vault-config")
	job := object(t, objects, "Job", "openbao", "vault-config")
	containers := field(t, job, "spec", "template", "spec", "containers").([]any)
	var script string
	for _, item := range containers {
		container := item.(map[string]any)
		for _, arg := range container["command"].([]any) {
			if value, ok := arg.(string); ok && strings.Contains(value, "configure_bookkeeper_secrets()") {
				script = value
			}
		}
	}
	if script == "" {
		t.Fatal("missing collector's scoped OpenBao reconciliation")
	}
	parsed, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "vault-config")
	if err != nil {
		t.Fatal(err)
	}
	var function bytes.Buffer
	syntax.Walk(parsed, func(node syntax.Node) bool {
		declaration, ok := node.(*syntax.FuncDecl)
		if !ok || declaration.Name.Value != "configure_bookkeeper_secrets" {
			return true
		}
		if err := syntax.NewPrinter().Print(&function, declaration); err != nil {
			t.Fatal(err)
		}
		return false
	})
	return function.Bytes()
}

func captureBookkeeperReconciliation(t *testing.T, function []byte) string {
	t.Helper()
	directory := t.TempDir()
	stub := "#!/bin/sh\nset -eu\nif [ \"$1\" = policy ]; then name=$3; else name=${2##*/}; fi\ncat > \"$BOOKKEEPER_CAPTURE/$name.json\"\n"
	if err := os.WriteFile(filepath.Join(directory, "bao"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "function.sh"), function, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "variables.tsv"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Use the repository's Flux substitution path before executing the actual
	// function: shell-local ${name} expressions must not become Flux variables.
	command := exec.Command("bash", "-euc", `
source "$1/scripts/flux-render.lib.sh"
die() { printf '%s\n' "$*" >&2; exit 2; }
controller_helm=''
flux_render_init "$2/render"
flux_render_substitute "$2/variables.tsv" "$2/function.sh" > "$2/resolved.sh"
source "$2/resolved.sh"
configure_bookkeeper_secrets
`, "collector-secret-test", root(), directory)
	command.Env = append(os.Environ(), "PATH="+directory+":"+os.Getenv("PATH"), "BOOKKEEPER_CAPTURE="+directory)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run actual collector secret reconciliation: %v: %s", err, out)
	}
	return directory
}

func readJSON(t *testing.T, directory, name string, value any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}
