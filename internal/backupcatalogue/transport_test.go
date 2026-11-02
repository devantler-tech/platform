package backupcatalogue

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testEndpoint = "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testPolicy() Policy {
	return Policy{Endpoint: testEndpoint, Bucket: "backup-fixture", Prefix: "velero/kopia/fixture/", MaxRequests: 12, MaxBytes: 1024, MaxResponseBytes: 128}
}

func request(t *testing.T, method, suffix string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, testEndpoint+"/backup-fixture/"+suffix, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGuardPassesCompleteScopedReads(t *testing.T) {
	var calls int
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == "HEAD" {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, ContentLength: 7777}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("fixture")), ContentLength: 7}, nil
	})
	g, err := NewReadTransport(t.Context(), testPolicy(), base)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*http.Request{
		request(t, "GET", "velero/kopia/fixture/kopia.repository"),
		request(t, "HEAD", "velero/kopia/fixture/p123"),
		request(t, "GET", "?list-type=2&prefix=velero%2Fkopia%2Ffixture%2F&max-keys=1000&encoding-type=url&delimiter="),
		request(t, "GET", "?list-type=2&prefix=velero%2Fkopia%2Ffixture%2Fx&continuation-token=opaque%2Btoken&max-keys=1000&encoding-type=url"),
	} {
		response, readErr := g.RoundTrip(r)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, readErr = io.ReadAll(response.Body); readErr != nil {
			t.Fatal(readErr)
		}
		if readErr = response.Body.Close(); readErr != nil {
			t.Fatal(readErr)
		}
	}
	if calls != 4 || g.Err() != nil {
		t.Fatalf("complete reads were not accepted: calls=%d err=%v", calls, g.Err())
	}
}

func TestGuardBlocksEscapesBeforeNetworkAndStaysFailed(t *testing.T) {
	cases := map[string]func(*http.Request){
		"write":           func(r *http.Request) { r.Method = "PUT" },
		"delete":          func(r *http.Request) { r.Method = "DELETE" },
		"post":            func(r *http.Request) { r.Method = "POST" },
		"body":            func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("write")); r.ContentLength = 5 },
		"http":            func(r *http.Request) { r.URL.Scheme = "http" },
		"other endpoint":  func(r *http.Request) { r.URL.Host = "other.invalid" },
		"host override":   func(r *http.Request) { r.Host = "other.invalid" },
		"userinfo":        func(r *http.Request) { r.URL.User = url.User("foreign") },
		"other bucket":    func(r *http.Request) { r.URL.Path = "/foreign/velero/kopia/fixture/x" },
		"other prefix":    func(r *http.Request) { r.URL.Path = "/backup-fixture/velero/kopia/foreign/x" },
		"traversal":       func(r *http.Request) { r.URL.Path = "/backup-fixture/velero/kopia/fixture/../foreign" },
		"backslash":       func(r *http.Request) { r.URL.Path = "/backup-fixture/velero/kopia/fixture/x\\..\\foreign" },
		"query operation": func(r *http.Request) { r.URL.RawQuery = "delete=" },
		"foreign list prefix": func(r *http.Request) {
			r.URL.Path = "/backup-fixture/"
			r.URL.RawQuery = "list-type=2&prefix=foreign%2F"
		},
		"duplicate list prefix": func(r *http.Request) {
			r.URL.Path = "/backup-fixture/"
			r.URL.RawQuery = "list-type=2&prefix=velero%2Fkopia%2Ffixture%2F&prefix=foreign"
		},
		"unknown list key": func(r *http.Request) {
			r.URL.Path = "/backup-fixture/"
			r.URL.RawQuery = "list-type=2&prefix=velero%2Fkopia%2Ffixture%2F&versions="
		},
		"too many keys": func(r *http.Request) {
			r.URL.Path = "/backup-fixture/"
			r.URL.RawQuery = "list-type=2&prefix=velero%2Fkopia%2Ffixture%2F&max-keys=1001"
		},
		"malformed query": func(r *http.Request) { r.URL.RawQuery = "prefix=%zz" },
		"method override": func(r *http.Request) { r.Header.Set("X-HTTP-Method-Override", "DELETE") },
		"fragment":        func(r *http.Request) { r.URL.Fragment = "unbound" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected network")
			}))
			if err != nil {
				t.Fatal(err)
			}
			r := request(t, "GET", "velero/kopia/fixture/x")
			mutate(r)
			if _, err = g.RoundTrip(r); err == nil {
				t.Fatal("unsafe request accepted")
			}
			if g.Err() == nil {
				t.Fatal("failure was not retained")
			}
			if _, err = g.RoundTrip(request(t, "GET", "velero/kopia/fixture/valid")); err == nil || calls != 0 {
				t.Fatalf("failed reader resumed network: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestGuardRejectsPartialAndOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   io.ReadCloser
		status int
		length int64
	}{
		{"oversized header", io.NopCloser(strings.NewReader("x")), 200, 129},
		{"oversized stream", io.NopCloser(strings.NewReader(strings.Repeat("x", 129))), 200, -1},
		{"transport body failure", &failingBody{}, 200, -1},
		{"redirect", io.NopCloser(strings.NewReader("")), 302, 0},
		{"provider error", io.NopCloser(strings.NewReader("private provider detail")), 403, 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: tc.body, ContentLength: tc.length}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			response, err := g.RoundTrip(request(t, "GET", "velero/kopia/fixture/x"))
			if err == nil {
				_, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			if err == nil || g.Err() == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("partial response accepted or leaked: %v / %v", err, g.Err())
			}
		})
	}
}

