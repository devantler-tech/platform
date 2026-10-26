package emailcredentials_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestPinnedOpenBaoPreservesValuesAndVersions(t *testing.T) {
	if os.Getenv("EMAIL_CREDENTIAL_NATIVE_TEST") != "1" {
		t.Skip("real pinned OpenBao API proof runs in Linux CI")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("native container proof requires Linux host networking")
	}
	manifest, err := os.ReadFile(filepath.Join(repositoryRoot(), "k8s/bases/infrastructure/vault-config/job.yaml"))
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
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == "vault-config" && strings.HasPrefix(c.Image, "quay.io/openbao/openbao:") && strings.Contains(c.Image, "@sha256:") {
			image = c.Image
		}
	}
	if image == "" {
		t.Fatal("native test must use the declared pinned OpenBao version")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	command := exec.Command("docker", "run", "--rm", "--detach", "--network=host", "--read-only",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--memory=256m", "--cpus=1", "--pids-limit=128",
		"--tmpfs", "/tmp:rw,nosuid,size=32m", "--env", "HOME=/tmp", "--entrypoint", "bao", image,
		"server", "-dev", "-dev-root-token-id="+syntheticToken, "-dev-listen-address="+address, "-dev-no-store-token")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	id, err := command.Output()
	if err != nil || !regexp.MustCompile(`^[a-f0-9]{64}$`).Match(bytes.TrimSpace(id)) {
		t.Fatal("isolated native OpenBao container could not start")
	}
	t.Cleanup(func() {
		if err := exec.Command("docker", "rm", "--force", strings.TrimSpace(string(id))).Run(); err != nil {
			t.Error("owned native OpenBao container cleanup failed")
		}
	})
	baseURL := "http://" + address
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
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
	request := func(method, key string, payload any) (int, map[string]any) {
		t.Helper()
		var input io.Reader
		if payload != nil {
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			input = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, baseURL+"/v1/secret/data/"+key, input)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Vault-Token", syntheticToken)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("native synthetic credential request failed")
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
			t.Fatal("native credential response was invalid")
		}
		return response.StatusCode, result
	}
	current := credentials()
	for key, data := range current {
		status, _ := request(http.MethodPost, key, map[string]any{"options": map[string]int{"cas": 0}, "data": data})
		if status != http.StatusOK {
			t.Fatal("native synthetic source could not be seeded")
		}
	}
	for _, mode := range []string{"plan", "copy", "copy"} {
		out, err := runMigrationAt(t, baseURL, mode)
		if err != nil {
			t.Fatalf("native preparation or replay failed: %v %s", err, out)
		}
		for key, data := range current {
			destination := strings.Replace(key, "apps/private-mail/", "infrastructure/email/", 1)
			status, result := request(http.MethodGet, destination, nil)
			if mode == "plan" {
				if status != http.StatusNotFound {
					t.Fatal("native plan changed a destination")
				}
				continue
			}
			state, _ := result["data"].(map[string]any)
			metadata, _ := state["metadata"].(map[string]any)
			if status != http.StatusOK || !reflect.DeepEqual(state["data"], data) || metadata["version"] != float64(1) {
				t.Fatal("native copy or replay changed fields or created another version")
			}
			sourceStatus, source := request(http.MethodGet, key, nil)
			sourceState, _ := source["data"].(map[string]any)
			if sourceStatus != http.StatusOK || !reflect.DeepEqual(sourceState["data"], data) {
				t.Fatal("native copy altered the existing credential")
			}
		}
	}
}
