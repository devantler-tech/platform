package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const initialR2Object = `{"version":1,"instance":"fixture-instance","generation":1,"nonce":"11111111111111111111111111111111","journal":{"version":1,"routes":{},"pending":[],"receipts":[],"attemptDay":"","attemptCount":0}}`
const pendingR2Object = `{"version":1,"instance":"fixture-instance","generation":1,"nonce":"11111111111111111111111111111111","journal":{"version":1,"routes":{"cnpg-degraded":{"observedAt":"2026-10-24T20:00:00Z","state":"firing","episode":"22222222222222222222222222222222","issue":0}},"pending":[{"id":"33333333333333333333333333333333","episode":"22222222222222222222222222222222","route":"cnpg-degraded","state":"firing","createdAt":"2026-10-24T20:00:00Z","issue":0,"sent":false,"attempts":0,"retryAt":"0001-01-01T00:00:00Z"}],"receipts":[],"attemptDay":"","attemptCount":0}}`

// This models only the external object API. Requests still traverse the actual
// maintained SDK, signing and serialization configured by production code.
type r2HTTPFixture struct {
	mu              sync.Mutex
	body            []byte
	etag            string
	gets, puts      int
	unsafe          bool
	requests        []string
	lostPutResponse int
	putStatus       int
	omitPutETag     bool
}

func (f *r2HTTPFixture) seed(data string) {
	f.body = []byte(data)
	sum := sha256.Sum256(f.body)
	f.etag = `"` + hex.EncodeToString(sum[:16]) + `"`
}

func (f *r2HTTPFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status, body := 200, ""
	headers := http.Header{}
	f.requests = append(f.requests, req.Method+" "+req.URL.String())
	wantQuery := map[string]string{http.MethodGet: "x-id=GetObject", http.MethodPut: "x-id=PutObject"}[req.Method]
	if req.URL.Scheme != "https" || req.URL.Host != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com" || req.URL.Path != "/fixture-journal/alerts/state.json" || req.URL.RawQuery != wantQuery || !strings.Contains(req.Header.Get("Authorization"), "Credential=fixture-access-key/") {
		f.unsafe = true
		status = 403
	} else {
		switch req.Method {
		case http.MethodGet:
			f.gets++
			if f.body == nil {
				status = 404
				body = `<Error><Code>NoSuchKey</Code></Error>`
			} else {
				body = string(f.body)
				headers.Set("ETag", f.etag)
				headers.Set("Content-Type", "application/json")
			}
		case http.MethodPut:
			f.puts++
			if f.putStatus != 0 {
				status = f.putStatus
				body = `<Error><Code>SlowDown</Code></Error>`
				break
			}
			if req.Header.Get("If-Match") == "" && req.Header.Get("If-None-Match") != "*" {
				f.unsafe = true
				status = 400
				break
			}
			if match := req.Header.Get("If-Match"); match != "" && (f.body == nil || match != f.etag) || req.Header.Get("If-None-Match") == "*" && f.body != nil {
				status = 412
				body = `<Error><Code>PreconditionFailed</Code></Error>`
				break
			}
			data, err := io.ReadAll(io.LimitReader(req.Body, 132*1024+1))
			if err != nil || len(data) > 132*1024 || req.Header.Get("Content-Type") != "application/json" {
				f.unsafe = true
				status = 400
				break
			}
			f.seed(string(data))
			headers.Set("ETag", f.etag)
			if f.omitPutETag {
				headers.Del("ETag")
			}
			if f.puts == f.lostPutResponse {
				return nil, errors.New("fixture connection lost after commit")
			}
		default:
			f.unsafe = true
			status = 405
		}
	}
	headers.Set("Content-Length", fmt.Sprint(len(body)))
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: headers, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
}

