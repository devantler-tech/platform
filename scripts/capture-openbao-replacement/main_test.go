package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const secretToken = "synthetic-openbao-token-only"

// fake stands in for kubectl and for each server behind a port-forward.
type fake struct {
	mu        sync.Mutex
	uids      map[string]string
	uidsAfter map[string]string // UIDs reported once the health reads have begun
	healthy   map[string]int    // health status per pod
	reads     map[string]int
	forwards  []string
	loginCode int
	raftCode  int
	revoked   bool
	revokeErr bool
	jwt       string
	tokenErr  bool
	servers   []*httptest.Server
}

func newFake() *fake {
	return &fake{
		uids:      map[string]string{"openbao-0": "uid-0", "openbao-1": "uid-1", "openbao-2": "uid-2"},
		uidsAfter: map[string]string{},
		healthy:   map[string]int{"openbao-0": 200, "openbao-1": 200, "openbao-2": 200},
		reads:     map[string]int{},
		loginCode: 200,
		raftCode:  200,
		jwt:       "synthetic-service-account-jwt\n",
	}
}

func (f *fake) close() {
	for _, s := range f.servers {
		s.Close()
	}
}

func (f *fake) podUID(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.forwards) > 0 {
		if uid, ok := f.uidsAfter[name]; ok {
			return uid, nil
		}
	}
	uid, ok := f.uids[name]
	if !ok {
		return "", errors.New("not found")
	}
	return uid, nil
}

func (f *fake) createToken(context.Context) (string, error) {
	if f.tokenErr {
		return "", errors.New("forbidden")
	}
	return f.jwt, nil
}

