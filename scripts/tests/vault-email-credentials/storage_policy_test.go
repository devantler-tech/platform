package emailcredentials_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func emailStoragePolicy(t *testing.T) (string, map[string]any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-config/job.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	extract := func(pattern string) string {
		t.Helper()
		match := regexp.MustCompile(pattern).FindSubmatch(data)
		if len(match) != 2 {
			t.Fatal("declared Email key policy or role is missing")
		}
		return string(match[1])
	}
	policy := extract(`(?ms)^ +bao policy write infra-email-storage-readonly - <<'POLICY'\n(.*?)^ +POLICY\n`)
	roleJSON := extract(`(?ms)^ +bao write auth/kubernetes/role/email-storage - <<'EMAIL_STORAGE_ROLE'\n(.*?)^ +EMAIL_STORAGE_ROLE\n`)
	var role map[string]any
	if err := json.Unmarshal([]byte(roleJSON), &role); err != nil {
		t.Fatal("declared storage role is not valid JSON")
	}
	return policy, role
}

func TestEmailStorageRoleHasOneNamespacedReader(t *testing.T) {
	_, role := emailStoragePolicy(t)
	want := map[string]any{
		"bound_service_account_names":      []any{"private-mail"},
		"bound_service_account_namespaces": []any{"private-mail"},
		"token_policies":                   []any{"infra-email-storage-readonly"},
		"token_no_default_policy":          true,
		"token_ttl":                        float64(3600),
		"token_max_ttl":                    float64(3600),
	}
	if !reflect.DeepEqual(role, want) {
		t.Fatal("Email storage role must bind exactly its namespaced identity and sole bounded reader policy")
	}
}

// verifyEmailStoragePolicy exercises the declared ACL on the same isolated pinned
// OpenBao used by the credential migration test; it never contacts production.
func verifyEmailStoragePolicy(t *testing.T, baseURL string, client *http.Client) {
	t.Helper()
	policy, _ := emailStoragePolicy(t)
	request := func(token, method, path string, payload any) (int, map[string]any) {
		t.Helper()
		var body io.Reader
		if payload != nil {
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequest(method, baseURL+"/v1/"+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Vault-Token", token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("isolated storage-policy request failed")
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Error("isolated storage-policy response could not close")
			}
		}()
		result := map[string]any{}
		if response.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
				t.Fatal("isolated storage-policy response was invalid")
			}
		}
		return response.StatusCode, result
	}
	status, _ := request(syntheticToken, http.MethodPut, "sys/policies/acl/infra-email-storage-readonly", map[string]any{"policy": policy})
	if status != http.StatusNoContent {
		t.Fatal("declared Email storage ACL was rejected by the pinned server")
	}
	status, _ = request(syntheticToken, http.MethodPost, "secret/data/infrastructure/email/storage", map[string]any{"data": map[string]string{"passphrase": "synthetic-storage-key-only"}})
	if status != http.StatusOK {
		t.Fatal("synthetic storage key could not be seeded")
	}
	status, tokenResult := request(syntheticToken, http.MethodPost, "auth/token/create", map[string]any{"policies": []string{"infra-email-storage-readonly"}, "no_default_policy": true, "ttl": "5m"})
	auth, _ := tokenResult["auth"].(map[string]any)
	token, _ := auth["client_token"].(string)
	if status != http.StatusOK || token == "" {
		t.Fatal("isolated storage reader could not be created")
	}
	status, result := request(token, http.MethodGet, "secret/data/infrastructure/email/storage", nil)
	outer, _ := result["data"].(map[string]any)
	if status != http.StatusOK || !reflect.DeepEqual(outer["data"], map[string]any{"passphrase": "synthetic-storage-key-only"}) {
		t.Fatal("storage reader could not retrieve the exact synthetic service key")
	}
	for _, check := range []struct{ method, path string }{
		{http.MethodPost, "secret/data/infrastructure/email/storage"},
		{http.MethodDelete, "secret/data/infrastructure/email/storage"},
		{http.MethodGet, "secret/data/infrastructure/email/admin"},
		{http.MethodGet, "secret/data/apps/private-mail/admin"},
		{http.MethodGet, "secret/data/infrastructure/email/storage/other"},
		{http.MethodGet, "secret/metadata/infrastructure/email/storage"},
		{"LIST", "secret/metadata/infrastructure/email"},
		{http.MethodGet, "sys/mounts"},
	} {
		status, _ = request(token, check.method, check.path, map[string]any{"data": map[string]string{"passphrase": "must-not-write"}})
		if status != http.StatusForbidden {
			t.Fatalf("storage reader did not refuse %s %s", check.method, strings.TrimPrefix(check.path, "secret/"))
		}
	}
}
