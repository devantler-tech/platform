package arcstaging_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func TestCredentialReaderIsSeparateFromJobRunners(t *testing.T) {
	component := "k8s/bases/infrastructure/ksail-analysis-runners/"
	store := readYAML(t, component+"secret-store.yaml")
	equal(t, field(t, store, "kind"), "SecretStore")
	equal(t, field(t, store, "metadata", "name"), "openbao")
	equal(t, field(t, store, "metadata", "namespace"), "arc-ksail-analysis")
	vault := field(t, store, "spec", "provider", "vault")
	equal(t, field(t, vault, "path"), "secret")
	equal(t, field(t, vault, "version"), "v2")
	auth := field(t, vault, "auth", "kubernetes")
	equal(t, field(t, auth, "mountPath"), "kubernetes")
	equal(t, field(t, auth, "role"), "arc-ksail-app")
	equal(t, field(t, auth, "serviceAccountRef", "name"), "arc-ksail-app")
	account := readYAML(t, component+"service-account.yaml")
	equal(t, field(t, account, "metadata", "name"), "arc-ksail-app")
	equal(t, field(t, account, "metadata", "namespace"), "arc-ksail-analysis")
	equal(t, field(t, account, "automountServiceAccountToken"), false)
	resources := field(t, readYAML(t, component+"kustomization.yaml"), "resources").([]any)
	for _, required := range []string{"secret-store.yaml", "service-account.yaml"} {
		found := false
		for _, resource := range resources {
			found = found || resource == required
		}
		if !found {
			t.Fatalf("credential reader resource %s is not included in the pool", required)
		}
	}
}

// Exercise only the literal authorization writes from the bootstrap script.
// The fake bao captures their actual arguments and stdin without running the
// credential seeding, initialization or any live OpenBao operation.
func captureBaoWrite(t *testing.T, prefix ...string) ([]string, string) {
	t.Helper()
	job := readYAML(t, "k8s/bases/infrastructure/vault-config/job.yaml")
	containers := field(t, job, "spec", "template", "spec", "containers").([]any)
	var source string
	for _, container := range containers {
		if field(t, container, "name") == "vault-config" {
			command := field(t, container, "command").([]any)
			source = command[len(command)-1].(string)
		}
	}
	parsed, err := syntax.NewParser().Parse(strings.NewReader(source), "vault-config")
	if err != nil {
		t.Fatal(err)
	}
	var matches []*syntax.Stmt
	syntax.Walk(parsed, func(node syntax.Node) bool {
		statement, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		call, ok := statement.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) < len(prefix) {
			return true
		}
		for index, expected := range prefix {
			if call.Args[index].Lit() != expected {
				return true
			}
		}
		for _, argument := range call.Args {
			if argument.Lit() == "" {
				t.Fatal("authorization write must use literal arguments")
			}
		}
		for _, redirect := range statement.Redirs {
			if redirect.Op != syntax.Hdoc || redirect.Hdoc == nil || redirect.Hdoc.Lit() == "" {
				t.Fatal("authorization payload must be a literal heredoc")
			}
		}
		matches = append(matches, statement)
		return false
	})
	if len(matches) != 1 {
		t.Fatalf("expected one authorization write %v, got %d", prefix, len(matches))
	}
	var script bytes.Buffer
	if err := syntax.NewPrinter().Print(&script, matches[0]); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" >\"$ARC_CAPTURE_ARGS\"\n/bin/cat >\"$ARC_CAPTURE_STDIN\"\n"
	if err := os.WriteFile(filepath.Join(directory, "bao"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(directory, "args")
	stdinFile := filepath.Join(directory, "stdin")
	command := exec.Command("/bin/sh", "-ec", script.String())
	command.Env = []string{"PATH=" + directory, "ARC_CAPTURE_ARGS=" + argsFile, "ARC_CAPTURE_STDIN=" + stdinFile}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("execute authorization write: %v: %s", err, output)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"), string(stdin)
}

func TestOpenBaoReaderCanReadOnlyTheExistingApp(t *testing.T) {
	_, policy := captureBaoWrite(t, "bao", "policy", "write", "arc-ksail-app-readonly")
	want := `path "secret/data/infrastructure/github/app" { capabilities = ["read"] }`
	if !reflect.DeepEqual(strings.Fields(policy), strings.Fields(want)) {
		t.Fatalf("ARC reader policy must grant only the existing App path, got %q", policy)
	}
	args, stdin := captureBaoWrite(t, "bao", "write", "auth/kubernetes/role/arc-ksail-app")
	wantArgs := []string{"write", "auth/kubernetes/role/arc-ksail-app",
		"bound_service_account_names=arc-ksail-app", "bound_service_account_namespaces=arc-ksail-analysis",
		"policies=arc-ksail-app-readonly", "ttl=1h"}
	if !reflect.DeepEqual(args, wantArgs) || stdin != "" {
		t.Fatalf("ARC role must bind only its credential reader: args=%v stdin=%q", args, stdin)
	}
	shared, _ := captureBaoWrite(t, "bao", "write", "auth/kubernetes/role/external-secrets")
	for _, argument := range shared {
		if strings.HasPrefix(argument, "policies=") && (strings.Contains(argument, "github") || strings.Contains(argument, "arc-ksail")) {
			t.Fatal("the shared ESO role must not gain GitHub App access")
		}
	}
}
