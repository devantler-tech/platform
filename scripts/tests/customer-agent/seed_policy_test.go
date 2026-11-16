package customeragent_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// TestNativePortalAgentSeedPolicy catches an undeployable credential chain:
// the declared ESO seed writer must create this exact key despite the app's
// more-specific read-only policy, without acquiring sibling write permission.
func TestNativePortalAgentSeedPolicy(t *testing.T) {
	if os.Getenv("CUSTOMER_AGENT_NATIVE_TEST") != "1" {
		t.Skip("set CUSTOMER_AGENT_NATIVE_TEST=1 for the isolated native ACL proof")
	}
	manifest, err := os.ReadFile("../../../k8s/bases/infrastructure/vault-config/job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct{ Name, Image string }
				}
			}
		}
	}
	if err := yaml.Unmarshal(manifest, &job); err != nil {
		t.Fatal(err)
	}
	image := ""
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name == "vault-config" && strings.HasPrefix(container.Image, "quay.io/openbao/openbao:") && strings.Contains(container.Image, "@sha256:") {
			image = container.Image
		}
	}
	if image == "" {
		t.Fatal("the ACL proof requires the declared pinned OpenBao distribution")
	}
	const rootToken = "synthetic-portal-agent-policy-root"
	id, err := exec.Command("docker", "run", "--detach", "--rm", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--memory=256m", "--pids-limit=128", "--tmpfs", "/tmp:rw,nosuid,size=32m", "--env", "HOME=/tmp",
		"--publish", "127.0.0.1::8200", "--entrypoint", "bao", image,
		"server", "-dev", "-dev-root-token-id="+rootToken, "-dev-listen-address=0.0.0.0:8200", "-dev-no-store-token").Output()
	if err != nil || !regexp.MustCompile(`^[a-f0-9]{64}$`).Match(bytes.TrimSpace(id)) {
		t.Fatal("isolated native OpenBao container could not start")
	}
	containerID := strings.TrimSpace(string(id))
	t.Cleanup(func() {
		if err := exec.Command("docker", "rm", "--force", containerID).Run(); err != nil {
			t.Error("owned native OpenBao fixture cleanup failed")
		}
	})
	port, err := exec.Command("docker", "port", containerID, "8200/tcp").Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(port)), "127.0.0.1:") {
		t.Fatal("could not resolve the ACL fixture's loopback-only endpoint")
	}
	baseURL := "http://" + strings.TrimSpace(string(port))
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := client.Get(baseURL + "/v1/sys/health")
		ready := err == nil && response.StatusCode == http.StatusOK
		if response != nil {
			_ = response.Body.Close()
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated native OpenBao did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	request := func(token, method, path string, payload any) (int, map[string]any) {
		t.Helper()
		var input io.Reader
		if payload != nil {
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			input = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, baseURL+"/v1/"+path, input)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Vault-Token", token)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("native synthetic ACL request failed")
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Error("native ACL response could not close")
			}
		}()
		result := map[string]any{}
		if response.StatusCode != http.StatusNoContent && json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) != nil {
			t.Fatal("native ACL response was invalid")
		}
		return response.StatusCode, result
	}
	for _, name := range []string{"vault-seed-write", "app-customer-identity-readonly"} {
		pattern := `(?ms)^ +bao policy write ` + regexp.QuoteMeta(name) + ` - <<'POLICY'\n(.*?)^ +POLICY\n`
		matches := regexp.MustCompile(pattern).FindAllSubmatch(manifest, -1)
		if len(matches) != 1 {
			t.Fatal("the actual declared seed or customer reader ACL is missing or ambiguous")
		}
		status, _ := request(rootToken, http.MethodPut, "sys/policies/acl/"+name, map[string]string{"policy": string(matches[0][1])})
		if status != http.StatusNoContent {
			t.Fatal("the actual declared ACL could not be installed in the pinned server")
		}
	}
	status, result := request(rootToken, http.MethodPost, "auth/token/create", map[string]any{
		"policies": []string{"vault-seed-write", "app-customer-identity-readonly"}, "no_default_policy": true, "ttl": "5m",
	})
	auth, _ := result["auth"].(map[string]any)
	token, _ := auth["client_token"].(string)
	if status != http.StatusOK || token == "" {
		t.Fatal("the synthetic seed writer could not be created")
	}
	const key = "apps/customer-identity/portal-agent-client"
	payload := map[string]any{"data": map[string]string{"client-secret": "synthetic-agent-credential"}}
	for range 2 {
		status, _ = request(token, http.MethodPost, "secret/data/"+key, payload)
		if status != http.StatusOK {
			t.Fatal("the declared seed writer cannot create and update the portal agent credential")
		}
	}
	status, _ = request(token, http.MethodGet, "secret/metadata/"+key, nil)
	if status != http.StatusOK {
		t.Fatal("the declared seed writer cannot read portal agent credential metadata")
	}
	status, _ = request(token, http.MethodPost, "secret/metadata/"+key, map[string]any{"custom_metadata": map[string]string{"fixture": "synthetic"}})
	if status != http.StatusNoContent {
		t.Fatal("the declared seed writer cannot update portal agent credential metadata")
	}
	for _, check := range []struct{ method, path string }{
		{http.MethodPost, "secret/data/" + key + "-other"},
		{http.MethodPost, "secret/data/" + key + "/child"},
		{http.MethodPost, "secret/metadata/" + key + "-other"},
		{http.MethodDelete, "secret/data/" + key},
	} {
		status, _ = request(token, check.method, check.path, payload)
		if status != http.StatusForbidden {
			t.Fatal("seed writer gained a sibling, descendant or delete permission")
		}
	}
	status, _ = request(token, http.MethodGet, "secret/data/"+key, nil)
	if status != http.StatusOK {
		t.Fatal("positive credential read failed after the native ACL refusal controls")
	}
	t.Log("native portal-agent seed grant is exact; sibling, descendant and delete refusals passed")
}
