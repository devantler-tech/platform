package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func credentialSecret(t *testing.T, config string, change func(map[string]any)) []byte {
	t.Helper()
	v := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "ghcr-auth", "namespace": "kyverno", "uid": "credential-uid", "resourceVersion": "42"}, "type": "kubernetes.io/dockerconfigjson", "data": map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString([]byte(config))}}
	if change != nil {
		change(v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRegistryPrecheckUsesActualKyvernoCredential(t *testing.T) {
	for _, config := range []string{
		`{"auths":{"ghcr.io":{"username":"probe","password":"private-test-token"}}}`,
		`{"auths":{"ghcr.io":{"auth":"cHJvYmU6cHJpdmF0ZS10ZXN0LXRva2Vu"}}}`,
	} {
		username, password, err := registryCredential(credentialSecret(t, config, nil))
		if err != nil || username != "probe" || password != "private-test-token" {
			t.Fatal("authoritative credential was not resolved")
		}
	}
}

func TestAmbiguousOrForeignCredentialCannotQualifyUnsignedControl(t *testing.T) {
	for _, config := range []string{
		`{"auths":{"other.example":{"username":"probe","password":"private-test-token"}}}`,
		`{"auths":{"ghcr.io":{"username":"probe","password":"private-test-token","auth":"b3RoZXI6dG9rZW4="}}}`,
		`{"auths":{"ghcr.io":{"username":"probe","password":""}}}`,
		`{"auths":{"ghcr.io":{"username":"probe","password":"private-test-token","password":"changed"}}}`,
		`{"auths":{"ghcr.io":{"username":"probe","password":"private-test-token"}}} {}`,
	} {
		if _, _, err := registryCredential(credentialSecret(t, config, nil)); err == nil {
			t.Fatal("unusable credential was accepted")
		}
	}
	valid := `{"auths":{"ghcr.io":{"username":"probe","password":"private-test-token"}}}`
	for _, change := range []func(map[string]any){
		func(v map[string]any) { v["metadata"].(map[string]any)["namespace"] = "other" },
		func(v map[string]any) { v["metadata"].(map[string]any)["uid"] = "" },
		func(v map[string]any) { v["kind"] = "ConfigMap" },
		func(v map[string]any) { v["type"] = "Opaque" },
	} {
		if _, _, err := registryCredential(credentialSecret(t, valid, change)); err == nil {
			t.Fatal("foreign credential binding was accepted")
		}
	}
}