func (f *fake) forward(_ context.Context, target string) (string, func(), error) {
	f.mu.Lock()
	f.forwards = append(f.forwards, target)
	f.mu.Unlock()
	mux := http.NewServeMux()
	if name, ok := strings.CutPrefix(target, "pod/"); ok {
		mux.HandleFunc("/v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.reads[name]++
			code := f.healthy[name]
			f.mu.Unlock()
			if r.URL.Query().Get("standbyok") != "true" || r.Header.Get("X-Vault-Token") != "" {
				code = 400
			}
			w.WriteHeader(code)
			standby := "true"
			if name == "openbao-0" {
				standby = "false"
			}
			_, _ = io.WriteString(w, `{"initialized":true,"sealed":false,"standby":`+standby+`,"server_time_utc":`+strconv.FormatInt(time.Now().Unix(), 10)+`,"version":"2.6.3","cluster_id":"c","node":"`+name+`"}`)
		})
	} else if target == activeService {
		mux.HandleFunc("/v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || body["role"] != observerAccount || body["jwt"] != "synthetic-service-account-jwt" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(f.loginCode)
			_, _ = io.WriteString(w, `{"auth":{"client_token":"`+secretToken+`"}}`)
		})
		mux.HandleFunc("/v1/sys/storage/raft/configuration", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.Header.Get("X-Vault-Token") != secretToken {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"errors":["permission denied"]}`)
				return
			}
			if f.raftCode < 0 {
				w.WriteHeader(502)
				_, _ = io.WriteString(w, "<html>bad gateway</html>")
				return
			}
			w.WriteHeader(f.raftCode)
			_, _ = io.WriteString(w, `{"data":{"config":{"index":0,"servers":[{"node_id":"openbao-0","address":"10.0.0.1:8201","leader":true,"voter":true,"protocol_version":"3"},{"node_id":"openbao-1","address":"10.0.0.2:8201","leader":false,"voter":true,"protocol_version":"3"},{"node_id":"openbao-2","address":"10.0.0.3:8201","leader":false,"voter":true,"protocol_version":"3"}]}}}`)
		})
		mux.HandleFunc("/v1/auth/token/revoke-self", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.Header.Get("X-Vault-Token") != secretToken || f.revokeErr {
				w.WriteHeader(403)
				return
			}
			f.mu.Lock()
			f.revoked = true
			f.mu.Unlock()
			w.WriteHeader(204)
		})
	} else {
		return "", nil, errors.New("unexpected target")
	}
	s := httptest.NewServer(mux)
	f.servers = append(f.servers, s)
	return s.URL, func() {}, nil
}

func (f *fake) client() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func clock() func() time.Time {
	t := time.Date(2026, 11, 3, 16, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func capture(t *testing.T, f *fake, dir string, extra ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"--expect-servers", "openbao-0,openbao-1,openbao-2", "--out", dir}, extra...)
	code := run(context.Background(), args, &stdout, &stderr, f, clock())
	for _, out := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(out, secretToken) || strings.Contains(out, "synthetic-service-account-jwt") || strings.Contains(out, "10.0.0.1") {
			t.Fatalf("a credential or address reached the log: %q", out)
		}
	}
	return code, stdout.String(), stderr.String()
}

func TestCapturesBothObservationsForTheJudges(t *testing.T) {
	f := newFake()
	defer f.close()
	dir := privateDir(t)
	code, out, _ := capture(t, f, dir)
	if code != 0 || out != "OPENBAO_REPLACEMENT_EVIDENCE=CAPTURED\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if !f.revoked {
		t.Fatal("the observer token was not revoked")
	}
	var health healthCapture
	b, _ := os.ReadFile(filepath.Join(dir, "health.json"))
	if json.Unmarshal(b, &health) != nil || len(health.Servers) != 3 || health.StartedAt >= health.CapturedAt {
		t.Fatalf("health capture: %s", b)
	}
	for i, s := range health.Servers {
		var body map[string]any
		if json.Unmarshal(s.Health, &body) != nil || body["node"] != s.Name || s.PodUID != f.uids[s.Name] || s.HealthStatus != 200 || s.Name != []string{"openbao-0", "openbao-1", "openbao-2"}[i] {
			t.Fatalf("observation %d not bound to its own server: %+v", i, s)
		}
	}
	var quorum quorumCapture
	b, _ = os.ReadFile(filepath.Join(dir, "quorum.json"))
	if json.Unmarshal(b, &quorum) != nil || quorum.Status != 200 || !bytes.Contains(quorum.Configuration, []byte(`"node_id":"openbao-0"`)) || quorum.StartedAt >= quorum.CapturedAt {
		t.Fatalf("quorum capture: %s", b)
	}
	for _, name := range []string{"health.json", "quorum.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is not private", name)
		}
	}
	want := []string{"pod/openbao-0", "pod/openbao-1", "pod/openbao-2", activeService}
	if strings.Join(f.forwards, " ") != strings.Join(want, " ") {
		t.Fatalf("forwards %v", f.forwards)
	}
}

func TestRecordsANonSuccessHealthStatusForTheJudge(t *testing.T) {
	f := newFake()
	defer f.close()
	f.healthy["openbao-1"] = 503
	dir := privateDir(t)
	if code, _, _ := capture(t, f, dir); code != 0 {
		t.Fatal("a sealed server is the judge's finding, not a failed capture")
	}
	var health healthCapture
	b, _ := os.ReadFile(filepath.Join(dir, "health.json"))
	if json.Unmarshal(b, &health) != nil || health.Servers[1].HealthStatus != 503 {
		t.Fatalf("status not preserved: %s", b)
	}
}

func TestFailedReadsAreUnverifiedAndWriteNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*fake)
		stage stage
	}{
		{"missing pod", func(f *fake) { delete(f.uids, "openbao-2") }, stagePods},
		{"pod replaced mid-capture", func(f *fake) { f.uidsAfter["openbao-1"] = "uid-1-new" }, stagePods},
		{"token mint refused", func(f *fake) { f.tokenErr = true }, stageIdentity},
		{"empty token", func(f *fake) { f.jwt = "\n" }, stageIdentity},
		{"login refused", func(f *fake) { f.loginCode = 403 }, stageLogin},
		{"raft read unreadable", func(f *fake) { f.raftCode = -1 }, stageRaft},
		{"revoke refused", func(f *fake) { f.revokeErr = true }, stageRevoke},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			defer f.close()
			tc.set(f)
			dir := privateDir(t)
			code, out, _ := capture(t, f, dir)
			if code != 2 || out != "OPENBAO_REPLACEMENT_EVIDENCE=UNVERIFIED stage="+string(tc.stage)+"\n" {
				t.Fatalf("code=%d out=%q", code, out)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatal("a failed capture left evidence for the judges")
			}
		})
	}
}

func TestRaftReadStatusIsPassedThroughNotJudged(t *testing.T) {
	// A non-200 raft read with a valid body still reaches the judge, which owns
	// the UNKNOWN verdict for it; only an unreadable body fails the capture.
	f := newFake()
	defer f.close()
	f.raftCode = 429
	dir := privateDir(t)
	if code, _, _ := capture(t, f, dir); code != 0 {
		t.Fatal("capture refused a readable raft response")
	}
	var quorum quorumCapture
	b, _ := os.ReadFile(filepath.Join(dir, "quorum.json"))
	if json.Unmarshal(b, &quorum) != nil || quorum.Status != 429 {
		t.Fatalf("status not preserved: %s", b)
	}
}

func TestRefusesInvalidArguments(t *testing.T) {
	f := newFake()
	defer f.close()
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{},
		{"--expect-servers", "openbao-0,openbao-1", "--out", privateDir(t)},
		{"--expect-servers", "openbao-0,openbao-0,openbao-1", "--out", privateDir(t)},
		{"--expect-servers", "openbao-0,openbao-1,other", "--out", privateDir(t)},
		{"--expect-servers", "openbao-0,openbao-1,openbao-2", "--out", open},
		{"--expect-servers", "openbao-0,openbao-1,openbao-2", "--out", filepath.Join(t.TempDir(), "absent")},
		{"--expect-servers", "openbao-0,openbao-1,openbao-2", "--out", privateDir(t), "extra"},
		{"--expect-servers", "openbao-0,openbao-1,openbao-2", "--out", privateDir(t), "--unknown"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr, f, clock()); code != 2 || stdout.Len() != 0 {
			t.Fatalf("%v: code=%d out=%q", args, code, stdout.String())
		}
	}
	if len(f.forwards) != 0 {
		t.Fatal("invalid arguments reached the cluster")
	}
}

func TestForwardingLineIsParsedStrictly(t *testing.T) {
	for line, ok := range map[string]bool{
		"Forwarding from 127.0.0.1:41234 -> 8200":  true,
		"Forwarding from [::1]:41234 -> 8200":      false,
		"Forwarding from 0.0.0.0:41234 -> 8200":    false,
		"Forwarding from 127.0.0.1:41234 -> 8201":  false,
		" Forwarding from 127.0.0.1:41234 -> 8200": false,
	} {
		if (forwarding.FindStringSubmatch(line) != nil) != ok {
			t.Fatalf("%q", line)
		}
	}
}

// The judges decide; this proves they can read what the collector writes, so
// an envelope drift fails here rather than as UNKNOWN in production.
func TestJudgesAcceptTheCaptures(t *testing.T) {
	f := newFake()
	defer f.close()
	dir := privateDir(t)
	var stdout, stderr bytes.Buffer
	args := []string{"--expect-servers", "openbao-0,openbao-1,openbao-2", "--out", dir}
	if code := run(context.Background(), args, &stdout, &stderr, f, time.Now); code != 0 {
		t.Fatalf("capture: %q", stdout.String())
	}
	for _, judge := range [][]string{
		{"../judge-openbao-health", "--input", filepath.Join(dir, "health.json"), "--expect-servers", "openbao-0,openbao-1,openbao-2", "--expect-version", "2.6.3", "--max-age", "5m"},
		{"../judge-openbao-quorum", "--input", filepath.Join(dir, "quorum.json"), "--expect-servers", "openbao-0,openbao-1,openbao-2", "--replace", "openbao-2", "--max-age", "5m"},
	} {
		out, err := exec.Command("go", append([]string{"run"}, judge...)...).CombinedOutput()
		if err != nil || !bytes.Contains(out, []byte("PROVEN")) {
			t.Fatalf("%s: %v %s", judge[0], err, out)
		}
	}
}
