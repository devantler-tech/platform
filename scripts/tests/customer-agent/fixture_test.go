package customeragent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

type nativeFixture struct {
	client  *http.Client
	baseURL string
}

func nativeDockerOutput(t *testing.T, args ...string) []byte {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "docker", args...).Output()
	if err != nil {
		t.Fatalf("owned native fixture Docker operation %s failed: %v", args[0], err)
	}
	return output
}

func ownedNativeContainer(t *testing.T, args ...string) string {
	t.Helper()
	id := bytes.TrimSpace(nativeDockerOutput(t, args...))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).Match(id) {
		t.Fatal("owned native fixture did not return a container ID")
	}
	containerID := string(id)
	t.Cleanup(func() {
		// The test context is cancelled before cleanups; cleanup needs its own bound.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, "docker", "rm", "--force", containerID).Run(); err != nil {
			t.Error("owned native fixture container cleanup failed")
		}
	})
	return containerID
}

func nativeFixtureEndpoint(t *testing.T, containerID, port string) nativeFixture {
	t.Helper()
	endpoint := strings.TrimSpace(string(nativeDockerOutput(t, "port", containerID, port)))
	if !strings.HasPrefix(endpoint, "127.0.0.1:") {
		t.Fatal("could not resolve the fixture's loopback-only endpoint")
	}
	return nativeFixture{
		baseURL: "http://" + endpoint,
		client: &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

func (fixture nativeFixture) get(t *testing.T, path string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fixture.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fixture.client.Do(request)
}

func (fixture nativeFixture) waitReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := fixture.get(t, path)
		ready := err == nil && response.StatusCode == http.StatusOK
		if response != nil {
			_ = response.Body.Close()
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated native fixture did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (fixture nativeFixture) jsonRequest(t *testing.T, token, method, path string, payload any) (int, map[string]any) {
	t.Helper()
	var input io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, fixture.baseURL+"/v1/"+path, input)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Vault-Token", token)
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatal("native synthetic ACL request failed")
	}
	defer closeFixtureResponse(t, response)
	result := map[string]any{}
	if response.StatusCode != http.StatusNoContent && json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) != nil {
		t.Fatal("native ACL response was invalid")
	}
	return response.StatusCode, result
}

func closeFixtureResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Error("native fixture response could not close")
	}
}
