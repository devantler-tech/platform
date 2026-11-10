// Command capture-openbao-replacement captures, from production, the two
// observations the single-server OpenBao replacement procedure (#4970, #5836)
// judges before it replaces anything: each expected server's health read
// through its own port-forward, and one authenticated Raft configuration read.
//
// It changes nothing. It reads pods, mints one short-lived token for the
// workload-less openbao-replacement-observer service account, logs in to the
// Kubernetes-auth role that can read only the Raft configuration, reads it once
// and revokes the OpenBao token. It writes health.json and quorum.json into
// --out for judge-openbao-health and judge-openbao-quorum, which decide; this
// command decides nothing about health or quorum.
//
// Every server's read is bound to the pod UID seen before and after it, so a
// pod replaced mid-capture cannot lend its evidence to its successor. It prints
// one closed status line and never a token, address or response body.
//
// Exit codes: 0 CAPTURED, 2 UNVERIFIED (a read failed or the arguments are
// invalid). There is no HOLD: holding is the judges' verdict.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	namespace       = "openbao"
	kubeContext     = "admin@prod"
	observerAccount = "openbao-replacement-observer"
	activeService   = "service/openbao-active"
	// maxBodyBytes matches the judges' own bound on one observation document.
	maxBodyBytes = 1 << 20
	// The token API refuses durations under ten minutes; the OpenBao token
	// the login returns is bounded separately by the role's TTL.
	tokenDuration = "10m"

	usage = "usage: capture-openbao-replacement --expect-servers openbao-0,openbao-1,openbao-2 --out <private-dir>"
)

