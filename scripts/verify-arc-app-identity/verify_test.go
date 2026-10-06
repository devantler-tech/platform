package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var fixtureKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

type fixture struct {
	bao, github *httptest.Server
	options     verificationOptions
	mu          sync.Mutex
	requests    []string
	responses   map[string]string
	statuses    map[string]int
	redirect    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	key := fixtureKey()
	f := &fixture{
		responses: map[string]string{
			"/v1/auth/kubernetes/login":                     `{"auth":{"client_token":"synthetic-bao-token"}}`,
			"/v1/secret/data/infrastructure/arc/github-app": fmt.Sprintf(`{"data":{"data":{"app_id":"123","installation_id":"456","pem":%q}}}`, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
			"/app":                              `{"id":123,"client_id":"Iv1.synthetic"}`,
			"/orgs/devantler-tech/installation": `{"id":456,"app_id":123,"client_id":"Iv1.synthetic","account":{"login":"devantler-tech","type":"Organization"},"target_type":"Organization","suspended_at":null,"permissions":{"organization_self_hosted_runners":"write","metadata":"read"}}`,
		},
		statuses: map[string]int{},
	}
	baoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login":
			if r.Method != http.MethodPost {
				t.Error("login must be POST")
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload) != 2 || payload["role"] != "arc-secret-reader" || payload["jwt"] != "synthetic-reader-jwt" {
				t.Error("reader identity is not confined")
			}
		case "/v1/secret/data/infrastructure/arc/github-app":
			if r.Method != http.MethodGet || r.Header.Get("X-Vault-Token") != "synthetic-bao-token" {
				t.Error("entry read must use the dedicated login token")
			}
		case "/v1/auth/token/revoke-self":
			if r.Method != http.MethodPost || r.Header.Get("X-Vault-Token") != "synthetic-bao-token" {
				t.Error("cleanup may only revoke its own token")
			}
			status := f.statuses[r.URL.Path]
			if status == 0 {
				status = http.StatusNoContent
			}
			w.WriteHeader(status)
			return
		default:
			t.Errorf("unexpected OpenBao endpoint %s", r.URL.Path)
		}
		f.respond(w, r)
	})
	githubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if r.Method != http.MethodGet || (r.URL.Path != "/app" && r.URL.Path != "/orgs/devantler-tech/installation") {
			t.Error("GitHub verifier must perform only the two identity GETs")
		}
		verifyFixtureJWT(t, r.Header.Get("Authorization"), key)
		f.respond(w, r)
	})
	f.bao = httptest.NewTLSServer(baoHandler)
	f.github = httptest.NewTLSServer(githubHandler)
	t.Cleanup(f.bao.Close)
	t.Cleanup(f.github.Close)
	roots := x509.NewCertPool()
	roots.AddCert(f.bao.Certificate())
	roots.AddCert(f.github.Certificate())
	f.options = verificationOptions{baoURL: f.bao.URL, githubURL: f.github.URL, client: secureClient(roots, ""), expectedClientID: "Iv1.synthetic", readerJWT: "synthetic-reader-jwt", now: time.Now}
	return f
}

func (f *fixture) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
}
func (f *fixture) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}
func (f *fixture) respond(w http.ResponseWriter, r *http.Request) {
	if f.redirect == r.URL.Path {
		w.Header().Set("Location", f.github.URL+"/credential-sink")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	status := f.statuses[r.URL.Path]
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, f.responses[r.URL.Path])
}

