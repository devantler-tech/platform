package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	now = time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	// served is inside the window fresh() captures in.
	served = time.Date(2026, 10, 16, 19, 57, 55, 0, time.UTC)
	// backslash builds escaped JSON keys without the source text being
	// unescaped by any tool that writes this file.
	backslash = string(rune(92))
)

func want() expectation {
	return expectation{servers: []string{"openbao-0", "openbao-1", "openbao-2"}, version: "2.6.3", maxAge: 5 * time.Minute}
}

// body mirrors OpenBao's sys/health response (api/v2 HealthResponse): the
// cluster name and identity are omitted while a server is sealed or
// uninitialized.
func body(initialized, sealed, standby bool, version, cluster string) string {
	return bodyAt(served, initialized, sealed, standby, version, cluster)
}

func bodyAt(at time.Time, initialized, sealed, standby bool, version, cluster string) string {
	clusterFields := fmt.Sprintf(`,"cluster_name":"synthetic","cluster_id":%q`, cluster)
	if !initialized || sealed {
		clusterFields = ""
	}
	return fmt.Sprintf(`{"initialized":%t,"sealed":%t,"standby":%t,"performance_standby":false,"replication_performance_mode":"disabled","replication_dr_mode":"disabled","server_time_utc":%d,"version":%q%s}`,
		initialized, sealed, standby, at.Unix(), version, clusterFields)
}

func server(name, uid string, status int, health string) string {
	return fmt.Sprintf(`{"name":%q,"podUID":%q,"healthStatus":%d,"health":%s}`, name, uid, status, health)
}

// document wraps servers in a capture that started ten seconds before it completed.
func document(capturedAt string, servers ...string) string {
	startedAt := capturedAt
	if completed, err := time.Parse(time.RFC3339, capturedAt); err == nil {
		startedAt = completed.Add(-10 * time.Second).Format(time.RFC3339)
	}
	return documentBetween(startedAt, capturedAt, servers...)
}

func documentBetween(startedAt, capturedAt string, servers ...string) string {
	return fmt.Sprintf(`{"startedAt":%q,"capturedAt":%q,"servers":[%s]}`, startedAt, capturedAt, strings.Join(servers, ","))
}

func healthyServers() []string { return healthyServersAt(served) }

func healthyServersAt(at time.Time) []string {
	return []string{
		server("openbao-0", "uid-0", 200, bodyAt(at, true, false, false, "2.6.3", "cluster-a")),
		server("openbao-1", "uid-1", 200, bodyAt(at, true, false, true, "2.6.3", "cluster-a")),
		server("openbao-2", "uid-2", 200, bodyAt(at, true, false, true, "2.6.3", "cluster-a")),
	}
}

func fresh(servers ...string) string { return document("2026-10-16T19:58:00Z", servers...) }

func replace(servers []string, index int, value string) []string {
	out := append([]string(nil), servers...)
	out[index] = value
	return out
}

func TestHealthyClusterIsProven(t *testing.T) {
	got := judge(strings.NewReader(fresh(healthyServers()...)), want(), now)
	if got.Verdict != verdictProven || len(got.Findings) != 0 || got.Servers != 3 {
		t.Fatalf("healthy cluster: %+v", got)
	}
	if got.Scope != scope || !strings.Contains(got.Scope, "not Raft membership or quorum proof") {
		t.Fatal("the verdict must state that health is not quorum proof")
	}
}

func TestDefiniteFindingsHold(t *testing.T) {
	healthy := healthyServers()
	for _, test := range []struct {
		name    string
		servers []string
		reason  string
	}{
		{"sealed despite HTTP success", replace(healthy, 1, server("openbao-1", "uid-1", 200, body(true, true, true, "2.6.3", "cluster-a"))), "server is sealed"},
		{"uninitialized despite HTTP success", replace(healthy, 2, server("openbao-2", "uid-2", 200, body(false, false, true, "2.6.3", "cluster-a"))), "server is not initialized"},
		{"sealed with the native sealed status", replace(healthy, 1, server("openbao-1", "uid-1", 503, body(true, true, true, "2.6.3", "cluster-a"))), "server is sealed"},
		{"stale release", replace(healthy, 0, server("openbao-0", "uid-0", 200, body(true, false, false, "2.6.2", "cluster-a"))), "server is not on the expected release"},
		{"split cluster", replace(healthy, 2, server("openbao-2", "uid-2", 200, body(true, false, true, "2.6.3", "cluster-b"))), "servers report different cluster identities"},
		{"no active server", replace(healthy, 0, server("openbao-0", "uid-0", 200, body(true, false, true, "2.6.3", "cluster-a"))), "no server is active"},
		{"two active servers", replace(healthy, 1, server("openbao-1", "uid-1", 200, body(true, false, false, "2.6.3", "cluster-a"))), "more than one server is active"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := judge(strings.NewReader(fresh(test.servers...)), want(), now)
			if got.Verdict != verdictHold || !hasReason(got, test.reason) {
				t.Fatalf("want HOLD %q, got %+v", test.reason, got)
			}
		})
	}
}

