package arcstaging_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func TestCredentialReaderIsSeparateFromJobRunners(t *testing.T) {
	component := "k8s/bases/infrastructure/actions-runners/"
	store := readYAML(t, component+"credentials/secret-store.yaml")
	equal(t, field(t, store, "kind"), "SecretStore")
	equal(t, field(t, store, "metadata", "name"), "openbao")
	equal(t, field(t, store, "metadata", "namespace"), "arc-runners")
	vault := field(t, store, "spec", "provider", "vault")
	equal(t, field(t, vault, "path"), "secret")
	equal(t, field(t, vault, "version"), "v2")
	auth := field(t, vault, "auth", "kubernetes")
	equal(t, field(t, auth, "mountPath"), "kubernetes")
	equal(t, field(t, auth, "role"), "arc-secret-reader")
	equal(t, field(t, auth, "serviceAccountRef", "name"), "arc-secret-reader")
	account := readYAML(t, component+"credentials/service-account.yaml")
	equal(t, field(t, account, "metadata", "name"), "arc-secret-reader")
	equal(t, field(t, account, "metadata", "namespace"), "arc-runners")
	equal(t, field(t, account, "automountServiceAccountToken"), false)
	resources := field(t, readYAML(t, component+"credentials/kustomization.yaml"), "resources").([]any)
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

// Inspect authorization writes as syntax data; neither this helper nor the
// parser executes any bootstrap statement or calls OpenBao.
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
	args, payload, err := parseBaoWrite(source, prefix...)
	if err != nil {
		t.Fatal(err)
	}
	return args, payload
}

func parseBaoWrite(source string, prefix ...string) ([]string, string, error) {
	parsed, err := syntax.NewParser().Parse(strings.NewReader(source), "vault-config")
	if err != nil {
		return nil, "", fmt.Errorf("parse bootstrap: %w", err)
	}
	var matches []*syntax.Stmt
	var invalid error
	matchesPrefix := func(call *syntax.CallExpr) bool {
		if len(call.Args) < len(prefix) {
			return false
		}
		for index, expected := range prefix {
			if call.Args[index].Lit() != expected {
				return false
			}
		}
		return true
	}
	syntax.Walk(parsed, func(node syntax.Node) bool {
		if binary, ok := node.(*syntax.BinaryCmd); ok {
			syntax.Walk(binary, func(child syntax.Node) bool {
				if call, ok := child.(*syntax.CallExpr); ok && matchesPrefix(call) {
					invalid = fmt.Errorf("authorization write must not be a pipeline or conditional command")
				}
				return true
			})
			return false
		}
		statement, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		call, ok := statement.Cmd.(*syntax.CallExpr)
		if !ok || !matchesPrefix(call) {
			return true
		}
		for _, argument := range call.Args {
			if argument.Lit() == "" {
				invalid = fmt.Errorf("authorization write must use literal arguments")
				return false
			}
		}
		if len(call.Assigns) != 0 || statement.Negated || statement.Background || len(statement.Redirs) > 1 {
			invalid = fmt.Errorf("authorization write must be one unmodified command")
			return false
		}
		for _, redirect := range statement.Redirs {
			if redirect.Op != syntax.Hdoc || redirect.Hdoc == nil || redirect.Hdoc.Lit() == "" {
				invalid = fmt.Errorf("authorization payload must be a literal heredoc")
				return false
			}
		}
		matches = append(matches, statement)
		return false
	})
	if invalid != nil {
		return nil, "", invalid
	}
	if len(matches) != 1 {
		return nil, "", fmt.Errorf("expected one authorization write %v, got %d", prefix, len(matches))
	}
	var args []string
	for _, argument := range matches[0].Cmd.(*syntax.CallExpr).Args[1:] {
		args = append(args, argument.Lit())
	}
	var payload string
	for _, redirect := range matches[0].Redirs {
		payload = redirect.Hdoc.Lit()
	}
	return args, payload, nil
}

func TestOpenBaoReaderCanReadOnlyTheExistingApp(t *testing.T) {
	_, policy := captureBaoWrite(t, "bao", "policy", "write", "infra-arc-app-readonly")
	want := `path "secret/data/infrastructure/arc/github-app" { capabilities = ["read"] }`
	if !reflect.DeepEqual(strings.Fields(policy), strings.Fields(want)) {
		t.Fatalf("ARC reader policy must grant only the existing App path, got %q", policy)
	}
	args, stdin := captureBaoWrite(t, "bao", "write", "auth/kubernetes/role/arc-secret-reader")
	wantArgs := []string{"write", "auth/kubernetes/role/arc-secret-reader",
		"bound_service_account_names=arc-secret-reader", "bound_service_account_namespaces=arc-runners",
		"policies=infra-arc-app-readonly", "ttl=1h"}
	if !reflect.DeepEqual(args, wantArgs) || stdin != "" {
		t.Fatalf("ARC role must bind only its credential reader: args=%v stdin=%q", args, stdin)
	}
	shared, _ := captureBaoWrite(t, "bao", "write", "auth/kubernetes/role/external-secrets")
	for _, argument := range shared {
		if strings.HasPrefix(argument, "policies=") && (strings.Contains(argument, "github") || strings.Contains(argument, "arc")) {
			t.Fatal("the shared ESO role must not gain GitHub App access")
		}
	}
}