type failingBody struct{}

func (*failingBody) Read([]byte) (int, error) { return 0, errors.New("private body failure") }
func (*failingBody) Close() error             { return nil }

func TestGuardTracksUnclosedReadsAndTotalLimits(t *testing.T) {
	p := testPolicy()
	p.MaxRequests, p.MaxBytes = 2, 10
	g, err := NewReadTransport(t.Context(), p, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("123456")), ContentLength: -1}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.RoundTrip(request(t, "GET", "velero/kopia/fixture/x"))
	if err != nil {
		t.Fatal(err)
	}
	if g.Err() == nil {
		t.Fatal("unclosed response mistaken for complete observation")
	}
	if _, err = io.ReadAll(r.Body); err != nil {
		t.Fatal(err)
	}
	if err = r.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if g.Err() != nil {
		t.Fatal(g.Err())
	}
	r, err = g.RoundTrip(request(t, "GET", "velero/kopia/fixture/y"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(r.Body); err == nil {
		t.Fatal("aggregate byte limit ignored")
	}
	_ = r.Body.Close()
	if g.Err() == nil {
		t.Fatal("aggregate failure lost")
	}
}

func TestGuardDeadlineAndConcurrentRequestBudget(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	calls := 0
	g, err := NewReadTransport(ctx, testPolicy(), roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("network") }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.RoundTrip(request(t, "GET", "velero/kopia/fixture/x")); err == nil || calls != 0 {
		t.Fatal("expired read reached network")
	}

	p := testPolicy()
	p.MaxRequests = 1
	var mu sync.Mutex
	calls = 0
	g, err = NewReadTransport(t.Context(), p, roundTripFunc(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if r, readErr := g.RoundTrip(request(t, "GET", "velero/kopia/fixture/x")); readErr == nil {
				_ = r.Body.Close()
			}
		})
	}
	wg.Wait()
	if calls > 1 || g.Err() == nil {
		t.Fatalf("concurrent budget escaped: %d / %v", calls, g.Err())
	}
}

type delayedDeadline struct{ context.Context }

func (delayedDeadline) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }
func (delayedDeadline) Err() error                  { return nil }

func TestGuardHonorsCallerDeadlineBeforeCancellationDelivery(t *testing.T) {
	calls := 0
	g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), ContentLength: 0}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	r := request(t, "GET", "velero/kopia/fixture/x").WithContext(delayedDeadline{t.Context()})
	if _, err = g.RoundTrip(r); err == nil || calls != 0 {
		t.Fatalf("expired caller reached storage before cancellation delivery: calls=%d err=%v", calls, err)
	}
}

