package emailcredentials_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The one-time mailbox history copy (#5481) reads the existing provider's
// sign-in through its own role. These checks pin that the role binds exactly
// the copy's namespaced identity and that its policy reads exactly one key.
func historyImportPolicy(t *testing.T) (string, map[string]any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-config/job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	extract := func(pattern string) string {
		t.Helper()
		matches := regexp.MustCompile(pattern).FindAllSubmatch(data, -1)
		if len(matches) != 1 {
			t.Fatalf("expected exactly one declaration, found %d", len(matches))
		}
		return string(matches[0][1])
	}
	policy := extract(`(?ms)^ +bao policy write infra-icloud-readonly - <<'POLICY'\n(.*?)^ +POLICY\n`)
	roleJSON := extract(`(?ms)^ +bao write auth/kubernetes/role/email-history-import - <<'EMAIL_HISTORY_ROLE'\n(.*?)^ +EMAIL_HISTORY_ROLE\n`)
	var role map[string]any
	if err := json.Unmarshal([]byte(roleJSON), &role); err != nil {
		t.Fatal("declared history import role is not valid JSON")
	}
	return policy, role
}

func TestHistoryImportRoleHasOneNamespacedReader(t *testing.T) {
	_, role := historyImportPolicy(t)
	want := map[string]any{
		"bound_service_account_names":      []any{"email-history-import"},
		"bound_service_account_namespaces": []any{"private-mail"},
		"token_policies":                   []any{"infra-icloud-readonly"},
		"token_no_default_policy":          true,
		"token_ttl":                        float64(3600),
		"token_max_ttl":                    float64(3600),
	}
	if !reflect.DeepEqual(role, want) {
		t.Fatal("history import role must bind exactly its namespaced identity and sole reader policy")
	}
}

func TestHistoryImportPolicyReadsOnlyTheProviderSignIn(t *testing.T) {
	policy, _ := historyImportPolicy(t)
	var lines []string
	for _, line := range strings.Split(policy, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	want := []string{
		`path "secret/data/infrastructure/apple/icloud" {`,
		`capabilities = ["read"]`,
		`}`,
		`path "auth/token/lookup-self" {`,
		`capabilities = ["read"]`,
		`}`,
		`path "auth/token/revoke-self" {`,
		`capabilities = ["update"]`,
		`}`,
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("history import policy must read exactly the provider sign-in; got %q", lines)
	}
}

// The shared External Secrets role must not gain the provider sign-in: only
// the scoped role above may read it.
func TestSharedExternalSecretsRoleCannotReadProviderSignIn(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-config/job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	role := regexp.MustCompile(`(?m)bao write auth/kubernetes/role/external-secrets \\\n(?:.*\\\n)*?\s+policies=([^ \\\n]+)`).FindSubmatch(data)
	if len(role) != 2 {
		t.Fatal("shared External Secrets role is missing")
	}
	for _, policy := range strings.Split(string(role[1]), ",") {
		if policy == "infra-icloud-readonly" {
			t.Fatal("shared External Secrets role must not carry the provider sign-in reader")
		}
	}
	if got := strings.Count(string(data), "infrastructure/apple/icloud"); got != 1 {
		t.Fatalf("the provider sign-in path must appear in exactly one policy, found %d", got)
	}
}
