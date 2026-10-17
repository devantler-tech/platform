// Package native_test exercises the OpenBao health judge against real servers.
//
// It starts three disposable servers of the pinned release on loopback, from
// the declared Raft configuration, and drives them through the states a
// single-server replacement meets: cold start, bootstrap, a sealed restart, a
// leader change, an unreachable peer, a follower joining and an uninitialized
// server. Each state's real health responses are captured in the judge's
// documented format and the judge must return the expected verdict (#4969).
//
// The store, unseal key and root token are synthetic and live only in this
// process; nothing is logged that a server returns. It proves server behaviour
// only: not production storage, quorum, or a Kubernetes rollout.
package native_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	release = "2.6.3"
	// waitLimit bounds every wait for a server state. Raft elections and
	// retry_join attempts settle in seconds on loopback.
	waitLimit = 90 * time.Second
)

var serverNames = []string{"openbao-0", "openbao-1", "openbao-2"}

type server struct {
	name        string
	apiAddr     string
	clusterAddr string
	root        string
	generation  int
	command     *exec.Cmd
	exited      chan struct{}
	log         *os.File
}

type cluster struct {
	t        *testing.T
	binary   string
	judge    string
	root     string
	declared string
	servers  map[string]*server
	client   *http.Client
	// Synthetic credentials of this disposable store; never logged.
	unsealKey string
	rootToken string
}

type judgeFinding struct {
	Server string `json:"server"`
	Reason string `json:"reason"`
}

type judgeReport struct {
	Verdict  string         `json:"verdict"`
	Findings []judgeFinding `json:"findings"`
	Scope    string         `json:"scope"`
}

type capturedServer struct {
	Name         string          `json:"name"`
	PodUID       string          `json:"podUID"`
	HealthStatus *int            `json:"healthStatus"`
	Health       json.RawMessage `json:"health"`
}

type capture struct {
	StartedAt  string           `json:"startedAt"`
	CapturedAt string           `json:"capturedAt"`
	Servers    []capturedServer `json:"servers"`
}

type healthBody struct {
	Initialized bool `json:"initialized"`
	Sealed      bool `json:"sealed"`
	Standby     bool `json:"standby"`
}

