// Command verify-github-broker-apps proves, before the one-off seed workflow
// (.github/workflows/seed-github-broker-apps.yaml) touches the cluster, that each
// organization App credential it is about to copy into OpenBao for the GitHub
// token broker belongs to the App it is named for: the App ID is the reviewed one,
// and a JWT signed with the key authenticates as that App (GET /app).
//
// It reads the credentials from the environment and prints one status line,
// never key material. Retire it with the seed workflow.
package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"
)

// reviewedApp is one App the broker serves, with the identity GitHub reports for it.
type reviewedApp struct {
	slug, appID, envPrefix string
	id                     int64
}

// The broker's Apps. A seed for any other App ID is refused.
var reviewedApps = []reviewedApp{
	{slug: "botantler-1", appID: "1025824", id: 1025824, envPrefix: "BOTANTLER_1"},
	{slug: "botantler-2", appID: "4126012", id: 4126012, envPrefix: "BOTANTLER_2"},
}

const (
	githubAPI   = "https://api.github.com"
	maxResponse = 1 << 20
)

var clientIDPattern = regexp.MustCompile(`^Iv[0-9A-Za-z.]{1,80}$`)

func main() {
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	os.Exit(run(context.Background(), os.Getenv, client, githubAPI, time.Now, os.Stdout))
}

// run verifies every reviewed App and prints exactly one status line.
func run(ctx context.Context, getenv func(string) string, client *http.Client, api string, now func() time.Time, out io.Writer) int {
	for _, app := range reviewedApps {
		if stage := verifyApp(ctx, app, getenv, client, api, now); stage != "" {
			_, _ = fmt.Fprintf(out, "GITHUB_BROKER_APPS=UNVERIFIED app=%s stage=%s\n", app.slug, stage)
			return 2
		}
	}
	_, _ = fmt.Fprintf(out, "GITHUB_BROKER_APPS=VERIFIED apps=%d\n", len(reviewedApps))
	return 0
}

// verifyApp returns the stage that failed, or "" when the credential is the App's.
func verifyApp(ctx context.Context, app reviewedApp, getenv func(string) string, client *http.Client, api string, now func() time.Time) string {
	clientID := getenv(app.envPrefix + "_CLIENT_ID")
	if getenv(app.envPrefix+"_APP_ID") != app.appID || !clientIDPattern.MatchString(clientID) {
		return "config"
	}
	key, err := parseKey(getenv(app.envPrefix + "_PRIVATE_KEY"))
	if err != nil {
		return "key"
	}
	token, err := appJWT(key, clientID, now())
	if err != nil {
		return "key"
	}
	var identity struct {
		ID       int64  `json:"id"`
		Slug     string `json:"slug"`
		ClientID string `json:"client_id"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := getApp(ctx, client, api, token, &identity); err != nil {
		return "github"
	}
	if identity.ID != app.id || identity.Slug != app.slug || identity.ClientID != clientID || identity.Owner.Login != "devantler-tech" {
		return "identity"
	}
	return ""
}

func parseKey(value string) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) != 0 {
		return nil, errors.New("invalid signing key")
	}
	var key *rsa.PrivateKey
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		key, _ = parsed.(*rsa.PrivateKey)
	default:
		return nil, errors.New("invalid signing key")
	}
	if err != nil || key == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return nil, errors.New("invalid signing key")
	}
	return key, nil
}

// appJWT signs GitHub's short-lived App authentication token, issued by the client ID.
func appJWT(key *rsa.PrivateKey, clientID string, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{clientID, now.Add(-time.Minute).Unix(), now.Add(5 * time.Minute).Unix()})
	if err != nil {
		return "", errors.New("signing failed")
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("signing failed")
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func getApp(ctx context.Context, client *http.Client, api, token string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/app", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return errors.New("unexpected status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return errors.New("unreadable response")
	}
	return json.Unmarshal(data, target)
}