func TestIncompleteOrAmbiguousEvidenceIsUnknown(t *testing.T) {
	healthy := healthyServers()
	h := body(true, false, true, "2.6.3", "cluster-a")
	for _, test := range []struct {
		name     string
		document string
		reason   string
	}{
		{"missing server", fresh(healthy[0], healthy[1]), "server was not observed"},
		{"duplicate server", fresh(append(healthy, healthy[2])...), "server observed more than once"},
		{"unexpected server", fresh(append(healthy, server("openbao-3", "uid-3", 200, h))...), "capture contains an unexpected server"},
		{"unnamed server", fresh(append(healthy, `{"podUID":"uid-9","healthStatus":200,"health":`+h+`}`)...), "a server observation has no name"},
		{"no pod identity", fresh(replace(healthy, 1, `{"name":"openbao-1","healthStatus":200,"health":`+h+`}`)...), "observation is not bound to a pod identity"},
		{"shared pod identity", fresh(replace(healthy, 1, server("openbao-1", "uid-0", 200, h))...), "pod identity is shared with another server"},
		{"HTTP success without a health body", fresh(replace(healthy, 1, `{"name":"openbao-1","podUID":"uid-1","healthStatus":200}`)...), "no health evidence"},
		{"null health body", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, "null"))...), "no health evidence"},
		{"partial health body", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, `{"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"cluster-a"}`))...), "health response is incomplete"},
		{"health body without seal state", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, `{"initialized":true,"standby":true,"version":"2.6.3","cluster_id":"cluster-a"}`))...), "health response is incomplete"},
		{"unsealed body without cluster identity", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, fmt.Sprintf(`{"initialized":true,"sealed":false,"standby":true,"server_time_utc":%d,"version":"2.6.3"}`, served.Unix())))...), "health response is incomplete"},
		{"health body without server time", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, `{"initialized":true,"sealed":false,"standby":true,"version":"2.6.3","cluster_id":"cluster-a"}`))...), "health response is incomplete"},
		{"health body older than the capture", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, bodyAt(served.Add(-3*time.Minute), true, false, true, "2.6.3", "cluster-a")))...), "health response was not produced during the capture"},
		{"health body newer than the capture", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, bodyAt(served.Add(3*time.Minute), true, false, true, "2.6.3", "cluster-a")))...), "health response was not produced during the capture"},
		{"old sealed body is not a current finding", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 503, bodyAt(served.Add(-3*time.Minute), true, true, true, "2.6.3", "")))...), "health response was not produced during the capture"},
		{"missing status outranks a wrong release", fresh(replace(healthy, 1, `{"name":"openbao-1","podUID":"uid-1","health":`+body(true, false, true, "2.6.2", "")+`}`)...), "health response has no HTTP status"},
		{"missing status outranks a sealed body", fresh(replace(healthy, 1, `{"name":"openbao-1","podUID":"uid-1","health":`+body(true, true, true, "2.6.3", "")+`}`)...), "health response has no HTTP status"},
		{"empty cluster identity outranks a wrong release", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, body(true, false, true, "2.6.2", "")))...), "health response names no cluster"},
		{"failing status outranks a wrong release", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 429, body(true, false, true, "2.6.2", "cluster-a")))...), "health HTTP status disagrees with its body"},
		{"health body of the wrong type", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, `{"initialized":"true","sealed":false,"standby":true,"version":"2.6.3","cluster_id":"cluster-a"}`))...), "health response is not an object of the expected shape"},
		{"healthy body with a failing status", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 429, h))...), "health HTTP status disagrees with its body"},
		{"no HTTP status", fresh(replace(healthy, 1, `{"name":"openbao-1","podUID":"uid-1","health":`+h+`}`)...), "health response has no HTTP status"},
		{"empty cluster identity", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, body(true, false, true, "2.6.3", "")))...), "health response names no cluster"},
		{"stale capture", document("2026-10-16T19:55:05Z", healthy...), "capture is stale; recapture"},
		{"fresh completion but stale start", documentBetween("2026-10-16T19:54:59Z", "2026-10-16T19:59:59Z", healthy...), "capture is stale; recapture"},
		{"future capture", document("2026-10-16T20:00:01Z", healthy...), "capture is from the future"},
		{"completed before it started", documentBetween("2026-10-16T19:58:00Z", "2026-10-16T19:57:59Z", healthy...), "capture completed before it started"},
		{"capture without completion time", `{"startedAt":"2026-10-16T19:58:00Z","servers":[` + strings.Join(healthy, ",") + `]}`, "capture has no start or completion time"},
		{"capture without start time", `{"capturedAt":"2026-10-16T19:58:00Z","servers":[` + strings.Join(healthy, ",") + `]}`, "capture has no start or completion time"},
		{"unparseable completion time", documentBetween("2026-10-16T19:58:00Z", "yesterday", healthy...), "capture times are not RFC 3339"},
		{"no servers at all", fresh(), "server was not observed"},
		{"unknown envelope field", `{"startedAt":"2026-10-16T19:57:50Z","capturedAt":"2026-10-16T19:58:00Z","kubernetesReady":true,"servers":[` + strings.Join(healthy, ",") + `]}`, "capture does not have the expected shape"},
		{"trailing document", fresh(healthy...) + ` {}`, "capture is ambiguous or malformed: trailing evidence"},
		{"truncated document", strings.TrimSuffix(fresh(healthy...), "]}"), "capture is ambiguous or malformed: incomplete evidence"},
		{"duplicate sealed member", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, `{"initialized":true,"sealed":true,"sealed":false,"standby":true,"version":"2.6.3","cluster_id":"cluster-a"}`))...), "capture is ambiguous or malformed: ambiguous evidence fields"},
		{"escaped duplicate sealed member", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, `{"initialized":true,"sealed":true,"`+backslash+`u0073ealed":false,"standby":true,"version":"2.6.3","cluster_id":"cluster-a"}`))...), "capture is ambiguous or malformed: ambiguous evidence fields"},
		{"escaped case alias of sealed", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, `{"initialized":true,"sealed":true,"`+backslash+`u0053ealed":false,"standby":true,"version":"2.6.3","cluster_id":"cluster-a"}`))...), "capture is ambiguous or malformed: case-aliased evidence field"},
		{"case-aliased sealed member", fresh(replace(healthy, 1, server("openbao-1", "uid-1", 200, `{"initialized":true,"Sealed":false,"standby":true,"version":"2.6.3","cluster_id":"cluster-a"}`))...), "capture is ambiguous or malformed: case-aliased evidence field"},
		{"duplicate server name member", fresh(replace(healthy, 1, `{"name":"openbao-9","name":"openbao-1","podUID":"uid-1","healthStatus":200,"health":`+h+`}`)...), "capture is ambiguous or malformed: ambiguous evidence fields"},
		{"oversized capture", `{"startedAt":"2026-10-16T19:57:50Z","capturedAt":"2026-10-16T19:58:00Z","servers":[],"pad":"` + strings.Repeat("x", maxCaptureBytes) + `"}`, "capture exceeds its size bound"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := judge(strings.NewReader(test.document), want(), now)
			if got.Verdict != verdictUnknown || !hasReason(got, test.reason) {
				t.Fatalf("want UNKNOWN %q, got %+v", test.reason, got)
			}
		})
	}
}