func TestJudgeVerdictsAgainstNativeServers(t *testing.T) {
	binary := os.Getenv("OPENBAO_HEALTH_TEST_BINARY")
	if binary == "" {
		t.Skip("the native binary is supplied by the mandatory OpenBao chart regression")
	}
	version, err := exec.Command(binary, "version").Output()
	if err != nil || !strings.HasPrefix(string(version), "OpenBao v"+release+" ") {
		t.Fatalf("expected the verified OpenBao %s binary", release)
	}
	t.Log("scope: native server health behaviour only; not production storage, quorum or a Kubernetes rollout")

	c := newCluster(t, binary)

	c.step("cold start: no server is initialized", func() {
		for _, name := range serverNames {
			c.start(name, true)
		}
		for _, name := range serverNames {
			c.waitHealth(name, func(status int, _ healthBody) bool { return status == http.StatusNotImplemented })
		}
		c.expect("HOLD", map[string]string{
			"openbao-0": "server is not initialized",
			"openbao-1": "server is not initialized",
			"openbao-2": "server is not initialized",
		})
	})

	c.step("bootstrap: initialize the first server, then unseal every peer", func() {
		c.initialize("openbao-0")
		c.unsealAll()
		c.waitAllHealthy()
		c.expect("PROVEN", nil)
	})

	c.step("one server sealed after a restart", func() {
		c.stop("openbao-2")
		c.start("openbao-2", true)
		c.waitHealth("openbao-2", func(_ int, body healthBody) bool { return body.Initialized && body.Sealed })
		c.expect("HOLD", map[string]string{"openbao-2": "server is sealed"})
		c.unsealAll()
		c.waitAllHealthy()
		c.expect("PROVEN", nil)
	})

	c.step("after a leader change", func() {
		// Autopilot admits a joined server as a voter only after it has been
		// stable for a while; until then the first server is the only voter and
		// no other server can win an election.
		c.waitVoters(serverNames)
		before := c.active()
		c.stepDown(before)
		// A stepped-down server may win the next election again, so repeat the
		// step-down until another server holds the active role.
		lastStepDown := time.Now()
		c.waitUntil("a different server is active", func() bool {
			after, ok := c.activeIfSettled()
			if ok && after == before && time.Since(lastStepDown) > 5*time.Second {
				c.stepDown(before)
				lastStepDown = time.Now()
			}
			return ok && after != before
		})
		c.expect("PROVEN", nil)
	})

	c.step("one server unreachable", func() {
		c.stop("openbao-2")
		c.waitUntil("the remaining servers are unsealed with exactly one active", func() bool {
			_, ok := c.activeAmong([]string{"openbao-0", "openbao-1"})
			return ok
		})
		c.expect("UNKNOWN", map[string]string{"openbao-2": "no health evidence"})
	})

	c.step("a follower joining with empty storage", func() {
		c.removePeer("openbao-2")
		c.wipe("openbao-2")
		c.start("openbao-2", true)
		c.waitHealth("openbao-2", func(status int, _ healthBody) bool { return status != 0 })
		report := c.capture()
		if report.Verdict != "HOLD" || !onlyServer(report, "openbao-2", "server is not initialized", "server is sealed") {
			t.Fatalf("joining follower: want HOLD on openbao-2 only, got %+v", report)
		}
		t.Logf("joining follower reported %q", report.Findings[0].Reason)
		c.unsealAll()
		c.waitAllHealthy()
		c.expect("PROVEN", nil)
	})

	c.step("one server uninitialized and not joining", func() {
		c.stop("openbao-2")
		c.removePeer("openbao-2")
		c.wipe("openbao-2")
		c.start("openbao-2", false)
		c.waitHealth("openbao-2", func(status int, _ healthBody) bool { return status == http.StatusNotImplemented })
		c.expect("HOLD", map[string]string{"openbao-2": "server is not initialized"})
	})
}

func newCluster(t *testing.T, binary string) *cluster {
	t.Helper()
	root := t.TempDir()
	judge := filepath.Join(root, "judge-openbao-health")
	build := exec.Command("go", "build", "-o", judge, "../../judge-openbao-health")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the judge: %v\n%s", err, output)
	}
	c := &cluster{
		t:        t,
		binary:   binary,
		judge:    judge,
		root:     root,
		declared: declaredRaftConfig(t),
		servers:  map[string]*server{},
		client:   &http.Client{Timeout: 30 * time.Second},
	}
	for _, name := range serverNames {
		c.servers[name] = &server{name: name, apiAddr: freeAddr(t), clusterAddr: freeAddr(t)}
	}
	t.Cleanup(func() {
		for _, name := range serverNames {
			c.stop(name)
		}
	})
	return c
}

func declaredRaftConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../../k8s/bases/infrastructure/controllers/openbao/helm-release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Spec struct {
			Values struct {
				Server struct {
					HA struct {
						Raft struct{ Config string }
					} `yaml:"ha"`
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document.Spec.Values.Server.HA.Raft.Config == "" {
		t.Fatal("the declared Raft configuration is missing")
	}
	return document.Spec.Values.Server.HA.Raft.Config
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

var retryJoin = regexp.MustCompile(`(?m)^[ \t]*retry_join \{[^}\n]*\}[ \t]*\n`)

// config renders the declared configuration for one local server. Only
// addresses, paths and the in-cluster service registration are substituted, and
// every substitution must match, so a changed declaration fails here.
func (c *cluster) config(s *server, join bool) string {
	c.t.Helper()
	replacements := []string{
		"0.0.0.0:8200", s.apiAddr,
		"0.0.0.0:8201", s.clusterAddr,
		"/openbao/data", filepath.ToSlash(filepath.Join(s.root, "data")),
		// The audit device is stored in the shared Raft log, so every server must
		// declare the identical path, as every production pod does. A relative path
		// resolves inside each server's own working directory.
		"/openbao/audit/audit.log", "audit.log",
		`service_registration "kubernetes" {}`, "",
	}
	for _, name := range serverNames {
		replacements = append(replacements,
			"http://"+name+".openbao-internal:8200", "http://"+c.servers[name].apiAddr)
	}
	for i := 0; i < len(replacements); i += 2 {
		if !strings.Contains(c.declared, replacements[i]) {
			c.t.Fatalf("the declared Raft configuration no longer contains %q", replacements[i])
		}
	}
	rendered := strings.NewReplacer(replacements...).Replace(c.declared)
	if !join {
		without := retryJoin.ReplaceAllString(rendered, "")
		if strings.Contains(without, "retry_join") || without == rendered {
			c.t.Fatal("could not remove the retry_join stanzas from the declared configuration")
		}
		rendered = without
	}
	return rendered + fmt.Sprintf("\napi_addr = %q\ncluster_addr = %q\n", "http://"+s.apiAddr, "https://"+s.clusterAddr)
}

func (c *cluster) start(name string, join bool) {
	c.t.Helper()
	s := c.servers[name]
	s.generation++
	if s.root == "" {
		s.root = filepath.Join(c.root, name)
	}
	if err := os.MkdirAll(filepath.Join(s.root, "data"), 0o700); err != nil {
		c.t.Fatal(err)
	}
	path := filepath.Join(s.root, "config.hcl")
	if err := os.WriteFile(path, []byte(c.config(s, join)), 0o600); err != nil {
		c.t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(s.root, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		c.t.Fatal(err)
	}
	command := exec.Command(c.binary, "server", "-config="+path)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.root, "BAO_RAFT_NODE_ID=" + name}
	command.Dir = s.root
	command.Stdout = log
	command.Stderr = log
	if err := command.Start(); err != nil {
		c.t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	s.command, s.exited, s.log = command, exited, log
}

func (c *cluster) stop(name string) {
	s := c.servers[name]
	if s.command == nil {
		return
	}
	_ = s.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(30 * time.Second):
		_ = s.command.Process.Kill()
		<-s.exited
	}
	_ = s.log.Close()
	s.command = nil
}

func (c *cluster) wipe(name string) {
	c.t.Helper()
	if err := os.RemoveAll(filepath.Join(c.servers[name].root, "data")); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cluster) step(name string, run func()) {
	c.t.Helper()
	c.t.Logf("state: %s", name)
	// Deferred so the dump also runs when t.Fatal unwinds the step.
	defer func() {
		if c.t.Failed() {
			c.dumpLogs()
		}
	}()
	run()
}

// dumpLogs prints the tail of each server log after a failure. Server logs
// carry no unseal key or token.
func (c *cluster) dumpLogs() {
	for _, name := range serverNames {
		data, err := os.ReadFile(filepath.Join(c.root, name, "server.log"))
		if err != nil {
			continue
		}
		if len(data) > 4000 {
			data = data[len(data)-4000:]
		}
		c.t.Logf("%s log tail:\n%s", name, data)
	}
}

// request sends one API call and returns the status and body. A transport
// failure returns status 0.
func (c *cluster) request(name, method, path string, payload any, token bool) (int, []byte) {
	c.t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			c.t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, "http://"+c.servers[name].apiAddr+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if token {
		request.Header.Set("X-Vault-Token", c.rootToken)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, nil
	}
	return response.StatusCode, data
}

func (c *cluster) health(name string) (int, healthBody, []byte) {
	if c.servers[name].command == nil {
		return 0, healthBody{}, nil
	}
	status, data := c.request(name, http.MethodGet, "/v1/sys/health?standbyok=true", nil, false)
	var body healthBody
	if status == 0 || json.Unmarshal(data, &body) != nil {
		return 0, healthBody{}, nil
	}
	return status, body, data
}

func (c *cluster) waitUntil(what string, ready func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !ready() {
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (c *cluster) waitHealth(name string, ready func(int, healthBody) bool) {
	c.t.Helper()
	c.waitUntil(name+" reports the expected health", func() bool {
		status, body, _ := c.health(name)
		return status != 0 && ready(status, body)
	})
}

// activeIfSettled returns the one active server when every server answers
// healthy, unsealed and initialized with exactly one active.
func (c *cluster) activeIfSettled() (string, bool) {
	return c.activeAmong(serverNames)
}

// activeAmong returns the one active server when every named server answers
// healthy, unsealed and initialized with exactly one active.
func (c *cluster) activeAmong(names []string) (string, bool) {
	active := ""
	for _, name := range names {
		status, body, _ := c.health(name)
		if status != http.StatusOK || !body.Initialized || body.Sealed {
			return "", false
		}
		if !body.Standby {
			if active != "" {
				return "", false
			}
			active = name
		}
	}
	return active, active != ""
}

func (c *cluster) waitAllHealthy() {
	c.t.Helper()
	c.waitUntil("every server is unsealed with exactly one active", func() bool {
		_, ok := c.activeIfSettled()
		return ok
	})
}

func (c *cluster) active() string {
	c.t.Helper()
	name, ok := c.activeIfSettled()
	if !ok {
		c.t.Fatal("the cluster has not settled on one active server")
	}
	return name
}

func (c *cluster) initialize(name string) {
	c.t.Helper()
	status, data := c.request(name, http.MethodPut, "/v1/sys/init",
		map[string]int{"secret_shares": 1, "secret_threshold": 1}, false)
	var result struct {
		Keys      []string `json:"keys"`
		RootToken string   `json:"root_token"`
	}
	if status != http.StatusOK || json.Unmarshal(data, &result) != nil || len(result.Keys) != 1 || result.RootToken == "" {
		c.t.Fatalf("initializing %s failed with status %d", name, status)
	}
	c.unsealKey, c.rootToken = result.Keys[0], result.RootToken
}

// unsealAll unseals every running sealed server until none is sealed, the way
// the bootstrap job does: a follower joins once a leader is unsealed.
func (c *cluster) unsealAll() {
	c.t.Helper()
	c.waitUntil("every running server is unsealed", func() bool {
		done := true
		for _, name := range serverNames {
			if c.servers[name].command == nil {
				continue
			}
			status, body, _ := c.health(name)
			if status != 0 && body.Initialized && !body.Sealed {
				continue
			}
			done = false
			if status != 0 {
				_, _ = c.request(name, http.MethodPut, "/v1/sys/unseal", map[string]string{"key": c.unsealKey}, false)
			}
		}
		return done
	})
}

func (c *cluster) stepDown(name string) {
	c.t.Helper()
	if status, _ := c.request(name, http.MethodPut, "/v1/sys/step-down", nil, true); status != http.StatusNoContent && status != http.StatusOK {
		c.t.Fatalf("step-down on %s failed with status %d", name, status)
	}
}

// removePeer removes a server from the Raft configuration through the active
// server, as a replacement must before the server rejoins with empty storage.
func (c *cluster) removePeer(name string) {
	c.t.Helper()
	leader := ""
	c.waitUntil("a running server is active", func() bool {
		for _, candidate := range serverNames {
			if candidate == name || c.servers[candidate].command == nil {
				continue
			}
			if status, body, _ := c.health(candidate); status == http.StatusOK && !body.Standby && !body.Sealed {
				leader = candidate
				return true
			}
		}
		return false
	})
	status, _ := c.request(leader, http.MethodPost, "/v1/sys/storage/raft/remove-peer",
		map[string]string{"server_id": name}, true)
	if status != http.StatusNoContent && status != http.StatusOK {
		c.t.Fatalf("removing %s from the Raft configuration failed with status %d", name, status)
	}
}

// capture reads every server's health in the judge's documented format and
// returns the judge's report, checking that its exit status matches the verdict.
func (c *cluster) capture() judgeReport {
	c.t.Helper()
	doc := capture{StartedAt: time.Now().UTC().Format(time.RFC3339)}
	for _, name := range serverNames {
		s := c.servers[name]
		observed := capturedServer{Name: name, PodUID: fmt.Sprintf("native-%s-%d", name, s.generation), Health: json.RawMessage("null")}
		if status, _, raw := c.health(name); status != 0 {
			observed.HealthStatus = &status
			observed.Health = raw
		}
		doc.Servers = append(doc.Servers, observed)
	}
	doc.CapturedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(doc)
	if err != nil {
		c.t.Fatal(err)
	}
	path := filepath.Join(c.root, "capture.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		c.t.Fatal(err)
	}
	command := exec.Command(c.judge, "--input", path, "--expect-servers", strings.Join(serverNames, ","),
		"--expect-version", release, "--max-age", "5m")
	output, err := command.Output()
	exit := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exit = exitErr.ExitCode()
	} else if err != nil {
		c.t.Fatalf("running the judge: %v", err)
	}
	var report judgeReport
	if err := json.Unmarshal(output, &report); err != nil {
		c.t.Fatalf("the judge printed no report: %v", err)
	}
	if want := map[string]int{"PROVEN": 0, "HOLD": 1, "UNKNOWN": 2}[report.Verdict]; report.Verdict == "" || exit != want {
		c.t.Fatalf("judge verdict %q exited %d", report.Verdict, exit)
	}
	return report
}

// expect captures and requires the verdict and exactly the given finding per
// server; nil findings means none.
func (c *cluster) expect(verdict string, findings map[string]string) {
	c.t.Helper()
	report := c.capture()
	got := map[string]string{}
	for _, f := range report.Findings {
		got[f.Server] = f.Reason
	}
	if report.Verdict != verdict || len(got) != len(findings) || len(report.Findings) != len(findings) {
		c.t.Fatalf("want %s with %v, got %s with %+v", verdict, findings, report.Verdict, report.Findings)
	}
	for name, reason := range findings {
		if got[name] != reason {
			c.t.Fatalf("want %s: %q, got %+v", name, reason, report.Findings)
		}
	}
}

func onlyServer(report judgeReport, name string, reasons ...string) bool {
	if len(report.Findings) != 1 || report.Findings[0].Server != name {
		return false
	}
	for _, reason := range reasons {
		if report.Findings[0].Reason == reason {
			return true
		}
	}
	return false
}

// waitVoters waits until the Raft configuration, read through the active
// server, lists exactly the named servers and every one of them votes.
func (c *cluster) waitVoters(names []string) {
	c.t.Helper()
	c.waitUntil("every server is a Raft voter", func() bool {
		leader, ok := c.activeAmong(names)
		if !ok {
			return false
		}
		status, data := c.request(leader, http.MethodGet, "/v1/sys/storage/raft/configuration", nil, true)
		if status != http.StatusOK {
			return false
		}
		var result struct {
			Data struct {
				Config struct {
					Servers []struct {
						NodeID string `json:"node_id"`
						Voter  bool   `json:"voter"`
					} `json:"servers"`
				} `json:"config"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &result) != nil || len(result.Data.Config.Servers) != len(names) {
			return false
		}
		want := make(map[string]bool, len(names))
		for _, name := range names {
			want[name] = true
		}
		for _, peer := range result.Data.Config.Servers {
			if !want[peer.NodeID] || !peer.Voter {
				return false
			}
			delete(want, peer.NodeID)
		}
		return len(want) == 0
	})
}
