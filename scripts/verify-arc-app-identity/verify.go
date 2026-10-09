package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type outcome string

const (
	pass          outcome = "PASS"
	holdTransport outcome = "HOLD_TRANSPORT"
	holdEntry     outcome = "HOLD_ENTRY"
	failTransport outcome = "FAIL_TRANSPORT"
	failIdentity  outcome = "FAIL_IDENTITY"
	failAPI       outcome = "FAIL_API"
	failCleanup   outcome = "FAIL_CLEANUP"
)
const maxResponse = 1 << 20

var clientIDPattern = regexp.MustCompile(`^Iv[0-9A-Za-z.]{1,80}$`)

type verificationOptions struct {
	baoURL, githubURL, expectedClientID, readerJWT string
	client                                         *http.Client
	now                                            func() time.Time
	baoClient                                      *http.Client
	afterIdentity                                  func(context.Context, verificationOptions, string, int64) outcome
	cleanup                                        context.Context
}

func secureClient(roots *x509.CertPool, serverName string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func verify(ctx context.Context, options verificationOptions) (result outcome) {
	if !httpsOrigin(options.baoURL) || !httpsOrigin(options.githubURL) || options.client == nil {
		return holdTransport
	}
	if !clientIDPattern.MatchString(options.expectedClientID) || options.readerJWT == "" || len(options.readerJWT) > maxResponse || options.now == nil {
		return failIdentity
	}
	if options.cleanup == nil {
		var stop func()
		options.cleanup, stop = cleanupContext(ctx)
		defer stop()
	}
	baoClient := options.baoClient
	if baoClient == nil {
		baoClient = options.client
	}
	payload, _ := json.Marshal(map[string]string{"role": "arc-secret-reader", "jwt": options.readerJWT})
	var login struct {
		Auth struct {
			Token string `json:"client_token"`
		} `json:"auth"`
	}
	_, result = request(ctx, baoClient, http.MethodPost, options.baoURL+"/v1/auth/kubernetes/login", "", payload, &login)
	if result != pass {
		return result
	}
	if login.Auth.Token == "" || len(login.Auth.Token) > maxResponse || strings.ContainsAny(login.Auth.Token, "\r\n") {
		return failAPI
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(options.cleanup, 5*time.Second)
		defer cancel()
		_, status := request(cleanup, baoClient, http.MethodPost, options.baoURL+"/v1/auth/token/revoke-self", login.Auth.Token, nil, nil)
		if status != pass {
			result = failCleanup
		}
	}()
	var entry struct {
		Data struct {
			Data struct {
				AppID          string `json:"app_id"`
				InstallationID string `json:"installation_id"`
				PEM            string `json:"pem"`
			} `json:"data"`
		} `json:"data"`
	}
	status, result := request(ctx, baoClient, http.MethodGet, options.baoURL+"/v1/secret/data/infrastructure/arc/github-app", login.Auth.Token, nil, &entry)
	if status == http.StatusNotFound {
		return holdEntry
	}
	if result != pass {
		return result
	}
	appID, err := positiveID(entry.Data.Data.AppID)
	if err != nil {
		return failIdentity
	}
	installationID, err := positiveID(entry.Data.Data.InstallationID)
	if err != nil {
		return failIdentity
	}
	key, err := parseKey(entry.Data.Data.PEM)
	if err != nil {
		return failIdentity
	}
	jwt, err := appJWT(key, options.expectedClientID, options.now())
	if err != nil {
		return failIdentity
	}
	var app struct {
		ID       int64  `json:"id"`
		ClientID string `json:"client_id"`
	}
	_, result = githubRequest(ctx, options, "/app", jwt, &app)
	if result != pass {
		return result
	}
	if app.ID != appID || app.ClientID != options.expectedClientID {
		return failIdentity
	}
	var installation struct {
		ID         int64  `json:"id"`
		AppID      int64  `json:"app_id"`
		ClientID   string `json:"client_id"`
		TargetType string `json:"target_type"`
		Account    struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		SuspendedAt json.RawMessage   `json:"suspended_at"`
		Permissions map[string]string `json:"permissions"`
	}
	_, result = githubRequest(ctx, options, "/orgs/devantler-tech/installation", jwt, &installation)
	if result != pass {
		return result
	}
	if installation.ID != installationID || installation.AppID != appID || installation.ClientID != app.ClientID || installation.Account.Login != "devantler-tech" || installation.Account.Type != "Organization" || installation.TargetType != "Organization" || string(installation.SuspendedAt) != "null" || installation.Permissions["organization_self_hosted_runners"] != "write" || installation.Permissions["metadata"] != "read" {
		return failIdentity
	}
	if options.afterIdentity != nil {
		return options.afterIdentity(ctx, options, jwt, installation.ID)
	}
	return pass
}

func httpsOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

func positiveID(value string) (int64, error) {
	if value == "" || len(value) > 19 || value[0] == '0' {
		return 0, errors.New("invalid identifier")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, errors.New("invalid identifier")
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid identifier")
	}
	return id, nil
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
	if err != nil || key == nil || key.N.BitLen() < 2048 {
		return nil, errors.New("invalid signing key")
	}
	if key.Validate() != nil {
		return nil, errors.New("invalid signing key")
	}
	return key, nil
}

func appJWT(key *rsa.PrivateKey, clientID string, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{clientID, now.Add(-time.Minute).Unix(), now.Add(5 * time.Minute).Unix()})
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("signing failed")
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func githubRequest(ctx context.Context, options verificationOptions, path, jwt string, target any) (int, outcome) {
	return doRequest(ctx, options.client, http.MethodGet, options.githubURL+path, map[string]string{"Authorization": "Bearer " + jwt, "Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2026-03-10"}, nil, target)
}
func request(ctx context.Context, client *http.Client, method, endpoint, token string, body []byte, target any) (int, outcome) {
	headers := map[string]string{}
	if token != "" {
		headers["X-Vault-Token"] = token
	}
	return doRequest(ctx, client, method, endpoint, headers, body, target)
}
func doRequest(ctx context.Context, client *http.Client, method, endpoint string, headers map[string]string, body []byte, target any) (int, outcome) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, failAPI
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, failTransport
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && !(target == nil && response.StatusCode == http.StatusNoContent) {
		return response.StatusCode, failAPI
	}
	if target == nil {
		return response.StatusCode, pass
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse || validateJSON(data) != nil || json.Unmarshal(data, target) != nil {
		return response.StatusCode, failAPI
	}
	return response.StatusCode, pass
}

// Reject duplicate object keys, trailing documents and excessive nesting before
// decoding identity fields. Ambiguous API responses must never prove a match.
func validateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := jsonValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func jsonValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			folded := foldJSONName(name)
			if !ok || keys[folded] {
				return errors.New("duplicate JSON key")
			}
			keys[folded] = true
			if err := jsonValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := jsonValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

// encoding/json struct fields use Unicode simple case folding as well as
// exact matches. Reject every equivalent spelling before that decoder runs.
func foldJSONName(name string) string {
	return strings.Map(func(value rune) rune {
		minimum := value
		for folded := unicode.SimpleFold(value); folded != value; folded = unicode.SimpleFold(folded) {
			if folded < minimum {
				minimum = folded
			}
		}
		return minimum
	}, name)
}
