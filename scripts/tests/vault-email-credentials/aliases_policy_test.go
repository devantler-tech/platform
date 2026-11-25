package emailcredentials_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The mail server's receiving aliases are read from their single OpenBao
// source (#5481): the policy reads exactly that key, writes nothing, and only
// the shared External Secrets role carries it.
func TestEmailAliasesPolicyIsAnExactReadOnTheSharedRole(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-config/job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	policies := regexp.MustCompile(`(?ms)^ +bao policy write infra-email-aliases-readonly - <<'POLICY'\n(.*?)^ +POLICY\n`).FindAllSubmatch(data, -1)
	if len(policies) != 1 {
		t.Fatalf("expected one aliases policy, found %d", len(policies))
	}
	var lines []string
	for _, line := range strings.Split(string(policies[0][1]), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	want := []string{`path "secret/data/infrastructure/email/aliases" {`, `capabilities = ["read"]`, `}`}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("aliases policy must read exactly the alias key; got %q", lines)
	}
	holders := 0
	for _, role := range regexp.MustCompile(`(?m)\s+policies=([^ \\\n]+)`).FindAllSubmatch(data, -1) {
		for _, policy := range strings.Split(string(role[1]), ",") {
			if policy == "infra-email-aliases-readonly" {
				holders++
			}
		}
	}
	shared := regexp.MustCompile(`(?m)bao write auth/kubernetes/role/external-secrets \\\n(?:.*\\\n)*?\s+policies=([^ \\\n]+)`).FindSubmatch(data)
	if len(shared) != 2 || !strings.Contains(","+string(shared[1])+",", ",infra-email-aliases-readonly,") || holders != 1 {
		t.Fatal("only the shared External Secrets role may carry the aliases reader")
	}
	if got := strings.Count(string(data), "infrastructure/email/aliases"); got != 1 {
		t.Fatalf("the aliases path must appear in exactly one policy, found %d", got)
	}
}
