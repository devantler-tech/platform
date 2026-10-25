package main

// Compiled only with the fixed reviewed publisher revision by the main-only
// workflow. No state machine, issue publication or operator-selected target.
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

func proofCredential(raw []byte) ([]byte, int64, error) {
	if len(raw) == 0 || len(raw) > 128*1024 {
		return nil, 0, errQuarantined
	}
	var value struct {
		Owner string `json:"owner"`
		Apps  []struct {
			ID           string `json:"id"`
			Installation string `json:"installation_id"`
			PEM          string `json:"pem_file"`
		} `json:"app_auth"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Owner != repositoryOwner || len(value.Apps) != 1 {
		return nil, 0, errQuarantined
	}
	appID, err := strconv.ParseInt(value.Apps[0].ID, 10, 64)
	if err != nil || appID != publisherAppID {
		return nil, 0, errQuarantined
	}
	installation, err := strconv.ParseInt(value.Apps[0].Installation, 10, 64)
	if err != nil || installation <= 0 || len(value.Apps[0].PEM) == 0 || len(value.Apps[0].PEM) > 64*1024 {
		return nil, 0, errQuarantined
	}
	return []byte(value.Apps[0].PEM), installation, nil
}

func TestProofCredential(t *testing.T) {
	key, installation, err := proofCredential([]byte(`{"owner":"devantler-tech","app_auth":[{"id":"4085939","installation_id":"9","pem_file":"fixture-only"}]}`))
	if err != nil || installation != 9 || string(key) != "fixture-only" {
		t.Fatal("valid publisher credential rejected")
	}
	for _, raw := range []string{
		`{"owner":"another-owner","app_auth":[{"id":"4085939","installation_id":"9","pem_file":"fixture-only"}]}`,
		`{"owner":"devantler-tech","app_auth":[{"id":"1","installation_id":"9","pem_file":"fixture-only"}]}`,
		`{"owner":"devantler-tech","app_auth":[{"id":"4085939","installation_id":"0","pem_file":"fixture-only"}]}`,
		`{"owner":"devantler-tech","app_auth":[{"id":"4085939","installation_id":"9","pem_file":""}]}`,
		`{"owner":"devantler-tech","app_auth":[]}`,
		`{"owner":"devantler-tech","app_auth":[{"id":"4085939","installation_id":"9","pem_file":"fixture-only","target":"another"}]}`,
		`{"owner":"devantler-tech","app_auth":[{"id":"4085939","installation_id":"9","pem_file":"fixture-only"}]} {}`,
		strings.Repeat("x", 1<<18),
	} {
		if key, _, err := proofCredential([]byte(raw)); err == nil || key != nil {
			t.Fatal("invalid publisher credential admitted")
		}
	}
}

func TestCredentialedPublisherReadOnly(t *testing.T) {
	file, err := os.Open(os.Getenv("ALERT_APP_PROOF_CREDENTIAL_FILE"))
	if err != nil {
		t.Fatal("proof credential unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 128*1024+1))
	_ = file.Close()
	if err != nil {
		t.Fatal("proof credential invalid")
	}
	defer clear(raw)
	key, expectedInstallation, err := proofCredential(raw)
	if err != nil {
		t.Fatal("proof credential invalid")
	}
	defer clear(key)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	defer base.CloseIdleConnections()
	appTransport, err := ghinstallation.NewAppsTransport(base, publisherAppID, key)
	if err != nil {
		t.Fatal("proof credential rejected")
	}
	appClient, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: appTransport, Timeout: 10 * time.Second, CheckRedirect: noRedirect}))
	if err != nil {
		t.Fatal("proof client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	installation, _, err := appClient.Apps.GetRepositoryInstallation(ctx, repositoryOwner, repositoryName)
	if err != nil || installation.GetID() != expectedInstallation || installation.GetAppID() != publisherAppID || installation.GetAppSlug() != publisherAppSlug || installation.GetAccount().GetLogin() != repositoryOwner {
		t.Fatal("existing credential does not establish the required publisher identity")
	}
	remote, err := newGitHubRemote(installation.GetID(), key)
	if err != nil {
		t.Fatal("publisher client unavailable")
	}
	scoped, ok := remote.client.Client().Transport.(*scopedTransport)
	if !ok {
		t.Fatal("publisher scope enforcement unavailable")
	}
	// Capture only the token this invocation mints, so cleanup cannot revoke a
	// shared credential or implicitly mint another token after a failed check.
	token, err := scoped.installation.Token(ctx)
	if err != nil || token == "" {
		t.Fatal("publisher scoped token unavailable")
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		request, requestErr := http.NewRequestWithContext(cleanupCtx, http.MethodDelete, "https://api.github.com/installation/token", nil)
		if requestErr != nil {
			t.Error("probe token revocation unconfirmed")
			return
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", "application/vnd.github+json")
		client := &http.Client{Transport: base, Timeout: 10 * time.Second, CheckRedirect: noRedirect}
		response, cleanupErr := client.Do(request)
		if response != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
		}
		if cleanupErr != nil || response == nil || response.StatusCode != http.StatusNoContent {
			t.Error("probe token revocation unconfirmed")
			return
		}
		t.Log("probe-owned token revocation confirmed")
	}()
	// Exercise the production scope gate and identity read using that same token.
	if err := remote.verify(ctx); err != nil || !remote.verified {
		t.Fatal("publisher identity or token boundary verification failed")
	}
	t.Log("publisher App identity, repository and scoped token boundary verified")
}
