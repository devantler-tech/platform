package readonly_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Exercise the actual proxy rule parser and request routing, not a second
// implementation of its expression language. CI uses the deployment's pinned
// image. A verified release binary can run the same proof without Docker.
func TestReadOnlyRequestsAgainstNativeTraefik(t *testing.T) {
	if os.Getenv("LONGHORN_UI_NATIVE_TEST") != "1" {
		t.Skip("set LONGHORN_UI_NATIVE_TEST=1 for the native proxy proof")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-HTTP-Method-Override") != "" || r.Header.Get("X-Method-Override") != "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Header.Get("X-Forwarded-Proto") != "https" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Longhorn-Read-Proof", "upstream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	config := authConfig(t, render(t, "hetzner"))
	field(config, "http", "services", "longhorn", "loadBalancer", "servers").([]any)[0].(object)["url"] = upstream.URL
	dynamic, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "dynamic.yaml"), bytes.ReplaceAll(dynamic, []byte("${domain}"), []byte("example.invalid")), 0o644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	native := os.Getenv("TRAEFIK_BINARY")
	configPath := filepath.Join(dir, "dynamic.yaml")
	if native == "" {
		configPath = "/config/dynamic.yaml"
	}
	static := fmt.Sprintf("entryPoints:\n  web:\n    address: %q\nproviders:\n  file:\n    filename: %q\nlog:\n  level: ERROR\n", address, configPath)
	if err = os.WriteFile(filepath.Join(dir, "static.yaml"), []byte(static), 0o644); err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	containerIDFile := filepath.Join(dir, "container-id")
	if native != "" {
		cmd = exec.CommandContext(ctx, native, "--configFile="+filepath.Join(dir, "static.yaml"))
	} else {
		data, readErr := os.ReadFile(filepath.Join(root(t), "k8s/bases/infrastructure/controllers/auth-proxy/deployment.yaml"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var deployment object
		if err = yaml.Unmarshal(data, &deployment); err != nil {
			t.Fatal(err)
		}
		image := list(deployment, "spec", "template", "spec", "containers")[0].(object)["image"].(string)
		if !strings.Contains(image, "@sha256:") {
			t.Fatal("native proxy proof requires the deployment's immutable image")
		}
		cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "--cidfile="+containerIDFile, "--network=host", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user=65532:65532", "--volume="+dir+":/config:ro", "--entrypoint=traefik", image, "--configFile=/config/static.yaml")
	}
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		// Killing the Docker client does not stop its container. Retire only
		// the exact ID this invocation created, then require an empty readback.
		if native == "" {
			data, readErr := os.ReadFile(containerIDFile)
			if readErr == nil {
				id := strings.TrimSpace(string(data))
				if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
					t.Error("native proxy container identity is invalid")
					return
				}
				cleanupCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				_ = exec.CommandContext(cleanupCtx, "docker", "rm", "--force", id).Run()
				left, listErr := exec.CommandContext(cleanupCtx, "docker", "container", "ls", "--all", "--filter=id="+id, "--format={{.ID}}").Output()
				if listErr != nil || len(bytes.TrimSpace(left)) != 0 {
					t.Error("native proxy container cleanup was not verified")
				}
			} else if !os.IsNotExist(readErr) {
				t.Error("native proxy container identity could not be read")
			}
		}
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	client := &http.Client{Timeout: 2 * time.Second}
	base := "http://" + address
	request := func(method, path string) (int, string, error) {
		req, requestErr := http.NewRequestWithContext(ctx, method, base+path, nil)
		if requestErr != nil {
			return 0, "", requestErr
		}
		req.Host = "longhorn.example.invalid"
		req.Header.Set("X-HTTP-Method-Override", "DELETE")
		req.Header.Set("X-Method-Override", "PUT")
		response, requestErr := client.Do(req)
		if requestErr != nil {
			return 0, "", requestErr
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode, response.Header.Get("X-Longhorn-Read-Proof"), nil
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, _, probeErr := request("GET", "/")
		if probeErr == nil && status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native proxy failed to serve its rendered rule: status=%d error=%v", status, probeErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if native == "" {
		id, readErr := os.ReadFile(containerIDFile)
		if readErr != nil || !regexp.MustCompile(`^[0-9a-f]{64}\n?$`).Match(id) {
			t.Fatal("native proxy proof lost its container identity")
		}
		// The reviewed production observer uses only this existing image's
		// shell and netcat; prove those tools rather than installing anything
		// in a serving Pod or assuming an image has them.
		if err = exec.CommandContext(ctx, "docker", "exec", strings.TrimSpace(string(id)), "/bin/sh", "-c", "command -v nc").Run(); err != nil {
			t.Fatal("deployed proxy image lacks the read-only observer's netcat")
		}
	}
	for _, fixture := range requests {
		t.Run(fixture.method+fixture.path, func(t *testing.T) {
			before := calls.Load()
			status, marker, requestErr := request(fixture.method, fixture.path)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			// Traefik QueryRegexp accepts any matching duplicate query value;
			// the manager chooses the first. This case MUST be refused by both
			// exact-query Cilium boundaries (tested separately), never treated as
			// authorized by this preliminary routing match.
			proxyRoutes := fixture.allowed || strings.Contains(fixture.path, "&action=")
			if proxyRoutes {
				if status != http.StatusOK || marker != "upstream" || calls.Load() != before+1 {
					t.Fatalf("read did not reach upstream: %d %q", status, marker)
				}
			} else if status != http.StatusNotFound || marker != "" || calls.Load() != before {
				t.Fatalf("administrative request reached upstream: status=%d marker=%q calls=%d", status, marker, calls.Load()-before)
			}
		})
	}
}