func r2FixtureStore(t *testing.T, fixture *r2HTTPFixture) *r2JournalStore {
	t.Helper()
	store, err := newR2JournalStore(r2Settings{AccountID: strings.Repeat("a", 32), Bucket: "fixture-journal", Key: "alerts/state.json", Instance: "fixture-instance"}, r2Credentials{AccessKeyID: "fixture-access-key", SecretAccessKey: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	attachR2Fixture(store, fixture)
	return store
}

func attachR2Fixture(store *r2JournalStore, fixture http.RoundTripper) {
	// Replace only the external network boundary, retaining the production SDK
	// options, signer, explicit credentials and retry policy.
	options := store.client.(*s3.Client).Options()
	client := *(options.HTTPClient.(*http.Client))
	client.Transport = fixture
	options.HTTPClient = &client
	store.client = s3.New(options)
}

func TestR2ReadBindsTheBodyAndRevisionInOneRequest(t *testing.T) {
	f := &r2HTTPFixture{}
	f.seed(initialR2Object)
	store := r2FixtureStore(t, f)
	session, err := store.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	state, err := session.Read(context.Background())
	if err != nil || state.Version != 1 || state.Routes == nil {
		t.Fatalf("valid remote journal refused: %v; requests=%v unsafe=%v", err, f.requests, f.unsafe)
	}
	if f.gets != 1 || f.puts != 0 || f.unsafe {
		t.Fatal("read did not use one bounded, signed object request")
	}
	if native := session.(*r2JournalSession); native.etag != f.etag || native.generation != 1 {
		t.Fatal("state was separated from its exact revision")
	}
	if !bytes.Equal(f.body, []byte(initialR2Object)) {
		t.Fatal("read changed the journal")
	}
}

func TestR2InitializationIsCreateOnceAndReadBack(t *testing.T) {
	f := &r2HTTPFixture{}
	store := r2FixtureStore(t, f)
	if err := store.Initialize(context.Background()); err != nil {
		t.Fatalf("explicit fresh initialization failed: %v", err)
	}
	committed := append([]byte(nil), f.body...)
	if f.puts != 1 || f.gets != 1 || f.unsafe {
		t.Fatal("initialization did not make one conditional creation and verify it")
	}
	if err := store.Initialize(context.Background()); !errors.Is(err, errStore) {
		t.Fatal("existing live state was initialized again", err)
	}
	if !bytes.Equal(f.body, committed) || f.puts != 2 || f.gets != 1 || f.unsafe {
		t.Fatal("reinitialization replaced or retried existing state")
	}
}

func TestR2UncertainInitializationIsNeverRetriedOrTreatedAsConfirmed(t *testing.T) {
	for _, kind := range []string{"lost-response", "missing-etag"} {
		t.Run(kind, func(t *testing.T) {
			f := &r2HTTPFixture{}
			if kind == "lost-response" {
				f.lostPutResponse = 1
			} else {
				f.omitPutETag = true
			}
			store := r2FixtureStore(t, f)
			if err := store.Initialize(context.Background()); !errors.Is(err, errStore) || f.puts != 1 || f.gets != 0 || f.body == nil || f.unsafe {
				t.Fatal("uncertain creation was retried or acknowledged", err)
			}
		})
	}
}

func TestR2ConditionalFailureNeverRetriesOrPermitsPublication(t *testing.T) {
	for _, status := range []int{409, 412, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := &r2HTTPFixture{putStatus: status}
			f.seed(initialR2Object)
			api := &fakeGitHub{}
			p := engine(t, t.TempDir(), api)
			p.store = r2FixtureStore(t, f)
			if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errStore) {
				t.Fatal("conditional failure was not propagated", err)
			}
			if f.puts != 1 || f.gets != 1 || api.writes != 0 || f.unsafe || string(f.body) != initialR2Object {
				t.Fatal("failed conditional write was retried or publication bypassed it")
			}
		})
	}
}

type synchronizedR2Reads struct {
	base  http.RoundTripper
	mu    sync.Mutex
	reads int
	ready chan struct{}
}

func (s *synchronizedR2Reads) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := s.base.RoundTrip(req)
	if req.Method == http.MethodGet && err == nil {
		s.mu.Lock()
		s.reads++
		wait := s.reads <= 2
		if s.reads == 2 {
			close(s.ready)
		}
		s.mu.Unlock()
		if wait {
			select {
			case <-s.ready:
			case <-req.Context().Done():
				_ = response.Body.Close()
				return nil, req.Context().Err()
			}
		}
	}
	return response, err
}