func verifyFixtureJWT(t *testing.T, authorization string, key *rsa.PrivateKey) {
	t.Helper()
	if !strings.HasPrefix(authorization, "Bearer ") {
		t.Error("missing App JWT")
		return
	}
	parts := strings.Split(strings.TrimPrefix(authorization, "Bearer "), ".")
	if len(parts) != 3 {
		t.Error("invalid App JWT")
		return
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	var h map[string]string
	var p struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(header, &h) != nil || h["alg"] != "RS256" || json.Unmarshal(payload, &p) != nil || p.Iss != "Iv1.synthetic" || p.Iat > time.Now().Unix() || p.Exp <= time.Now().Unix() || p.Exp-p.Iat > 600 {
		t.Error("invalid App JWT identity or lifetime")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
		t.Error("App JWT does not prove possession of the stored key")
	}
}

func TestVerifyMatchingIdentityThroughTLS(t *testing.T) {
	f := newFixture(t)
	if got := verify(context.Background(), f.options); got != pass {
		t.Fatalf("matching authenticated identities: got %s, want PASS", got)
	}
	want := "POST /v1/auth/kubernetes/login|GET /v1/secret/data/infrastructure/arc/github-app|GET /app|GET /orgs/devantler-tech/installation|POST /v1/auth/token/revoke-self"
	if got := strings.Join(f.calls(), "|"); got != want {
		t.Fatalf("unexpected API surface: %s", got)
	}
}

func TestRejectIdentityAndInstallationMismatches(t *testing.T) {
	for _, tc := range []struct{ name, path, from, to string }{
		{"app ID", "/app", `"id":123`, `"id":124`},
		{"OAuth client ID", "/app", `"Iv1.synthetic"`, `"Iv1.management"`},
		{"installation ID", "/orgs/devantler-tech/installation", `"id":456`, `"id":457`},
		{"installation App ID", "/orgs/devantler-tech/installation", `"app_id":123`, `"app_id":124`},
		{"installation client ID", "/orgs/devantler-tech/installation", `"Iv1.synthetic"`, `"Iv1.management"`},
		{"organization", "/orgs/devantler-tech/installation", `"devantler-tech"`, `"other-org"`},
		{"account type", "/orgs/devantler-tech/installation", `"type":"Organization"`, `"type":"User"`},
		{"target type", "/orgs/devantler-tech/installation", `"target_type":"Organization"`, `"target_type":"User"`},
		{"read-only runners", "/orgs/devantler-tech/installation", `"organization_self_hosted_runners":"write"`, `"organization_self_hosted_runners":"read"`},
		{"missing runners", "/orgs/devantler-tech/installation", `"organization_self_hosted_runners":"write",`, ``},
		{"missing metadata", "/orgs/devantler-tech/installation", `,"metadata":"read"`, ``},
		{"suspended installation", "/orgs/devantler-tech/installation", `"suspended_at":null`, `"suspended_at":"2026-10-06T00:00:00Z"`},
		{"missing suspension state", "/orgs/devantler-tech/installation", `"suspended_at":null,`, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.responses[tc.path] = strings.Replace(f.responses[tc.path], tc.from, tc.to, 1)
			if got := verify(context.Background(), f.options); got != failIdentity {
				t.Fatalf("got %s, want identity failure", got)
			}
			assertCleanup(t, f)
		})
	}
}

func TestRejectMalformedAndFailedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
		want             outcome
	}{
		{"missing entry", "/v1/secret/data/infrastructure/arc/github-app", `{"errors":["sensitive-error-canary"]}`, 404, holdEntry},
		{"denied login", "/v1/auth/kubernetes/login", `{"errors":["sensitive-error-canary"]}`, 403, failAPI},
		{"denied entry", "/v1/secret/data/infrastructure/arc/github-app", `{"errors":["sensitive-error-canary"]}`, 403, failAPI},
		{"duplicate ID", "/app", `{"id":123,"id":124,"client_id":"Iv1.synthetic"}`, 200, failAPI},
		{"case-folded ID ambiguity", "/app", `{"id":124,"ID":123,"client_id":"Iv1.synthetic"}`, 200, failAPI},
		{"case-folded client ambiguity", "/app", `{"id":123,"client_id":"Iv1.other","CLIENT_ID":"Iv1.synthetic"}`, 200, failAPI},
		{"trailing response", "/app", `{"id":123,"client_id":"Iv1.synthetic"} {}`, 200, failAPI},
		{"malformed app", "/app", `not-json-sensitive-canary`, 200, failAPI},
		{"App API failure", "/app", `{"message":"sensitive-error-canary"}`, 500, failAPI},
		{"malformed ID", "/v1/secret/data/infrastructure/arc/github-app", `{"data":{"data":{"app_id":"123/extra","installation_id":"456","pem":"sensitive-key-canary"}}}`, 200, failIdentity},
		{"malformed key", "/v1/secret/data/infrastructure/arc/github-app", `{"data":{"data":{"app_id":"123","installation_id":"456","pem":"sensitive-key-canary"}}}`, 200, failIdentity},
		{"oversized response", "/app", strings.Repeat("sensitive-canary", 100000), 200, failAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.responses[tc.path] = tc.body
			f.statuses[tc.path] = tc.status
			if got := verify(context.Background(), f.options); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if tc.path != "/v1/auth/kubernetes/login" {
				assertCleanup(t, f)
			}
		})
	}
}

func TestRejectUnsafeTransportWithoutCredentials(t *testing.T) {
	for _, tc := range []string{"http Bao", "http GitHub", "invalid CA", "invalid hostname", "redirect Bao", "redirect GitHub", "proxy"} {
		t.Run(tc, func(t *testing.T) {
			f := newFixture(t)
			switch tc {
			case "http Bao":
				f.options.baoURL = strings.Replace(f.options.baoURL, "https:", "http:", 1)
			case "http GitHub":
				f.options.githubURL = strings.Replace(f.options.githubURL, "https:", "http:", 1)
			case "invalid CA":
				f.options.client = secureClient(x509.NewCertPool(), "")
			case "invalid hostname":
				roots := x509.NewCertPool()
				roots.AddCert(f.bao.Certificate())
				f.options.client = secureClient(roots, "wrong.test")
			case "redirect Bao":
				f.redirect = "/v1/auth/kubernetes/login"
			case "redirect GitHub":
				f.redirect = "/app"
			case "proxy":
				t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
			}
			got := verify(context.Background(), f.options)
			if tc == "proxy" {
				if got != pass {
					t.Fatalf("environment proxy affected fixed authenticated connection: %s", got)
				}
				return
			}
			if got == pass {
				t.Fatal("unsafe connection was accepted")
			}
			for _, call := range f.calls() {
				if strings.Contains(call, "credential-sink") {
					t.Fatal("credential-bearing redirect was followed")
				}
			}
			if tc == "http Bao" || tc == "http GitHub" || tc == "invalid CA" || tc == "invalid hostname" {
				if len(f.calls()) != 0 {
					t.Fatal("transmitted credentials before TLS validation")
				}
			}
		})
	}
}

func TestCleanupFailureAndCancellationCannotPass(t *testing.T) {
	f := newFixture(t)
	f.statuses["/v1/auth/token/revoke-self"] = 500
	if got := verify(context.Background(), f.options); got != failCleanup {
		t.Fatalf("cleanup failure got %s", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := verify(ctx, newFixture(t).options); got == pass {
		t.Fatal("cancelled verification passed")
	}
}

func assertCleanup(t *testing.T, f *fixture) {
	t.Helper()
	calls := f.calls()
	if len(calls) == 0 || calls[len(calls)-1] != "POST /v1/auth/token/revoke-self" {
		t.Fatal("ephemeral OpenBao login was not revoked")
	}
}

func TestUnicodeCaseFoldAmbiguity(t *testing.T) {
	for _, body := range []string{`{"id":1,"ID":2}`, `{"s":1,"ſ":2}`, `{"k":1,"K":2}`, `{"nested":{"account":{},"ACCOUNT":{}}}`} {
		if validateJSON([]byte(body)) == nil {
			t.Fatalf("ambiguous keys accepted: %s", body)
		}
	}
}