var (
	serverName = regexp.MustCompile(`^openbao-[0-9]{1,2}$`)
	forwarding = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:([0-9]{1,5}) -> 8200$`)
)

// stage names the first read that failed; only these words reach the log.
type stage string

func (s stage) Error() string { return string(s) }

const (
	stagePods     stage = "pod-identity"
	stageHealth   stage = "server-health"
	stageIdentity stage = "identity-token"
	stageLogin    stage = "observer-login"
	stageRaft     stage = "raft-configuration"
	stageRevoke   stage = "token-revoke"
	stageWrite    stage = "private-capture"
)

// cluster is everything the command asks of production, so the regressions
// can stand in for kubectl and the servers behind each port-forward.
type cluster interface {
	podUID(ctx context.Context, name string) (string, error)
	createToken(ctx context.Context) (string, error)
	// forward returns the base URL of a local port-forward to target and a
	// function that ends it.
	forward(ctx context.Context, target string) (string, func(), error)
	client() *http.Client
}

type healthObservation struct {
	Name         string          `json:"name"`
	PodUID       string          `json:"podUID"`
	HealthStatus int             `json:"healthStatus"`
	Health       json.RawMessage `json:"health"`
}

type healthCapture struct {
	StartedAt  string              `json:"startedAt"`
	CapturedAt string              `json:"capturedAt"`
	Servers    []healthObservation `json:"servers"`
}

type quorumCapture struct {
	StartedAt     string          `json:"startedAt"`
	CapturedAt    string          `json:"capturedAt"`
	Status        int             `json:"status"`
	Configuration json.RawMessage `json:"configuration"`
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// request performs one bounded read and returns its status and body. A body
// over the bound is a failed read, never a truncated one.
func request(ctx context.Context, c *http.Client, method, url, token string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes+1))
	if err != nil || len(b) > maxBodyBytes {
		return 0, nil, errors.New("unreadable response")
	}
	return res.StatusCode, b, nil
}

// captureHealth reads every server's health through a port-forward to that
// pod alone, so a body always comes from the server it is filed under.
func captureHealth(ctx context.Context, k cluster, servers []string, now func() time.Time) (healthCapture, error) {
	before := make(map[string]string, len(servers))
	for _, name := range servers {
		uid, err := k.podUID(ctx, name)
		if err != nil || uid == "" {
			return healthCapture{}, stagePods
		}
		before[name] = uid
	}
	capture := healthCapture{StartedAt: stamp(now())}
	for _, name := range servers {
		base, stop, err := k.forward(ctx, "pod/"+name)
		if err != nil {
			return healthCapture{}, stageHealth
		}
		status, body, err := request(ctx, k.client(), http.MethodGet, base+"/v1/sys/health?standbyok=true", "", nil)
		stop()
		if err != nil || !json.Valid(body) {
			return healthCapture{}, stageHealth
		}
		capture.Servers = append(capture.Servers, healthObservation{Name: name, PodUID: before[name], HealthStatus: status, Health: body})
	}
	capture.CapturedAt = stamp(now())
	for _, name := range servers {
		if uid, err := k.podUID(ctx, name); err != nil || uid != before[name] {
			return healthCapture{}, stagePods
		}
	}
	return capture, nil
}

// captureQuorum logs the observer in through the active server, reads the Raft
// configuration once and revokes the token before reporting anything.
func captureQuorum(ctx context.Context, k cluster, now func() time.Time) (capture quorumCapture, err error) {
	jwt, err := k.createToken(ctx)
	if err != nil || strings.TrimSpace(jwt) == "" {
		return quorumCapture{}, stageIdentity
	}
	base, stop, err := k.forward(ctx, activeService)
	if err != nil {
		return quorumCapture{}, stageLogin
	}
	defer stop()
	login, _ := json.Marshal(map[string]string{"role": observerAccount, "jwt": strings.TrimSpace(jwt)})
	status, body, err := request(ctx, k.client(), http.MethodPost, base+"/v1/auth/kubernetes/login", "", login)
	var auth struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &auth) != nil || auth.Auth.ClientToken == "" {
		return quorumCapture{}, stageLogin
	}
	token := auth.Auth.ClientToken
	defer func() {
		// A capture whose token outlives it is not reported as captured.
		status, _, revokeErr := request(context.WithoutCancel(ctx), k.client(), http.MethodPost, base+"/v1/auth/token/revoke-self", token, []byte("{}"))
		if err == nil && (revokeErr != nil || status != http.StatusNoContent) {
			capture, err = quorumCapture{}, stageRevoke
		}
	}()
	capture.StartedAt = stamp(now())
	status, body, err = request(ctx, k.client(), http.MethodGet, base+"/v1/sys/storage/raft/configuration", token, nil)
	if err != nil || !json.Valid(body) {
		return quorumCapture{}, stageRaft
	}
	capture.CapturedAt = stamp(now())
	capture.Status, capture.Configuration = status, body
	return capture, nil
}

func writePrivate(dir, name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), b, 0o600)
}

func parseServers(list string) ([]string, bool) {
	servers := strings.Split(list, ",")
	seen := map[string]bool{}
	for _, s := range servers {
		if !serverName.MatchString(s) || seen[s] {
			return nil, false
		}
		seen[s] = true
	}
	return servers, len(servers) >= 3
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, k cluster, now func() time.Time) int {
	flags := flag.NewFlagSet("capture-openbao-replacement", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	list := flags.String("expect-servers", "", "comma-separated server names from the reviewed render")
	out := flags.String("out", "", "existing private directory for health.json and quorum.json")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	servers, ok := parseServers(*list)
	info, err := os.Stat(*out)
	if !ok || *out == "" || err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	fail := func(s error) int {
		_, _ = fmt.Fprintf(stdout, "OPENBAO_REPLACEMENT_EVIDENCE=UNVERIFIED stage=%s\n", s)
		return 2
	}
	health, err := captureHealth(ctx, k, servers, now)
	if err != nil {
		return fail(err)
	}
	quorum, err := captureQuorum(ctx, k, now)
	if err != nil {
		return fail(err)
	}
	if writePrivate(*out, "health.json", health) != nil || writePrivate(*out, "quorum.json", quorum) != nil {
		return fail(stageWrite)
	}
	_, _ = fmt.Fprintln(stdout, "OPENBAO_REPLACEMENT_EVIDENCE=CAPTURED")
	return 0
}

// kubectl is the production cluster, reached with the protected kubeconfig.
type kubectl struct{ http *http.Client }

func (kubectl) args(rest ...string) []string {
	return append([]string{"--context=" + kubeContext, "--request-timeout=20s", "--namespace=" + namespace}, rest...)
}

func (k kubectl) output(ctx context.Context, rest ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "kubectl", k.args(rest...)...)
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	err := cmd.Run()
	return out.Bytes(), err
}

func (k kubectl) podUID(ctx context.Context, name string) (string, error) {
	b, err := k.output(ctx, "get", "pod", name, "--output=jsonpath={.metadata.uid}")
	return strings.TrimSpace(string(b)), err
}

func (k kubectl) createToken(ctx context.Context) (string, error) {
	b, err := k.output(ctx, "create", "token", observerAccount, "--duration="+tokenDuration)
	return string(b), err
}

func (k kubectl) forward(ctx context.Context, target string) (string, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "kubectl", k.args("port-forward", "--address=127.0.0.1", target, ":8200")...)
	cmd.Stderr = io.Discard
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, err
	}
	stop := func() { cancel(); _ = cmd.Wait() }
	port := make(chan string, 1)
	go func() {
		lines := bufio.NewScanner(pipe)
		for lines.Scan() {
			if m := forwarding.FindStringSubmatch(lines.Text()); m != nil {
				port <- m[1]
				break
			}
		}
		_, _ = io.Copy(io.Discard, pipe)
	}()
	select {
	case p := <-port:
		return "http://127.0.0.1:" + p, stop, nil
	case <-time.After(20 * time.Second):
		stop()
		return "", nil, errors.New("port-forward did not start")
	}
}

func (k kubectl) client() *http.Client { return k.http }

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, kubectl{http: c}, time.Now))
}