func TestGuardRejectsInvalidPolicyAndIncompleteClose(t *testing.T) {
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Endpoint = "https://other.invalid" },
		func(p *Policy) { p.Endpoint += "/" },
		func(p *Policy) { p.Bucket = "../foreign" },
		func(p *Policy) { p.Prefix = "../foreign/" },
		func(p *Policy) { p.Prefix = "valid/\tforeign/" },
		func(p *Policy) { p.MaxRequests = 0 },
		func(p *Policy) { p.MaxBytes = 0 },
		func(p *Policy) { p.MaxResponseBytes = 0 },
	} {
		p := testPolicy()
		mutate(&p)
		g, err := NewReadTransport(t.Context(), p, roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("network during construction"); return nil, nil }))
		if err == nil || g != nil {
			t.Fatalf("invalid policy accepted: %#v", p)
		}
	}
	g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("unfinished")), ContentLength: -1}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.RoundTrip(request(t, "GET", "velero/kopia/fixture/x"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Body.Close(); err == nil || g.Err() == nil {
		t.Fatal("partial read accepted after close")
	}
}

func TestGuardHeadMetadataHasNoResponseBody(t *testing.T) {
	g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, ContentLength: 1 << 30}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.RoundTrip(request(t, "HEAD", "velero/kopia/fixture/p123"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(r.Body); err != nil {
		t.Fatalf("object length mistaken for HEAD body length: %v", err)
	}
	if err = r.Body.Close(); err != nil || g.Err() != nil {
		t.Fatalf("valid HEAD not complete: %v / %v", err, g.Err())
	}
}

func TestGuardRequiresExplicitOptionalMissingClassification(t *testing.T) {
	for _, key := range []string{"kopia.blobcfg", "kopia.repository", "x123"} {
		t.Run(key, func(t *testing.T) {
			g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(*http.Request) (*http.Response, error) {
				data := "<Error><Code>NoSuchKey</Code></Error>"
				return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(data)), ContentLength: int64(len(data))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			r, err := g.RoundTrip(request(t, "GET", "velero/kopia/fixture/"+key))
			if key != "kopia.blobcfg" {
				if err == nil || g.Err() == nil {
					t.Fatal("missing required data accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("optional retention config cannot be absent: %v", err)
			}
			if g.Err() == nil {
				t.Fatal("unclassified missing object accepted")
			}
			if _, err = io.ReadAll(r.Body); err != nil {
				t.Fatal(err)
			}
			if err = r.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if g.Err() == nil {
				t.Fatal("closed but unclassified missing object accepted")
			}
			if err = g.acceptMissingRetentionConfig(); err != nil || g.Err() != nil {
				t.Fatalf("valid classification not accepted: %v / %v", err, g.Err())
			}
		})
	}
}

type blockedClose struct {
	entered, release chan struct{}
	once             sync.Once
}

func (*blockedClose) Read([]byte) (int, error) { return 0, io.EOF }
func (b *blockedClose) Close() error           { b.once.Do(func() { close(b.entered) }); <-b.release; return nil }

func TestGuardExpiredResponseCannotCompleteDuringCleanup(t *testing.T) {
	body := &blockedClose{entered: make(chan struct{}), release: make(chan struct{})}
	g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body, ContentLength: 0}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	r := request(t, "GET", "velero/kopia/fixture/x").WithContext(ctx)
	result := make(chan error, 1)
	go func() { _, readErr := g.RoundTrip(r); result <- readErr }()
	<-body.entered
	held := g.Err()
	close(body.release)
	if err = <-result; err == nil {
		t.Fatal("expired response accepted")
	}
	if held == nil {
		t.Fatal("expired response reported complete while cleanup was blocked")
	}
}

func TestGuardRejectsMalformedOptionalErrorPayloads(t *testing.T) {
	for _, data := range []string{"", "<Error>", "<Error><Code>NoSuchKey</Code>", "<Error><Code>AccessDenied</Code><Code>NoSuchKey</Code></Error>", "<Other><Code>NoSuchKey</Code></Other>", "<Error><Code>NoSuchKey</Code></Error><Error><Code>NoSuchKey</Code></Error>"} {
		g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(r *http.Request) (*http.Response, error) { return storageResponse(r, 404, data), nil }))
		if err != nil {
			t.Fatal(err)
		}
		r, readErr := g.RoundTrip(request(t, "GET", "velero/kopia/fixture/kopia.blobcfg"))
		if readErr == nil {
			_, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		if readErr == nil {
			t.Fatalf("malformed optional absence passed transport: %q", data)
		}
	}
}

func TestGuardCannotCompleteBeforeResponseHeadersArrive(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	g, err := NewReadTransport(t.Context(), testPolicy(), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return storageResponse(r, 200, "done"), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	r := request(t, "GET", "velero/kopia/fixture/x")
	result := make(chan error, 1)
	go func() {
		response, readErr := g.RoundTrip(r)
		if readErr == nil {
			_, readErr = io.ReadAll(response.Body)
			readErr = errors.Join(readErr, response.Body.Close())
		}
		result <- readErr
	}()
	<-entered
	held := g.Err()
	close(release)
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	if held == nil {
		t.Fatal("in-flight request reported complete before receiving headers")
	}
	if g.Err() != nil {
		t.Fatal(g.Err())
	}
}

type countedBody struct {
	io.Reader
	consumed *atomic.Int64
}

func (b countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.consumed.Add(int64(n))
	return n, err
}
func (countedBody) Close() error { return nil }

func TestGuardReservesAggregateBytesBeforeConcurrentReads(t *testing.T) {
	p := testPolicy()
	p.MaxBytes = 10
	var consumed atomic.Int64
	g, err := NewReadTransport(t.Context(), p, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{Request: r, StatusCode: 200, Header: http.Header{}, Body: countedBody{strings.NewReader(strings.Repeat("x", 64)), &consumed}, ContentLength: -1}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var bodies []io.ReadCloser
	for range 2 {
		r, readErr := g.RoundTrip(request(t, "GET", "velero/kopia/fixture/x"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		bodies = append(bodies, r.Body)
	}
	var wg sync.WaitGroup
	for _, body := range bodies {
		wg.Go(func() { _, _ = io.ReadAll(body); _ = body.Close() })
	}
	wg.Wait()
	if consumed.Load() > p.MaxBytes+1 || g.Err() == nil {
		t.Fatalf("aggregate bound applied after consuming bytes: consumed=%d max=%d err=%v", consumed.Load(), p.MaxBytes, g.Err())
	}
}
