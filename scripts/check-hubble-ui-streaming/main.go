// This integration fixture runs only on a disposable Linux CI runner. It uses
// the chart's immutable frontend image and never contacts a Kubernetes cluster.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const prefix = "fixture-stream-prefix\n"

var imagePattern = regexp.MustCompile(`^quay\.io/cilium/hubble-ui:[^@]+@sha256:[0-9a-f]{64}$`)

func main() {
	image := flag.String("image", "", "immutable chart frontend image")
	baseline := flag.String("baseline", "", "original rendered nginx configuration")
	candidate := flag.String("candidate", "", "post-rendered nginx configuration")
	flag.Parse()
	if err := run(*image, *baseline, *candidate); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	// The fixed local runner socket prevents ambient remote Docker contexts from
	// selecting a different machine. No privileged container or host mount is used.
	cmd := exec.CommandContext(ctx, "docker", append([]string{"--host", "unix:///var/run/docker.sock"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("docker %s failed: %w: %.1500s", args[0], err, out)
	}
	return out, nil
}

func run(image, baseline, candidate string) error {
	if runtime.GOOS != "linux" || !imagePattern.MatchString(image) {
		return errors.New("the runtime fixture requires Linux and the chart's immutable upstream frontend image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := docker(ctx, "pull", image); err != nil {
		return err
	}
	for _, fixture := range []struct {
		name, config string
		spool        bool
	}{{"original-buffering-control", baseline, true}, {"streaming-correction", candidate, false}} {
		if err := exercise(ctx, image, fixture.config, fixture.spool); err != nil {
			return fmt.Errorf("%s: %w", fixture.name, err)
		}
		fmt.Printf("PASS: %s preserves requests, complete responses, health and upstream failures\n", fixture.name)
	}
	return nil
}

func exercise(ctx context.Context, image, config string, wantSpool bool) (resultErr error) {
	config, err := filepath.Abs(config)
	if err != nil {
		return err
	}
	info, err := os.Stat(config)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || strings.ContainsAny(config, ",\n\r") {
		return errors.New("fixture configuration must be a regular file with an unambiguous bind-mount path")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:8090")
	if err != nil {
		return fmt.Errorf("the fixed fixture upstream port is unavailable: %w", err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	requestErrors := make(chan error, 2)
	payload := bytes.Repeat([]byte("complete-stream-fixture\n"), (32*1024*1024)/len("complete-stream-fixture\n"))
	upstream := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 4096))
		if readErr != nil || r.Method != http.MethodPost || string(body) != `{"fixture":true}` ||
			r.Host != "fixture.invalid" || r.Header.Get("X-Fixture") != "forwarded" || r.Header.Get("X-Real-IP") == "" || r.Header.Get("Content-Type") != "application/json" {
			requestErrors <- fmt.Errorf("request method, body or forwarded headers changed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/api/fixture-error" {
			w.Header().Set("X-Fixture-Response", "preserved")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "fixture upstream unavailable")
			return
		}
		if r.URL.Path != "/api/service-map-stream" {
			requestErrors <- errors.New("API routing changed")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, prefix)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = w.Write(payload)
		case <-r.Context().Done():
		}
	})}
	go func() { _ = upstream.Serve(listener) }()
	defer func() {
		if err := upstream.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("fixture upstream cleanup: %w", err))
		}
	}()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	name := "hubble-stream-fixture-" + hex.EncodeToString(nonce)
	if _, err := docker(ctx, "run", "--detach", "--rm", "--name", name, "--network", "host",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--mount",
		"type=bind,source="+config+",target=/etc/nginx/conf.d/default.conf,readonly", image); err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := docker(cleanup, "rm", "--force", name); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("fixture container cleanup: %w", err))
		}
	}()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		// Bound the downstream receive window so the original proxy cannot hide
		// the whole response in the client socket during the deliberate pause.
		if err := conn.(*net.TCPConn).SetReadBuffer(128 * 1024); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("bound fixture receive buffer: %w", err)
		}
		return conn, nil
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get("http://127.0.0.1:8081/healthz")
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && string(body) == "ok" {
				ready = true
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		logs, _ := docker(ctx, "logs", name)
		return fmt.Errorf("frontend did not become healthy: %.1500s", logs)
	}
	index, err := client.Get("http://127.0.0.1:8081/")
	if err != nil {
		return err
	}
	indexBody, readErr := io.ReadAll(io.LimitReader(index.Body, 1024*1024))
	_ = index.Body.Close()
	if !validFrontendIndex(index.StatusCode, index.Header.Get("Content-Type"), indexBody, readErr) {
		return fmt.Errorf("frontend entry point did not match the pinned image: status=%d content-type=%q", index.StatusCode, index.Header.Get("Content-Type"))
	}
	request := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8081"+path, strings.NewReader(`{"fixture":true}`))
		if err != nil {
			return nil, err
		}
		req.Host = "fixture.invalid"
		req.Header.Set("X-Fixture", "forwarded")
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}
	type firstChunk struct {
		response *http.Response
		body     []byte
		err      error
	}
	first := make(chan firstChunk, 1)
	go func() {
		resp, err := request("/api/service-map-stream")
		chunk := make([]byte, len(prefix))
		if err == nil {
			_, err = io.ReadFull(resp.Body, chunk)
		}
		first <- firstChunk{resp, chunk, err}
	}()
	var observed firstChunk
	early := false
	select {
	case observed = <-first:
		early = true
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	finish()
	if !early {
		select {
		case observed = <-first:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if observed.response != nil {
		defer observed.response.Body.Close()
	}
	if observed.err != nil || string(observed.body) != prefix || observed.response.StatusCode != http.StatusOK {
		return fmt.Errorf("stream prefix or response changed: %v", observed.err)
	}
	if !early {
		return errors.New("first chunk did not arrive before upstream completion")
	}
	// Both configurations can flush a short prefix promptly. The reported
	// regression is disk spooling when the producer outruns the reader: release
	// the large body, pause downstream reads, then verify every response byte.
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	rest, err := io.ReadAll(io.LimitReader(observed.response.Body, int64(len(payload)+1)))
	if err != nil || !bytes.Equal(rest, payload) {
		return fmt.Errorf("the complete large response was not preserved: %v", err)
	}
	resp, err := request("/api/fixture-error")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || string(body) != "fixture upstream unavailable" || resp.Header.Get("X-Fixture-Response") != "preserved" {
		return fmt.Errorf("upstream failure propagation changed: %v", err)
	}
	select {
	case err := <-requestErrors:
		return err
	default:
	}
	logs, err := docker(ctx, "logs", name)
	if err != nil {
		return err
	}
	if spooled := bytes.Contains(logs, []byte("buffered to a temporary file")); spooled != wantSpool {
		return fmt.Errorf("temporary-file buffering under a slow reader: got %t, want %t", spooled, wantSpool)
	}
	return nil
}

func validFrontendIndex(status int, contentType string, body []byte, readErr error) bool {
	if readErr != nil || status != http.StatusOK {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "text/html" {
		return false
	}
	// /app/index.html from the chart's immutable v0.13.6 frontend image
	// (sha256:049a80a03585c043d0c3121bdc518c72b0fd42bae07e4bcce0fdb8f1e88041d0).
	// The released minifier omits the optional opening html tag. Verify its
	// exact entry point instead of accepting an arbitrary HTML-looking body.
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]) == "c93d8bfc9217a3fb3d91a7a1672191641efe7a6aa046dfd7f03818fc7f3a87ff"
}
