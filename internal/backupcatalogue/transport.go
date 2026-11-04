// Package backupcatalogue implements bounded, read-only backup observations.
package backupcatalogue

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Policy bounds the endpoint, object prefix, request count and response bytes for reads.
type Policy struct {
	Endpoint, Bucket, Prefix   string
	MaxRequests                int
	MaxBytes, MaxResponseBytes int64
}

var errReadHeld = errors.New("backup catalogue read is incomplete or outside its approved bounds")

var r2Host = regexp.MustCompile(`^[a-f0-9]{32}\.r2\.cloudflarestorage\.com$`)
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var requestedRange = regexp.MustCompile(`^bytes=([0-9]+)-([0-9]+)$`)
var returnedRange = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

// ReadTransport must be installed before constructing the storage client. Its
// sticky failure applies to every request, including constructor probes and
// concurrent reads that outlive a failed repository operation.
type ReadTransport struct {
	ctx           context.Context
	cancel        context.CancelFunc
	policy        Policy
	base          http.RoundTripper
	host          string
	mu            sync.Mutex
	failed        bool
	requests      int
	bytes         int64
	open          int
	missing       int
	closedMissing int
	pending       int
	reserved      int64
	changed       chan struct{}
	operations    int
}

// NewReadTransport constructs a read transport within the supplied policy bounds.
func NewReadTransport(ctx context.Context, p Policy, base http.RoundTripper) (*ReadTransport, error) {
	u, err := url.Parse(p.Endpoint)
	if ctx == nil || base == nil || err != nil || u.Scheme != "https" || !r2Host.MatchString(u.Host) ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		!bucketName.MatchString(p.Bucket) || strings.Contains(p.Bucket, "..") ||
		p.Prefix == "" || !strings.HasSuffix(p.Prefix, "/") || !cleanKey(p.Prefix) ||
		p.MaxRequests < 1 || p.MaxRequests > 16384 || p.MaxBytes < 1 || p.MaxBytes > 1<<30 ||
		p.MaxResponseBytes < 1 || p.MaxResponseBytes > 64<<20 {
		return nil, errReadHeld
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Minute)
	return &ReadTransport{ctx: bounded, cancel: cancel, policy: p, base: base, host: u.Host, changed: make(chan struct{})}, nil
}

