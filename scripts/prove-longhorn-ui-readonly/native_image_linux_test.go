package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestPinnedImageProbe catches differences between Bash and the deployed ash,
// timeout and nc. Linux CI runs this without a cluster or ambient credentials.
func TestPinnedImageProbe(t *testing.T) {
	if mode := os.Getenv("LONGHORN_UI_IMAGE_FIXTURE"); mode != "" {
		if mode != "1" || os.Getpid() != 1 {
			t.Fatal("image fixture must be the isolated container entrypoint")
		}
		exercisePinnedImageProbe(t)
		return
	}
	data, err := os.ReadFile("../../k8s/bases/infrastructure/controllers/auth-proxy/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct{ Name, Image string }
				}
			}
		}
	}
	if err = yaml.Unmarshal(data, &deployment); err != nil {
		t.Fatal(err)
	}
	image := ""
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "traefik" {
			if image != "" {
				t.Fatal("ambiguous probe image")
			}
			image = container.Image
		}
	}
	if !regexp.MustCompile(`^docker\.io/library/traefik:v3\.[0-9]+@sha256:[0-9a-f]{64}$`).MatchString(image) {
		t.Fatal("probe image must be the deployed digest-pinned Traefik image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	directory := t.TempDir()
	binary := filepath.Join(directory, "probe.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("compile static image fixture: %v\n%s", buildErr, output)
	}
	// The image receives only the static test binary and its one mode flag.
	// No host network, source tree, Docker socket or kubeconfig is mounted.
	containerIDFile := filepath.Join(directory, "container-id")
	t.Cleanup(func() {
		data, readErr := os.ReadFile(containerIDFile)
		if os.IsNotExist(readErr) {
			return
		}
		id := strings.TrimSpace(string(data))
		if readErr != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
			t.Error("image fixture container identity could not be verified")
			return
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "--force", id).Run()
		left, listErr := exec.CommandContext(cleanupCtx, "docker", "container", "ls", "--all", "--filter=id="+id, "--format={{.ID}}").Output()
		if listErr != nil || strings.TrimSpace(string(left)) != "" {
			t.Error("image fixture container cleanup was not verified")
		}
	})
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "--cidfile="+containerIDFile, "--network", "none",
		"--read-only", "--user", "65532:65532", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--pids-limit", "64", "--memory", "128m",
		"--sysctl", "net.ipv4.ip_unprivileged_port_start=0",
		"--add-host", "longhorn-frontend.longhorn-system.svc.cluster.local:127.0.0.1",
		"--mount", "type=bind,src="+binary+",dst=/probe.test,readonly",
		"--env", "LONGHORN_UI_IMAGE_FIXTURE=1", "--entrypoint", "/probe.test", image,
		"-test.run", "^TestPinnedImageProbe$", "-test.v", "-test.timeout", "60s")
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("deployed image probe fixture: %v\n%s", runErr, output)
	}
}

// exercisePinnedImageProbe preserves both production destinations and all
// command arguments after kubectl; only the HTTP servers are fixtures.
func exercisePinnedImageProbe(t *testing.T) {
	var lock sync.Mutex
	seen := 0
	control := false
	wantRequests := []string{
		"GET /v1/volumes 8080",
		"POST /v1/volumes/" + absent + "?action=UNRECOGNIZED-READ-ONLY-PROBE 8080",
		"GET /v1/volumes 80",
		"POST /v1/volumes/" + absent + "?action=UNRECOGNIZED-READ-ONLY-PROBE&action=snapshotList 80",
		"GET /v1/supportbundles/" + absent + "/" + absent + "/download 80",
		"GET /v1/unknown-read-probe 80",
		"GET / 8080",
		"GET /runtime~main.6d7bda24.js?697c2a38c2a3b317069c 8080",
		"GET /styles.beeabb27.async.js?697c2a38c2a3b317069c 8080",
		"GET /main.5284f33a.async.js?697c2a38c2a3b317069c 8080",
		"GET / 80",
		"GET /runtime~main.6d7bda24.js?697c2a38c2a3b317069c 80",
		"GET /styles.beeabb27.async.js?697c2a38c2a3b317069c 80",
		"GET /main.5284f33a.async.js?697c2a38c2a3b317069c 80",
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		lock.Lock()
		defer lock.Unlock()
		_, port, err := net.SplitHostPort(request.Context().Value(http.LocalAddrContextKey).(net.Addr).String())
		index := seen
		if control {
			index = 0
		}
		if err != nil || index >= len(wantRequests) || request.Method+" "+request.RequestURI+" "+port != wantRequests[index] {
			t.Error("probe changed a fixed request or destination")
		}
		if request.Host != "longhorn.platform.devantler.tech" || request.ContentLength != 0 {
			t.Error("probe changed the public host or sent a body")
		}
		if request.Method != "GET" && (request.Method != "POST" || !strings.Contains(request.URL.Path, absent) || request.URL.Query().Get("action") != "UNRECOGNIZED-READ-ONLY-PROBE") {
			t.Error("probe could issue a real write")
		}
		connection, response, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = connection.Close() }()
		if err = connection.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Error(err)
			return
		}
		// A real premature SHUT_WR is EOF. Keeping the request socket's
		// write side open instead reaches the deadline before the response.
		_, readErr := response.ReadByte()
		var deadline net.Error
		status := 499
		timedOut := errors.As(readErr, &deadline) && deadline.Timeout()
		if control && !errors.Is(readErr, io.EOF) || !control && !timedOut {
			t.Error("socket did not exhibit the expected write-side lifetime")
			return
		}
		if timedOut {
			if control || seen >= 14 {
				t.Error("unexpected extra request")
				return
			}
			status = []int{200, 404, 200, 403, 403, 403, 200, 200, 200, 200, 200, 200, 200, 200}[seen]
		}
		if !control {
			seen++
		}
		_, _ = fmt.Fprintf(response, "HTTP/1.1 %d Fixture\r\nContent-Length: 0\r\nX-Fixture: discarded\r\n", status)
		if strings.HasSuffix(request.URL.Path, ".js") {
			_, _ = response.WriteString("Content-Type: application/javascript\r\n")
		}
		_, _ = response.WriteString("\r\n")
		_ = response.Flush()
	})
	for _, address := range []string{"127.0.0.1:8080", "127.0.0.1:80"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
		t.Cleanup(func() { _ = server.Close() })
		go func() { _ = server.Serve(listener) }()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := prove(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		if !socketProbeArguments(args) {
			return nil, errors.New("unexpected production command")
		}
		if args[23] == "8080" && args[22] != "127.0.0.1" || args[23] == "80" && args[22] != "longhorn-frontend.longhorn-system.svc.cluster.local" {
			return nil, errors.New("production destination was changed")
		}
		return exec.CommandContext(ctx, args[9], args[10:]...).Output()
	})
	lock.Lock()
	count := seen
	control = true
	lock.Unlock()
	if err != nil || count != 14 {
		t.Fatalf("pinned-image requests: seen=%d error=%v", count, err)
	}
	// This is the old production command boundary. Its real BusyBox nc EOF
	// must be detected, so the positive fixture cannot pass vacuously.
	old := exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s\r\n' 'GET /v1/volumes HTTP/1.1' 'Host: longhorn.platform.devantler.tech' 'Content-Length: 0' 'Connection: close' '' | /bin/busybox nc -w 2 127.0.0.1 8080`)
	output, err := old.Output()
	if err != nil || !strings.HasPrefix(string(output), "HTTP/1.1 499 ") {
		t.Fatal("native half-close control did not reproduce the rejected request")
	}
}