func TestUnknownOutranksHold(t *testing.T) {
	healthy := healthyServers()
	sealed := server("openbao-1", "uid-1", 200, body(true, true, true, "2.6.3", "cluster-a"))
	got := judge(strings.NewReader(fresh(healthy[0], sealed)), want(), now)
	if got.Verdict != verdictUnknown || !hasReason(got, "server is sealed") || !hasReason(got, "server was not observed") {
		t.Fatalf("a missing server must keep the verdict UNKNOWN while still naming the sealed one: %+v", got)
	}
}

func TestUnrelatedNativeHealthFieldsKeepTheirSpelling(t *testing.T) {
	healthy := healthyServers()
	native := fmt.Sprintf(`{"initialized":true,"sealed":false,"standby":true,"version":"2.6.3","cluster_id":"cluster-a","server_time_utc":%d,"replication_dr_mode":"disabled","Extra":{"Label":"one","label":"two"}}`, served.Unix())
	got := judge(strings.NewReader(fresh(replace(healthy, 2, server("openbao-2", "uid-2", 200, native))...)), want(), now)
	if got.Verdict != verdictProven {
		t.Fatalf("fields the judge does not read must not change the verdict: %+v", got)
	}
}

func TestExpectationsComeOnlyFromReviewedArguments(t *testing.T) {
	for _, test := range []struct {
		servers, version string
		maxAge           time.Duration
	}{
		{"", "2.6.3", time.Minute},
		{"openbao-0,,openbao-2", "2.6.3", time.Minute},
		{"openbao-0,openbao-0", "2.6.3", time.Minute},
		{"openbao-0", "", time.Minute},
		{"openbao-0", "2.6.3", 0},
		{"openbao-0", "2.6.3", 2 * time.Hour},
	} {
		if _, err := parseExpectation(test.servers, test.version, test.maxAge); err == nil {
			t.Errorf("accepted invalid expectation %+v", test)
		}
	}
	got, err := parseExpectation("openbao-0,openbao-1,openbao-2", "2.6.3", 5*time.Minute)
	if err != nil || len(got.servers) != 3 {
		t.Fatalf("valid expectation refused: %v", err)
	}
}