func cleanKey(key string) bool {
	for _, c := range key {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	if strings.ContainsAny(key, "\\\x00\r\n") || strings.HasPrefix(key, "/") || strings.Contains(key, "//") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return path.Clean(key) == strings.TrimSuffix(key, "/")
}

func (g *ReadTransport) allowed(r *http.Request) bool {
	if r == nil || r.URL == nil || (r.Method != "GET" && r.Method != "HEAD") ||
		r.URL.Scheme != "https" || r.URL.Host != g.host || (r.Host != "" && r.Host != g.host) ||
		r.URL.User != nil || r.URL.Fragment != "" || r.URL.Opaque != "" || r.URL.ForceQuery ||
		(r.Body != nil && r.Body != http.NoBody) || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 ||
		r.Header.Get("X-HTTP-Method-Override") != "" || r.Header.Get("X-Method-Override") != "" || r.Header.Get("Upgrade") != "" {
		return false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	root := "/" + g.policy.Bucket + "/"
	if r.URL.Path == root && r.Method == "GET" {
		for key, values := range q {
			if len(values) != 1 {
				return false
			}
			switch key {
			case "list-type":
				if values[0] != "2" {
					return false
				}
			case "encoding-type":
				if values[0] != "url" {
					return false
				}
			case "delimiter":
				if values[0] != "" {
					return false
				}
			case "fetch-owner":
				if values[0] != "true" {
					return false
				}
			case "prefix":
				if !strings.HasPrefix(values[0], g.policy.Prefix) || !cleanKey(values[0]) {
					return false
				}
			case "max-keys":
				n, parseErr := strconv.Atoi(values[0])
				if parseErr != nil || n < 1 || n > 1000 {
					return false
				}
			case "continuation-token":
				if values[0] == "" || len(values[0]) > 8192 {
					return false
				}
			default:
				return false
			}
		}
		return q.Get("list-type") == "2" && q.Get("prefix") != ""
	}
	key := strings.TrimPrefix(r.URL.Path, root)
	return strings.HasPrefix(r.URL.Path, root) && strings.HasPrefix(key, g.policy.Prefix) &&
		len(key) > len(g.policy.Prefix) && len(key) <= 1024 && cleanKey(key) && len(q) == 0
}

func (g *ReadTransport) activeLocked() bool {
	return !g.failed && contextActive(g.ctx)
}

func contextActive(ctx context.Context) bool {
	deadline, bounded := ctx.Deadline()
	return ctx.Err() == nil && (!bounded || time.Now().Before(deadline))
}

func (g *ReadTransport) hold() error {
	g.mu.Lock()
	g.failed = true
	g.cancel()
	g.signalLocked()
	g.mu.Unlock()
	return errReadHeld
}

func (g *ReadTransport) signalLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// RoundTrip validates and performs one request within the transport's read policy.
func (g *ReadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !g.allowed(r) || !contextActive(r.Context()) {
		return nil, g.hold()
	}
	g.mu.Lock()
	if !g.activeLocked() || g.requests >= g.policy.MaxRequests {
		g.mu.Unlock()
		return nil, g.hold()
	}
	g.requests++
	g.pending++
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(g.ctx, 30*time.Second)
	if deadline, bounded := r.Context().Deadline(); bounded {
		cancel()
		limit := time.Now().Add(30 * time.Second)
		if deadline.Before(limit) {
			limit = deadline
		}
		ctx, cancel = context.WithDeadline(g.ctx, limit)
	}
	stop := context.AfterFunc(r.Context(), cancel)
	response, err := g.base.RoundTrip(r.Clone(ctx))
	finish := func() { stop(); cancel() }
	optionalMissing := response != nil && response.StatusCode == 404 && r.Method == "GET" &&
		r.URL.Path == "/"+g.policy.Bucket+"/"+g.policy.Prefix+"kopia.blobcfg"
	if err != nil || response == nil || response.Body == nil || (response.StatusCode < 200 || response.StatusCode >= 300) && !optionalMissing ||
		response.ContentLength < -1 ||
		!optionalMissing && !validResponseRange(r, response) ||
		(r.Method != "HEAD" && response.ContentLength > g.policy.MaxResponseBytes) {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		finish()
		g.mu.Lock()
		g.pending--
		g.failed = true
		g.signalLocked()
		g.mu.Unlock()
		return nil, g.hold()
	}
	g.mu.Lock()
	g.pending--
	active := g.activeLocked() && contextActive(ctx)
	if !active {
		g.failed = true
	}
	if active {
		g.open++
		if optionalMissing {
			g.missing++
		}
	}
	g.mu.Unlock()
	if !active {
		_ = response.Body.Close()
		finish()
		return nil, g.hold()
	}
	expected := response.ContentLength
	if r.Method == "HEAD" {
		expected = 0
	}
	response.Body = &readBody{base: response.Body, guard: g, ctx: ctx, finish: finish, expected: expected, optionalMissing: optionalMissing}
	if optionalMissing {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		closeErr := response.Body.Close()
		var payload struct {
			Code string `xml:"Code"`
		}
		codes := response.Header.Values("X-Amz-Error-Code")
		if readErr != nil || closeErr != nil || len(data) > 64<<10 || !singularXML(data, "Error", false) ||
			xml.Unmarshal(data, &payload) != nil || payload.Code != "NoSuchKey" || len(codes) > 1 || len(codes) == 1 && codes[0] != "NoSuchKey" {
			return nil, g.hold()
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
	}
	return response, nil
}

func validResponseRange(r *http.Request, response *http.Response) bool {
	wanted := r.Header.Values("Range")
	actual := response.Header.Values("Content-Range")
	if len(wanted) == 0 {
		return response.StatusCode == 200 && len(actual) == 0
	}
	if r.Method != "GET" || len(wanted) != 1 || len(actual) != 1 || response.StatusCode != 206 {
		return false
	}
	w := requestedRange.FindStringSubmatch(wanted[0])
	a := returnedRange.FindStringSubmatch(actual[0])
	if len(w) != 3 || len(a) != 4 {
		return false
	}
	var n [5]int64
	for i, s := range []string{w[1], w[2], a[1], a[2], a[3]} {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return false
		}
		n[i] = v
	}
	return n[0] <= n[1] && n[0] == n[2] && n[1] == n[3] && n[4] > n[3] && response.ContentLength == n[1]-n[0]+1
}

// Err is called after repository and response cleanup. An open response is not
// complete evidence; unlike a failed read, it may still finish normally.
func (g *ReadTransport) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.activeLocked() || g.pending != 0 || g.open != 0 || g.missing != 0 || g.operations != 0 {
		return errReadHeld
	}
	return nil
}

// Close cancels the transport's bounded read context.
func (g *ReadTransport) Close() { g.cancel() }

func (g *ReadTransport) beginOperation(ctx context.Context) error {
	g.mu.Lock()
	if !g.activeLocked() || !contextActive(ctx) {
		g.mu.Unlock()
		return g.hold()
	}
	g.operations++
	g.mu.Unlock()
	return nil
}

func (g *ReadTransport) endOperation() {
	g.mu.Lock()
	g.operations--
	g.signalLocked()
	g.mu.Unlock()
}

// Only the storage adapter may acknowledge this response, after MinIO has
// validated a NoSuchKey/404 for the exact optional kopia.blobcfg object. Other
// missing objects, malformed responses and unread bodies remain failures.
func (g *ReadTransport) acceptMissingRetentionConfig() error {
	g.mu.Lock()
	if !g.activeLocked() || g.closedMissing == 0 || g.missing == 0 {
		g.mu.Unlock()
		return g.hold()
	}
	g.closedMissing--
	g.missing--
	g.mu.Unlock()
	return nil
}

type readBody struct {
	base            io.ReadCloser
	guard           *ReadTransport
	ctx             context.Context
	finish          func()
	expected        int64
	read            int64
	eof             bool
	closed          atomic.Bool
	optionalMissing bool
}

func (b *readBody) Read(p []byte) (int, error) {
	g := b.guard
	var reserved int64
	for {
		g.mu.Lock()
		if !g.activeLocked() || !contextActive(b.ctx) || b.closed.Load() {
			g.mu.Unlock()
			return 0, g.hold()
		}
		if len(p) == 0 {
			g.mu.Unlock()
			return 0, nil
		}
		// One shared overflow byte detects an overlong stream without allowing
		// every concurrent reader its own over-budget buffer.
		reserved = min(int64(len(p)), g.policy.MaxResponseBytes-b.read+1, g.policy.MaxBytes-g.bytes-g.reserved+1)
		if reserved > 0 {
			g.reserved += reserved
			g.mu.Unlock()
			break
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-b.ctx.Done():
			return 0, g.hold()
		}
	}
	p = p[:reserved]
	n, err := b.base.Read(p)
	g.mu.Lock()
	g.reserved -= reserved
	b.read += int64(n)
	g.bytes += int64(n)
	if err == io.EOF {
		b.eof = true
	}
	valid := g.activeLocked() && contextActive(b.ctx) && n >= 0 && n <= len(p) && b.read <= g.policy.MaxResponseBytes && g.bytes <= g.policy.MaxBytes &&
		(err != io.EOF || b.expected < 0 || b.read == b.expected)
	g.signalLocked()
	g.mu.Unlock()
	if !valid || (err != nil && err != io.EOF) {
		return 0, g.hold()
	}
	return n, err
}

func (b *readBody) Close() error {
	if b.closed.Swap(true) {
		return nil
	}
	err := b.base.Close()
	g := b.guard
	g.mu.Lock()
	g.open--
	complete := g.activeLocked() && contextActive(b.ctx) && (b.eof || b.expected >= 0 && b.read == b.expected)
	if err == nil && complete && b.optionalMissing {
		g.closedMissing++
	}
	g.mu.Unlock()
	b.finish()
	if err != nil || !complete {
		return g.hold()
	}
	return nil
}
