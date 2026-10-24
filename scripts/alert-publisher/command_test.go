package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

type forbiddenReader struct{}

func (forbiddenReader) Read([]byte) (int, error) { panic("disabled command read input") }

func TestDisabledCommandNeedsNoCredentialOrNetwork(t *testing.T) {
	var output bytes.Buffer
	code := command(nil, forbiddenReader{}, &output, func(int64, []byte) (githubAPI, error) {
		t.Fatal("disabled publisher loaded credentials")
		return nil, errQuarantined
	})
	if code != 0 || !strings.Contains(output.String(), "disabled") {
		t.Fatal("publisher enabled by default")
	}
}
func TestInvocationAndInputErrorsNeverEchoCanaries(t *testing.T) {
	for _, args := range [][]string{{"--private-canary"}, {"--installation-id", "private-canary"}, {"--enabled", "--state-dir", "/state", "--key-file", "/missing", "--installation-id", "17"}} {
		var output bytes.Buffer
		code := command(args, strings.NewReader(`{"route":"private-canary"}`), &output, func(int64, []byte) (githubAPI, error) {
			t.Fatal("unsafe input reached credential factory")
			return nil, errQuarantined
		})
		if code != 2 || strings.Contains(output.String(), "private-canary") {
			t.Fatal("unsafe invocation was accepted or echoed")
		}
	}
}

// This forwarder exists only in the test binary. Production keeps its fixed
// HTTPS origin and exposes no URL override. Test requests traverse the actual
// SDK, issuer and HTTP client before reaching the local fake server.
type fixtureForwarder struct {
	target *url.URL
	base   http.RoundTripper
}

func (f fixtureForwarder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "api.github.com" {
		return nil, errQuarantined
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = f.target.Scheme
	clone.URL.Host = f.target.Host
	clone.Host = f.target.Host
	clone.RequestURI = ""
	return f.base.RoundTrip(clone)
}
func processRemote(t *testing.T, address string, key []byte) *githubRemote {
	t.Helper()
	target, err := url.Parse(address)
	if err != nil || target.Hostname() != "127.0.0.1" {
		t.Fatal("invalid fixture server")
	}
	base := &http.Transport{Proxy: nil}
	forwarder := fixtureForwarder{target: target, base: base}
	transport, err := ghinstallation.New(forwarder, publisherAppID, 17, key)
	if err != nil {
		t.Fatal(err)
	}
	transport.Client = &http.Client{Transport: forwarder, Timeout: 3 * time.Second, CheckRedirect: noRedirect}
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{RepositoryIDs: []int64{repositoryID}, Permissions: &github.InstallationPermissions{Issues: github.Ptr("write")}}
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: &scopedTransport{installation: transport, base: forwarder}, Timeout: 3 * time.Second, CheckRedirect: noRedirect}))
	if err != nil {
		t.Fatal(err)
	}
	return &githubRemote{client: client, now: time.Now}
}

func TestPublisherProcessHelper(t *testing.T) {
	if os.Getenv("ALERT_PUBLISHER_TEST_HELPER") != "1" {
		return
	}
	code := command([]string{"--enabled", "--state-dir", os.Getenv("ALERT_PUBLISHER_TEST_STATE"), "--key-file", os.Getenv("ALERT_PUBLISHER_TEST_KEY"), "--installation-id", "17"}, os.Stdin, os.Stderr, func(id int64, key []byte) (githubAPI, error) {
		if id != 17 {
			return nil, errQuarantined
		}
		return processRemote(t, os.Getenv("ALERT_PUBLISHER_TEST_SERVER"), key), nil
	})
	os.Exit(code)
}

func TestSeparateProcessesShareOneDurableIncident(t *testing.T) {
	fake := &fakeGitHubHTTP{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = "https"
		clone.URL.Host = "api.github.com"
		response, err := fake.RoundTrip(clone)
		if err != nil {
			http.Error(w, "fixture unavailable", 500)
			return
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Errorf("close fake response: %v", err)
			}
		}()
		for key, values := range response.Header {
			w.Header()[key] = values
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer server.Close()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "fixture.pem")
	if err := os.WriteFile(keyPath, testKey(t), 0400); err != nil {
		t.Fatal(err)
	}
	obs, err := json.Marshal(observation{Route: "cnpg-degraded", State: "firing", ObservedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPublisherProcessHelper$")
			cmd.Env = append(os.Environ(), "ALERT_PUBLISHER_TEST_HELPER=1", "ALERT_PUBLISHER_TEST_STATE="+filepath.Join(dir, "state"), "ALERT_PUBLISHER_TEST_KEY="+keyPath, "ALERT_PUBLISHER_TEST_SERVER="+server.URL)
			cmd.Stdin = bytes.NewReader(obs)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("fixture child failed: %v: %s", err, output)
			}
		}()
	}
	wg.Wait()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.posts != 1 || len(fake.issues) != 1 || fake.unsafeRequest {
		t.Fatal("separate processes duplicated or misrouted the incident")
	}
	s, err := readJournal(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Receipts) != 1 || len(s.Pending) != 0 {
		t.Fatal("separate processes lost the delivery receipt")
	}
}
