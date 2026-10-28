// This integration fixture runs only on a disposable Linux CI runner. It uses
// the chart's immutable frontend image and never contacts a Kubernetes cluster.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
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
		early        bool
	}{{"original-buffering-control", baseline, false}, {"streaming-correction", candidate, true}} {
		if err := exercise(ctx, image, fixture.config, fixture.early); err != nil {
			return fmt.Errorf("%s: %w", fixture.name, err)
		}
		fmt.Printf("PASS: %s preserves requests, complete responses, health and upstream failures\n", fixture.name)
	}
	return nil
}

func exercise(ctx context.Context, image, config string, wantEarly bool) (resultErr error) {
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
	payload := bytes.Repeat([]byte("complete-stream-fixture\n"), 128*1024)
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
	client := &http.Client{Timeout: 15 * time.Second}
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
	if readErr != nil || index.StatusCode != http.StatusOK || !bytes.Contains(bytes.ToLower(indexBody), []byte("<html")) {
		return errors.New("the ordinary UI frontend no longer serves its HTML entry point")
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
	rest, err := io.ReadAll(io.LimitReader(observed.response.Body, int64(len(payload)+1)))
	if err != nil || !bytes.Equal(rest, payload) {
		return fmt.Errorf("the complete large response was not preserved: %v", err)
	}
	if early != wantEarly {
		return fmt.Errorf("first chunk before upstream completion: got %t, want %t", early, wantEarly)
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
	if wantEarly {
		logs, err := docker(ctx, "logs", name)
		if err != nil || bytes.Contains(logs, []byte("buffered to a temporary file")) {
			return fmt.Errorf("streaming configuration still spilled its response: %v", err)
		}
	}
	return nil
}