func TestConcurrentR2PublishersCannotBothSendTheSameIncident(t *testing.T) {
	f := &r2HTTPFixture{}
	f.seed(initialR2Object)
	store := r2FixtureStore(t, f)
	attachR2Fixture(store, &synchronizedR2Reads{base: f, ready: make(chan struct{})})
	api := &fakeGitHub{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := engine(t, t.TempDir(), api)
			p.store = store
			if err := p.run(ctx, ptr(eventAt("firing", 0))); err != nil && !errors.Is(err, errStore) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if api.writes != 1 || len(api.issues) != 1 || f.unsafe {
		t.Fatal("competing revisions duplicated or misrouted the incident")
	}
	var envelope struct {
		Journal journal `json:"journal"`
	}
	if json.Unmarshal(f.body, &envelope) != nil || len(envelope.Journal.Pending) != 0 || len(envelope.Journal.Receipts) != 1 {
		t.Fatal("winning session did not retain a single confirmed receipt")
	}
}

func TestR2UnsafeOrIncompleteJournalNeverPublishes(t *testing.T) {
	for name, body := range map[string]string{
		"missing":              "",
		"wrong-instance":       strings.Replace(initialR2Object, "fixture-instance", "other-instance", 1),
		"unknown-envelope":     strings.Replace(initialR2Object, `"version":1`, `"unexpected":true,"version":1`, 1),
		"duplicate-envelope":   strings.Replace(initialR2Object, `"instance":"fixture-instance"`, `"instance":"unapproved","instance":"fixture-instance"`, 1),
		"partial-journal":      `{"version":1,"instance":"fixture-instance","generation":1,"nonce":"11111111111111111111111111111111","journal":{"version":1,"routes":{}}}`,
		"unknown-journal":      strings.Replace(initialR2Object, `"attemptCount":0`, `"attemptCount":0,"unexpected":true`, 1),
		"oversized":            initialR2Object + strings.Repeat(" ", maxR2ObjectBytes),
		"oversized-journal":    strings.Replace(initialR2Object, `"journal":{`, `"journal":{`+strings.Repeat(" ", 128*1024), 1),
		"case-aliased-pending": strings.Replace(pendingR2Object, `"sent":false`, `"Sent":false`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := &r2HTTPFixture{}
			if body != "" {
				f.seed(body)
			}
			store := r2FixtureStore(t, f)
			session, err := store.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if _, err := session.Read(context.Background()); !errors.Is(err, errStore) {
				t.Fatalf("unsafe remote journal was accepted: %v", err)
			}
			if err := session.Save(context.Background(), journal{}); !errors.Is(err, errStore) {
				t.Fatal("invalid read authorized a write", err)
			}
			if f.puts != 0 || f.gets != 1 || f.unsafe {
				t.Fatal("invalid state was initialized or retried")
			}
		})
	}
}

func TestR2ConditionalWriteCannotOverwriteAnotherSession(t *testing.T) {
	f := &r2HTTPFixture{}
	f.seed(initialR2Object)
	store := r2FixtureStore(t, f)
	first, err := store.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	state, err := first.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stale, err := second.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.AttemptDay = "2026-10-25"
	state.AttemptCount = 1
	if err := first.Save(context.Background(), state); err != nil {
		t.Fatalf("valid conditional write refused: %v", err)
	}
	committed := append([]byte(nil), f.body...)
	if err := second.Save(context.Background(), stale); !errors.Is(err, errStore) {
		t.Fatal("stale session replaced newer state", err)
	}
	if !bytes.Equal(f.body, committed) || f.gets != 2 || f.puts != 2 || f.unsafe {
		t.Fatal("CAS conflict was retried or newer state lost")
	}
	var envelope struct {
		Generation int    `json:"generation"`
		Nonce      string `json:"nonce"`
		Journal    struct {
			AttemptCount int `json:"attemptCount"`
		} `json:"journal"`
	}
	if json.Unmarshal(f.body, &envelope) != nil || envelope.Generation != 2 || envelope.Nonce == strings.Repeat("1", 32) || envelope.Journal.AttemptCount != 1 {
		t.Fatal("acknowledged conditional write did not advance the bound state")
	}
	if err := second.Save(context.Background(), stale); !errors.Is(err, errStore) || f.puts != 2 {
		t.Fatal("failed session was reused")
	}
}

func TestR2LostAcknowledgementAtEveryPublicationBarrierNeverDuplicates(t *testing.T) {
	for _, barrier := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(barrier), func(t *testing.T) {
			f := &r2HTTPFixture{lostPutResponse: barrier}
			f.seed(initialR2Object)
			api := &fakeGitHub{}
			p := engine(t, t.TempDir(), api)
			p.store = r2FixtureStore(t, f)
			if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errStore) {
				t.Fatal("lost storage acknowledgement did not stop the invocation", err)
			}
			wantWrites := 0
			if barrier == 3 {
				wantWrites = 1
			}
			if api.writes != wantWrites || f.puts != barrier || f.unsafe {
				t.Fatal("uncertain storage write was retried or bypassed")
			}
			resumed := engine(t, t.TempDir(), api)
			resumed.store = r2FixtureStore(t, f)
			err := resumed.run(context.Background(), nil)
			if barrier == 2 {
				if !errors.Is(err, errAmbiguous) || api.writes != 0 || f.puts != 2 {
					t.Fatal("durable send intent permitted an unproven resend", err)
				}
			} else if err != nil || api.writes != 1 || len(api.issues) != 1 {
				t.Fatal("recovered journal lost or duplicated the incident", err)
			}
			var envelope struct {
				Journal journal `json:"journal"`
			}
			if json.Unmarshal(f.body, &envelope) != nil {
				t.Fatal("committed journal unreadable")
			}
			if barrier == 2 {
				if len(envelope.Journal.Pending) != 1 || !envelope.Journal.Pending[0].Sent {
					t.Fatal("lost send acknowledgement erased uncertainty")
				}
			} else if len(envelope.Journal.Pending) != 0 || len(envelope.Journal.Receipts) != 1 {
				t.Fatal("recovery lost its confirmed receipt")
			}
		})
	}
}
