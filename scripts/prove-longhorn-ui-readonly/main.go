package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type executor func(context.Context, ...string) ([]byte, error)

// No real resource can have this Kubernetes name. The action is also unknown:
// even if every read-only filter failed, these probes cannot mutate a volume.
const absent = "READ-ONLY-PROBE-5649"

var statusLine = regexp.MustCompile(`^HTTP/1\.[01] ([1-5][0-9]{2}) [^\r\n]*\r?\n?$`)

// prove checks fixed read-only boundaries and collects sanitized failures.
func prove(ctx context.Context, run executor) error {
	var failures []error
	for i, p := range []struct{ host, port, request, status string }{
		{"127.0.0.1", "8080", "GET /v1/volumes", "200"},
		{"127.0.0.1", "8080", "POST /v1/volumes/" + absent + "?action=UNRECOGNIZED-READ-ONLY-PROBE", "404"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /v1/volumes", "200"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "POST /v1/volumes/" + absent + "?action=UNRECOGNIZED-READ-ONLY-PROBE&action=snapshotList", "403"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /v1/supportbundles/" + absent + "/" + absent + "/download", "403"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /v1/unknown-read-probe", "403"},
		// These filenames come from the pinned Longhorn 1.13.0 UI document.
		// Check the browser's exact paths, including its cache query, through
		// both boundaries; a successful API read alone cannot render the UI.
		{"127.0.0.1", "8080", "GET /", "200"},
		{"127.0.0.1", "8080", "GET /runtime~main.6d7bda24.js?697c2a38c2a3b317069c", "200"},
		{"127.0.0.1", "8080", "GET /styles.beeabb27.async.js?697c2a38c2a3b317069c", "200"},
		{"127.0.0.1", "8080", "GET /main.5284f33a.async.js?697c2a38c2a3b317069c", "200"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /", "200"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /runtime~main.6d7bda24.js?697c2a38c2a3b317069c", "200"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /styles.beeabb27.async.js?697c2a38c2a3b317069c", "200"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /main.5284f33a.async.js?697c2a38c2a3b317069c", "200"},
	} {
		// Raw HTTP never follows redirects or executes response data. Neither
		// target nor request is an input. Only script responses need MIME data;
		// bound their input and discard every other header and the entire body.
		javascript := strings.Contains(p.request, ".js?")
		program := fmt.Sprintf("set -eu; printf '%%s\\r\\n' '%s HTTP/1.1' 'Host: longhorn.platform.devantler.tech' 'Connection: close' 'Content-Length: 0' '' | nc -w 10 %s %s", p.request, p.host, p.port)
		if javascript {
			program += ` | head -c 65536 | { IFS= read -r status; printf '%s\n' "$status"; while IFS= read -r header; do case "$header" in ''|"$(printf '\r')") printf '\n'; break;; [Cc][Oo][Nn][Tt][Ee][Nn][Tt]-[Tt][Yy][Pp][Ee]:*) printf '%s\n' "$header";; esac; done; }`
		} else {
			program += ` | { IFS= read -r status; printf '%s' "$status"; }`
		}
		out, err := run(ctx, "--context=admin@prod", "--request-timeout=20s", "-n", "oauth2-proxy", "exec", "deployment/auth-proxy", "-c", "traefik", "--", "/bin/sh", "-c", program)
		if err != nil {
			failures = append(failures, fmt.Errorf("fixed dashboard probe %d transport failed; no response details logged", i+1))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		status := out
		if javascript {
			status = bytes.SplitN(out, []byte("\n"), 2)[0]
		}
		match := statusLine.FindSubmatch(status)
		if len(match) != 2 {
			failures = append(failures, fmt.Errorf("fixed dashboard probe %d returned no valid HTTP status line", i+1))
			continue
		}
		if string(match[1]) != p.status {
			// Only a validated three-digit code may leave the process. Never
			// include the response reason, headers, body, or command error.
			failures = append(failures, fmt.Errorf("fixed dashboard probe %d: expected HTTP %s, received HTTP %s", i+1, p.status, match[1]))
			continue
		}
		if javascript && !javascriptContentType(out) {
			failures = append(failures, fmt.Errorf("fixed dashboard probe %d did not return a valid JavaScript content type; no headers or contents logged", i+1))
		}
	}
	return errors.Join(failures...)
}

// javascriptContentType rejects HTML fallbacks without revealing response data.
func javascriptContentType(out []byte) bool {
	if len(out) > 65536 {
		return false
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), nil)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	if len(response.Header.Values("Content-Type")) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	return err == nil && (mediaType == "application/javascript" || mediaType == "text/javascript")
}

func main() {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_REF") != "refs/heads/main" || os.Getenv("GITHUB_RUN_ATTEMPT") != "1" || os.Getenv("LONGHORN_UI_READ_PROOF_CONFIRM") != "prove-longhorn-ui-readonly" {
		fmt.Fprintln(os.Stderr, "refusing probe outside the first confirmed reviewed-main dispatch")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err := prove(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		// Do not forward kubectl stderr: it can contain credentials or topology.
		return exec.CommandContext(ctx, "kubectl", args...).Output()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("PASS: dashboard API and JavaScript response checks succeeded; proxy writes, duplicate-action bypass, diagnostic download and unknown API requests were denied")
}
