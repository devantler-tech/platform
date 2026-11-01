package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"
)

type executor func(context.Context, ...string) ([]byte, error)

// No real resource can have this Kubernetes name. The action is also unknown:
// even if every read-only filter failed, these probes cannot mutate a volume.
const absent = "READ-ONLY-PROBE-5649"

var statusLine = regexp.MustCompile(`^HTTP/1\.[01] ([0-9]{3}) [^\r\n]*\r?\n?$`)

func prove(ctx context.Context, run executor) error {
	for i, p := range []struct{ host, port, request, status string }{
		{"127.0.0.1", "8080", "GET /v1/volumes", "200"},
		{"127.0.0.1", "8080", "POST /v1/volumes/" + absent + "?action=UNRECOGNIZED-READ-ONLY-PROBE", "404"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /v1/volumes", "200"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "POST /v1/volumes/" + absent + "?action=UNRECOGNIZED-READ-ONLY-PROBE&action=snapshotList", "403"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /v1/supportbundles/" + absent + "/" + absent + "/download", "403"},
		{"longhorn-frontend.longhorn-system.svc.cluster.local", "80", "GET /v1/unknown-read-probe", "403"},
	} {
		// Raw HTTP never follows redirects, runs no remote output, and exposes
		// only the first status line. Neither target nor request is an input.
		program := fmt.Sprintf("set -eu; printf '%%s\\r\\n' '%s HTTP/1.1' 'Host: longhorn.platform.devantler.tech' 'Connection: close' 'Content-Length: 0' '' | nc -w 10 %s %s | { IFS= read -r status; printf '%%s' \"$status\"; }", p.request, p.host, p.port)
		out, err := run(ctx, "--context=admin@prod", "--request-timeout=20s", "-n", "oauth2-proxy", "exec", "deployment/auth-proxy", "-c", "traefik", "--", "/bin/sh", "-c", program)
		if err != nil {
			return errors.New("fixed dashboard probe transport failed; no response details logged")
		}
		match := statusLine.FindSubmatch(out)
		if len(match) != 2 || string(match[1]) != p.status {
			return fmt.Errorf("fixed dashboard probe %d did not return its required status", i+1)
		}
	}
	return nil
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
	fmt.Println("PASS: dashboard reads succeeded; proxy writes, duplicate-action bypass, diagnostic download and unknown API requests were denied")
}