// TestCommandExitCodesAndPrivateOutput builds the command and runs it the way
// a procedure would, so the exit codes and the printed line are what is tested.
func TestCommandExitCodesAndPrivateOutput(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "judge-openbao-health")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	readAt := time.Now().UTC().Add(-time.Minute)
	recent := readAt.Format(time.RFC3339)
	healthy := healthyServersAt(readAt)
	for i := range healthy {
		healthy[i] = strings.ReplaceAll(healthy[i], "cluster-a", "private-cluster-canary")
	}
	sealed := replace(healthy, 1, server("openbao-1", "uid-1", 503, bodyAt(readAt, true, true, true, "2.6.3", "")))
	all := []string{"--expect-servers", "openbao-0,openbao-1,openbao-2", "--expect-version", "2.6.3"}
	for _, test := range []struct {
		name     string
		document string
		args     []string
		code     int
		verdict  string
	}{
		{"proven", document(recent, healthy...), all, 0, verdictProven},
		{"hold", document(recent, sealed...), all, 1, verdictHold},
		{"unknown", document(recent, healthy[0]), all, 2, verdictUnknown},
		{"empty release", document(recent, healthy...), []string{"--expect-servers", "openbao-0,openbao-1,openbao-2", "--expect-version", ""}, 2, ""},
		{"short help flag", document(recent, healthy...), append(append([]string(nil), all...), "-h"), 2, ""},
		{"long help flag", document(recent, healthy...), append(append([]string(nil), all...), "--help"), 2, ""},
		{"unknown flag", document(recent, healthy...), append(append([]string(nil), all...), "--assume-healthy"), 2, ""},
		{"repeated flag cannot narrow the servers", document(recent, healthy[0]), append(append([]string(nil), all...), "--expect-servers", "openbao-0"), 2, ""},
		{"repeated flag cannot swap the release", document(recent, healthy...), append(append([]string(nil), all...), "--expect-version=2.6.2"), 2, ""},
		{"positional argument", document(recent, healthy...), append(append([]string(nil), all...), "extra"), 2, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture.json")
			if err := os.WriteFile(path, []byte(test.document), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"--input", path}, test.args...)
			cmd := exec.Command(binary, args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != test.code {
				t.Fatalf("exit %d, want %d; stderr: %s", code, test.code, stderr.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), "private-cluster-canary") || strings.Contains(stdout.String(), "uid-") {
				t.Fatal("output must not echo captured identities or health bodies")
			}
			if test.verdict == "" {
				if stdout.Len() != 0 {
					t.Fatal("invalid arguments must not print a verdict")
				}
				return
			}
			var printed report
			if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil || printed.Verdict != test.verdict {
				t.Fatalf("printed %q, want verdict %s", stdout.String(), test.verdict)
			}
		})
	}
}

func hasReason(got report, reason string) bool {
	for _, f := range got.Findings {
		if f.Reason == reason {
			return true
		}
	}
	return false
}
