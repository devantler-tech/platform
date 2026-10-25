package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

// Exercise the actual SDK and App issuer through a fake HTTP transport. No
// production key, token, hostname lookup or network request is used.
type fakeGitHubHTTP struct {
	comments                                               []map[string]any
	mu                                                     sync.Mutex
	issues                                                 []map[string]any
	posts, mints, repoReads                                int
	failRepoReads                                          int
	mintStatus, postStatus                                 int
	extraGrant, wrongOwner, privateRepo, missingVisibility bool
	copyMarker, onlyForeign, duplicateBot                  bool
	commitThenTimeout                                      bool
	mintFailureBody                                        *trackedBody
	unsafeRequest                                          bool
}
type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error { b.closed.Store(true); return nil }
func jsonResponse(req *http.Request, status int, value any) *http.Response {
	b, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b))), Request: req}
}
func (f *fakeGitHubHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.URL.Host != "api.github.com" || req.URL.Scheme != "https" {
		f.unsafeRequest = true
		return nil, errors.New("private-canary")
	}
	path := req.URL.Path
	if path == "/app/installations/17/access_tokens" && req.Method == "POST" {
		f.mints++
		var body struct {
			IDs         []int64           `json:"repository_ids"`
			Permissions map[string]string `json:"permissions"`
		}
		if json.NewDecoder(req.Body).Decode(&body) != nil || len(body.IDs) != 1 || body.IDs[0] != repositoryID || len(body.Permissions) != 1 || body.Permissions["issues"] != "write" || !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.unsafeRequest = true
			return jsonResponse(req, 400, map[string]string{"message": "private-canary"}), nil
		}
		if f.mintStatus != 0 {
			resp := jsonResponse(req, f.mintStatus, map[string]string{"message": "private-canary"})
			body := &trackedBody{Reader: strings.NewReader(`{"message":"private-canary"}`)}
			resp.Body = body
			resp.Header.Set("Retry-After", "600")
			f.mintFailureBody = body
			return resp, nil
		}
		permissions := map[string]string{"issues": "write", "metadata": "read"}
		if f.extraGrant {
			permissions["contents"] = "write"
		}
		return jsonResponse(req, 201, map[string]any{"token": "ghs_" + strings.Repeat("x", 516), "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339), "permissions": permissions, "repositories": []any{f.repository()}}), nil
	}
	if req.Header.Get("Authorization") != "Bearer ghs_"+strings.Repeat("x", 516) {
		f.unsafeRequest = true
		return jsonResponse(req, 401, map[string]string{"message": "private-canary"}), nil
	}
	switch {
	case path == "/apps/"+publisherAppSlug:
		return jsonResponse(req, 200, map[string]any{"id": publisherAppID, "slug": publisherAppSlug, "owner": map[string]any{"login": repositoryOwner}}), nil
	case path == "/repos/"+repositoryOwner+"/"+repositoryName:
		f.repoReads++
		if f.failRepoReads > 0 {
			f.failRepoReads--
			return jsonResponse(req, 503, map[string]string{"message": "private-canary"}), nil
		}
		return jsonResponse(req, 200, f.repository()), nil
	case path == "/repos/"+repositoryOwner+"/"+repositoryName+"/issues" && req.Method == "POST":
		f.posts++
		if f.postStatus != 0 {
			return jsonResponse(req, f.postStatus, map[string]string{"message": "private-canary"}), nil
		}
		var body map[string]any
		if json.NewDecoder(req.Body).Decode(&body) != nil || len(body) != 3 || body["title"] != routes["cnpg-degraded"] || body["type"] != "Bug" {
			f.unsafeRequest = true
			return jsonResponse(req, 400, map[string]string{"message": "private-canary"}), nil
		}
		body["number"] = len(f.issues) + 1
		body["type"] = map[string]any{"id": 1, "name": "Bug"}
		body["state"] = "open"
		body["user"] = botUser()
		f.issues = append(f.issues, body)
		if f.commitThenTimeout {
			return nil, errors.New("private-canary timeout after commit")
		}
		return jsonResponse(req, 201, body), nil
	case path == "/repos/"+repositoryOwner+"/"+repositoryName+"/issues" && req.Method == "GET":
		var result []map[string]any
		if !f.onlyForeign {
			result = append(result, f.issues...)
		}
		if (f.copyMarker || f.onlyForeign) && len(f.issues) > 0 {
			result = append(result, map[string]any{"number": 800, "body": f.issues[0]["body"], "user": map[string]any{"id": 9, "login": "untrusted-contributor", "type": "User"}})
		}
		if f.duplicateBot && len(f.issues) > 0 {
			result = append(result, map[string]any{"number": 801, "body": f.issues[0]["body"], "user": botUser()})
		}
		return jsonResponse(req, 200, result), nil
	case strings.HasPrefix(path, "/repos/"+repositoryOwner+"/"+repositoryName+"/issues/"):
		parts := strings.Split(strings.TrimPrefix(path, "/repos/"+repositoryOwner+"/"+repositoryName+"/issues/"), "/")
		number, err := strconv.Atoi(parts[0])
		if err != nil || number <= 0 || number > len(f.issues) {
			return jsonResponse(req, 404, map[string]string{"message": "missing fixture issue"}), nil
		}
		if len(parts) == 1 && req.Method == "GET" {
			return jsonResponse(req, 200, f.issues[number-1]), nil
		}
		if len(parts) == 2 && parts[1] == "comments" {
			if req.Method == "POST" {
				f.posts++
				if f.postStatus != 0 {
					return jsonResponse(req, f.postStatus, map[string]string{"message": "private-canary"}), nil
				}
				var body map[string]any
				if json.NewDecoder(req.Body).Decode(&body) != nil || len(body) != 1 {
					f.unsafeRequest = true
					return jsonResponse(req, 400, map[string]string{"message": "private-canary"}), nil
				}
				body["id"] = len(f.comments) + 1
				body["user"] = botUser()
				body["fixture_issue"] = number
				f.comments = append(f.comments, body)
				if f.commitThenTimeout {
					return nil, errors.New("private-canary timeout after comment commit")
				}
				return jsonResponse(req, 201, body), nil
			}
			var result []map[string]any
			for _, c := range f.comments {
				if c["fixture_issue"] == number {
					result = append(result, c)
				}
			}
			return jsonResponse(req, 200, result), nil
		}
		f.unsafeRequest = true
		return jsonResponse(req, 404, map[string]string{"message": "private-canary"}), nil
	default:
		f.unsafeRequest = true
		return jsonResponse(req, 404, map[string]string{"message": "private-canary"}), nil
	}
}

func TestSDKCommentTimeoutReconcilesWithoutDuplicate(t *testing.T) {
	f := &fakeGitHubHTTP{}
	now := func() time.Time { return fixtureTime.Add(30 * time.Minute) }
	p := publisher{dir: t.TempDir() + "/state", api: testRemote(t, f, now), now: now}
	if err := p.run(context.Background(), ptr(eventAt("firing", 0))); err != nil {
		t.Fatal(err)
	}
	f.commitThenTimeout = true
	if err := p.run(context.Background(), ptr(eventAt("healthy", 1))); !errors.Is(err, errAmbiguous) {
		t.Fatal(err)
	}
	f.commitThenTimeout = false
	p.api = testRemote(t, f, now)
	if err := p.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	s, err := readJournal(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.posts != 2 || len(f.issues) != 1 || len(f.comments) != 1 || len(s.Receipts) != 2 || s.Receipts[1].Comment != 1 {
		t.Fatal("comment was lost or repeated")
	}
}

func TestSDKExpiredTokenRefusalRetainsOperationForNewInvocation(t *testing.T) {
	f := &fakeGitHubHTTP{postStatus: 401}
	clock := fixtureTime.Add(30 * time.Minute)
	now := func() time.Time { return clock }
	p := publisher{dir: t.TempDir() + "/state", api: testRemote(t, f, now), now: now}
	if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.As(err, new(*retryError)) {
		t.Fatal(err)
	}
	if len(f.issues) != 0 {
		t.Fatal("refused request created an issue")
	}
	f.postStatus = 0
	clock = clock.Add(6 * time.Minute)
	p.api = testRemote(t, f, now)
	if err := p.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if f.mints != 2 || f.posts != 2 || len(f.issues) != 1 {
		t.Fatal("expired authentication was not renewed safely")
	}
}
func (f *fakeGitHubHTTP) repository() map[string]any {
	owner := repositoryOwner
	if f.wrongOwner {
		owner = "untrusted-owner"
	}
	r := map[string]any{"id": repositoryID, "name": repositoryName, "full_name": repositoryOwner + "/" + repositoryName, "owner": map[string]any{"login": owner}, "private": f.privateRepo, "archived": false, "has_issues": true}
	if f.missingVisibility {
		delete(r, "private")
	}
	return r
}
func botUser() map[string]any {
	return map[string]any{"id": 77, "login": publisherAppSlug + "[bot]", "type": "Bot"}
}

var fixtureKeyOnce sync.Once
var fixtureKey []byte
var fixtureKeyErr error

func testKey(t *testing.T) []byte {
	t.Helper()
	fixtureKeyOnce.Do(func() {
		var key *rsa.PrivateKey
		key, fixtureKeyErr = rsa.GenerateKey(rand.Reader, 2048)
		if fixtureKeyErr == nil {
			fixtureKey = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		}
	})
	if fixtureKeyErr != nil {
		t.Fatal(fixtureKeyErr)
	}
	return fixtureKey
}
func testRemote(t *testing.T, server *fakeGitHubHTTP, now func() time.Time) *githubRemote {
	t.Helper()
	transport, err := ghinstallation.New(server, publisherAppID, 17, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	transport.Client = &http.Client{Transport: server, Timeout: time.Second, CheckRedirect: noRedirect}
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{RepositoryIDs: []int64{repositoryID}, Permissions: &github.InstallationPermissions{Issues: github.Ptr("write")}}
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: &scopedTransport{installation: transport, base: server}, CheckRedirect: noRedirect, Timeout: time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	return &githubRemote{client: client, now: now}
}
func TestNativeSDKMintAndReceipt(t *testing.T) {
	f := &fakeGitHubHTTP{}
	now := func() time.Time { return fixtureTime.Add(30 * time.Minute) }
	p := publisher{dir: t.TempDir() + "/state", api: testRemote(t, f, now), now: now}
	if err := p.run(context.Background(), ptr(eventAt("firing", 0))); err != nil {
		t.Fatal(err)
	}
	s, err := readJournal(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 || f.mints != 1 || f.unsafeRequest || len(s.Receipts) != 1 || len(s.Pending) != 0 {
		t.Fatal("scoped mint and receipt were not verified")
	}
}
func TestPreWriteReadFailureRemainsDefinitelyUnsent(t *testing.T) {
	f := &fakeGitHubHTTP{failRepoReads: 1}
	clock := fixtureTime.Add(30 * time.Minute)
	now := func() time.Time { return clock }
	dir := t.TempDir() + "/state"
	p := publisher{dir: dir, api: testRemote(t, f, now), now: now}
	err := p.run(context.Background(), ptr(eventAt("firing", 0)))
	if !errors.As(err, new(*retryError)) {
		t.Fatal(err)
	}
	s, err := readJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.posts != 0 || len(s.Pending) != 1 || s.Pending[0].Sent {
		t.Fatal("a failed read was treated as a possible POST")
	}
	clock = clock.Add(6 * time.Minute)
	p.api = testRemote(t, f, now)
	if err = p.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 {
		t.Fatal("unsent operation did not retry exactly once")
	}
}
func TestMintRefusalsRetryWithoutPublishingOrLeaking(t *testing.T) {
	for _, status := range []int{401, 429, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := &fakeGitHubHTTP{mintStatus: status}
			clock := fixtureTime.Add(30 * time.Minute)
			now := func() time.Time { return clock }
			p := publisher{dir: t.TempDir() + "/state", api: testRemote(t, f, now), now: now}
			err := p.run(context.Background(), ptr(eventAt("firing", 0)))
			if !errors.As(err, new(*retryError)) || strings.Contains(err.Error(), "private-canary") || f.posts != 0 || f.mintFailureBody == nil || !f.mintFailureBody.closed.Load() {
				t.Fatal("mint failure was not safely deferred")
			}
			clock = clock.Add(11 * time.Minute)
			f.mintStatus = 0
			p.api = testRemote(t, f, now)
			if err = p.run(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if f.posts != 1 {
				t.Fatal("deferred mint lost or repeated publication")
			}
		})
	}
}
func TestScopeAndVisibilityMismatchPreventAllPosts(t *testing.T) {
	for name, f := range map[string]*fakeGitHubHTTP{"extra-grant": {extraGrant: true}, "wrong-owner": {wrongOwner: true}, "private": {privateRepo: true}, "unknown-visibility": {missingVisibility: true}} {
		t.Run(name, func(t *testing.T) {
			now := func() time.Time { return fixtureTime.Add(30 * time.Minute) }
			p := publisher{dir: t.TempDir() + "/state", api: testRemote(t, f, now), now: now}
			if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errQuarantined) {
				t.Fatal("unsafe target accepted", err)
			}
			if f.posts != 0 {
				t.Fatal("unsafe target was published")
			}
		})
	}
}
func TestCopiedPublicMarkersHaveNoAuthority(t *testing.T) {
	for _, onlyForeign := range []bool{false, true} {
		t.Run(strconv.FormatBool(onlyForeign), func(t *testing.T) {
			f := &fakeGitHubHTTP{copyMarker: true, onlyForeign: onlyForeign, commitThenTimeout: true}
			now := func() time.Time { return fixtureTime.Add(30 * time.Minute) }
			p := publisher{dir: t.TempDir() + "/state", api: testRemote(t, f, now), now: now}
			if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errAmbiguous) {
				t.Fatal(err)
			}
			f.commitThenTimeout = false
			p.api = testRemote(t, f, now)
			err := p.run(context.Background(), nil)
			if onlyForeign {
				if !errors.Is(err, errAmbiguous) {
					t.Fatal("foreign marker became a receipt", err)
				}
			} else if err != nil {
				t.Fatal("foreign marker blocked a legitimate receipt", err)
			}
			if f.posts != 1 {
				t.Fatal("copied marker caused another POST")
			}
		})
	}
}
func TestDuplicateAuthenticatedMarkersRequireReconciliation(t *testing.T) {
	f := &fakeGitHubHTTP{duplicateBot: true}
	now := func() time.Time { return fixtureTime.Add(30 * time.Minute) }
	p := publisher{dir: t.TempDir() + "/state", api: testRemote(t, f, now), now: now}
	if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errQuarantined) {
		t.Fatal("duplicate bot receipts were accepted", err)
	}
}
