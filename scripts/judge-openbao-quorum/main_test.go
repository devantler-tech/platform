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

var now = time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)

func want(replace string) expectation {
	return expectation{servers: []string{"openbao-0", "openbao-1", "openbao-2"}, replace: replace, maxAge: 5 * time.Minute}
}

// member mirrors one entry of OpenBao's sys/storage/raft/configuration
// response, including the native fields this command ignores.
func raftMember(id string, leader, voter bool) string {
	return fmt.Sprintf(`{"node_id":%q,"address":"%s.synthetic:8201","leader":%t,"protocol_version":"3","voter":%t}`, id, id, leader, voter)
}

func configBody(members ...string) string {
	return fmt.Sprintf(`{"request_id":"synthetic","lease_id":"","renewable":false,"lease_duration":0,"data":{"config":{"index":0,"servers":[%s]}},"wrap_info":null,"warnings":null,"auth":null}`, strings.Join(members, ","))
}

func documentAt(startedAt, capturedAt string, status int, body string) string {
	return fmt.Sprintf(`{"startedAt":%q,"capturedAt":%q,"status":%d,"configuration":%s}`, startedAt, capturedAt, status, body)
}

func fresh(body string) string {
	return documentAt("2026-10-17T19:58:00Z", "2026-10-17T19:58:01Z", 200, body)
}

func healthyMembers() []string {
	return []string{raftMember("openbao-0", true, true), raftMember("openbao-1", false, true), raftMember("openbao-2", false, true)}
}

func replaceAt(members []string, index int, value string) []string {
	out := append([]string(nil), members...)
	out[index] = value
	return out
}

func reasons(r report) string {
	parts := []string{}
	for _, f := range r.Findings {
		parts = append(parts, f.Server+":"+f.Reason)
	}
	return strings.Join(parts, "; ")
}

func TestVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		document string
		replace  string
		verdict  string
		reason   string
	}{
		{"healthy cluster, replace a follower", fresh(configBody(healthyMembers()...)), "openbao-2", verdictProven, ""},
		{"healthy cluster, replace the leader", fresh(configBody(healthyMembers()...)), "openbao-0", verdictProven, ""},
		{"members in any order", fresh(configBody(raftMember("openbao-2", false, true), raftMember("openbao-0", true, true), raftMember("openbao-1", false, true))), "openbao-1", verdictProven, ""},

		{"expected server missing", fresh(configBody(healthyMembers()[:2]...)), "openbao-0", verdictHold, "openbao-2:server is not a Raft member"},
		{"expected server is a non-voter", fresh(configBody(replaceAt(healthyMembers(), 2, raftMember("openbao-2", false, false))...)), "openbao-1", verdictHold, "openbao-2:server is not a voter"},
		{"unexpected member", fresh(configBody(append(healthyMembers(), raftMember("intruder", false, false))...)), "openbao-1", verdictHold, "configuration has 1 unexpected member(s)"},
		{"no leader", fresh(configBody(replaceAt(healthyMembers(), 0, raftMember("openbao-0", false, true))...)), "openbao-1", verdictHold, "no member is leader"},
		{"two leaders", fresh(configBody(replaceAt(healthyMembers(), 1, raftMember("openbao-1", true, true))...)), "openbao-2", verdictHold, "more than one member is leader"},
		{"quorum lost when a voter is already gone", fresh(configBody(raftMember("openbao-0", true, true), raftMember("openbao-1", false, true))), "openbao-1", verdictHold, "openbao-1:replacing it leaves 1 of 2 voters, below quorum 2"},
		{"quorum lost with two of three non-voters", fresh(configBody(raftMember("openbao-0", true, true), raftMember("openbao-1", false, false), raftMember("openbao-2", false, true))), "openbao-2", verdictHold, "openbao-2:replacing it leaves 1 of 2 voters, below quorum 2"},
		{"unexpected voter cannot be counted towards a safe replacement", fresh(configBody(raftMember("openbao-0", true, true), raftMember("intruder", false, true))), "openbao-0", verdictHold, "configuration has 1 unexpected member(s)"},

		{"stale capture", documentAt("2026-10-17T19:50:00Z", "2026-10-17T19:50:01Z", 200, configBody(healthyMembers()...)), "openbao-1", verdictUnknown, "capture is stale; recapture"},
		{"capture from the future", documentAt("2026-10-17T19:59:59Z", "2026-10-17T20:00:01Z", 200, configBody(healthyMembers()...)), "openbao-1", verdictUnknown, "capture is from the future"},
		{"completed before it started", documentAt("2026-10-17T19:58:02Z", "2026-10-17T19:58:01Z", 200, configBody(healthyMembers()...)), "openbao-1", verdictUnknown, "capture completed before it started"},
		{"times not RFC 3339", documentAt("yesterday", "2026-10-17T19:58:01Z", 200, configBody(healthyMembers()...)), "openbao-1", verdictUnknown, "capture times are not RFC 3339"},
		{"no times", `{"status":200,"configuration":` + configBody(healthyMembers()...) + `}`, "openbao-1", verdictUnknown, "capture has no start or completion time"},
		{"no status", `{"startedAt":"2026-10-17T19:58:00Z","capturedAt":"2026-10-17T19:58:01Z","configuration":` + configBody(healthyMembers()...) + `}`, "openbao-1", verdictUnknown, "configuration response has no HTTP status"},
		{"forbidden read", documentAt("2026-10-17T19:58:00Z", "2026-10-17T19:58:01Z", 403, `{"errors":["permission denied"]}`), "openbao-1", verdictUnknown, "configuration read did not succeed"},
		{"null configuration", fresh("null"), "openbao-1", verdictUnknown, "no configuration evidence"},
		{"configuration without data", fresh(`{"request_id":"synthetic"}`), "openbao-1", verdictUnknown, "configuration response is incomplete"},
		{"configuration without servers", fresh(`{"data":{"config":{"index":0}}}`), "openbao-1", verdictUnknown, "configuration response is incomplete"},
		{"servers is not a list", fresh(`{"data":{"config":{"servers":"openbao-0"}}}`), "openbao-1", verdictUnknown, "configuration response is not an object of the expected shape"},
		{"member without voter field", fresh(configBody(replaceAt(healthyMembers(), 1, `{"node_id":"openbao-1","leader":false}`)...)), "openbao-2", verdictUnknown, "openbao-1:member entry is incomplete"},
		{"member without node identity", fresh(configBody(append(healthyMembers(), `{"leader":false,"voter":true}`)...)), "openbao-2", verdictUnknown, "a member has no node identity"},
		{"member listed twice", fresh(configBody(append(healthyMembers(), raftMember("openbao-1", false, true))...)), "openbao-2", verdictUnknown, "openbao-1:member listed more than once"},
		{"unknown outranks hold", fresh(configBody(raftMember("openbao-0", true, true), raftMember("openbao-1", false, false), `{"node_id":"openbao-2","leader":false}`)), "openbao-1", verdictUnknown, "openbao-2:member entry is incomplete"},
		{"duplicate envelope member", `{"startedAt":"2026-10-17T19:58:00Z","startedAt":"2026-10-17T19:58:00Z","capturedAt":"2026-10-17T19:58:01Z","status":200,"configuration":` + configBody(healthyMembers()...) + `}`, "openbao-1", verdictUnknown, "capture is ambiguous or malformed: ambiguous evidence fields"},
		{"case-aliased voter field", fresh(configBody(replaceAt(healthyMembers(), 2, `{"node_id":"openbao-2","leader":false,"voter":false,"Voter":true}`)...)), "openbao-1", verdictUnknown, "capture is ambiguous or malformed: case-aliased evidence field"},
		{"unexpected envelope field", `{"startedAt":"2026-10-17T19:58:00Z","capturedAt":"2026-10-17T19:58:01Z","status":200,"verdict":"PROVEN","configuration":` + configBody(healthyMembers()...) + `}`, "openbao-1", verdictUnknown, "capture does not have the expected shape"},
		{"trailing document", fresh(configBody(healthyMembers()...)) + `{}`, "openbao-1", verdictUnknown, "capture is ambiguous or malformed"},
		{"truncated", fresh(configBody(healthyMembers()...))[:80], "openbao-1", verdictUnknown, "capture is ambiguous or malformed: incomplete evidence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judge(strings.NewReader(tc.document), want(tc.replace), now)
			if got.Verdict != tc.verdict {
				t.Fatalf("verdict = %s, want %s (%s)", got.Verdict, tc.verdict, reasons(got))
			}
			if tc.reason != "" && !strings.Contains(reasons(got), tc.reason) {
				t.Fatalf("findings %q do not contain %q", reasons(got), tc.reason)
			}
			if tc.verdict == verdictProven && len(got.Findings) != 0 {
				t.Fatalf("a proven verdict carries findings: %s", reasons(got))
			}
		})
	}
}

func TestOversizedCaptureIsUnknown(t *testing.T) {
	padding := strings.Repeat(" ", maxCaptureBytes)
	got := judge(strings.NewReader(fresh(configBody(healthyMembers()...))+padding), want("openbao-1"), now)
	if got.Verdict != verdictUnknown || !strings.Contains(reasons(got), "size bound") {
		t.Fatalf("oversized capture: %s %s", got.Verdict, reasons(got))
	}
}

// TestReportNeverEchoesCapturedData proves that addresses and unexpected node
// identities from the capture never reach the printed report.
func TestReportNeverEchoesCapturedData(t *testing.T) {
	document := fresh(configBody(append(healthyMembers(), raftMember("secret-node-name", false, true))...))
	got := judge(strings.NewReader(document), want("openbao-1"), now)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret-node-name", "synthetic:8201", "synthetic"} {
		if bytes.Contains(encoded, []byte(leak)) {
			t.Fatalf("report contains captured value %q: %s", leak, encoded)
		}
	}
}

func TestParseExpectation(t *testing.T) {
	cases := []struct {
		servers, replace string
		maxAge           time.Duration
		ok               bool
	}{
		{"openbao-0,openbao-1,openbao-2", "openbao-1", 5 * time.Minute, true},
		{"openbao-0,openbao-1,openbao-2", "", 5 * time.Minute, false},
		{"openbao-0,openbao-1,openbao-2", "openbao-9", 5 * time.Minute, false},
		{"openbao-0,openbao-0", "openbao-0", 5 * time.Minute, false},
		{"openbao-0,", "openbao-0", 5 * time.Minute, false},
		{"", "openbao-0", 5 * time.Minute, false},
		{"openbao-0", "openbao-0", 0, false},
		{"openbao-0", "openbao-0", 2 * time.Hour, false},
	}
	for _, tc := range cases {
		_, err := parseExpectation(tc.servers, tc.replace, tc.maxAge)
		if (err == nil) != tc.ok {
			t.Errorf("parseExpectation(%q, %q, %s) error = %v, want ok=%t", tc.servers, tc.replace, tc.maxAge, err, tc.ok)
		}
	}
}

// TestCommandExitCodes builds the command and checks the exit status the
// calling procedure branches on, including refused arguments.
func TestCommandExitCodes(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "judge-openbao-quorum")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	write := func(document string) string {
		path := filepath.Join(t.TempDir(), "capture.json")
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stamp := func(offset time.Duration) string { return time.Now().UTC().Add(offset).Format(time.RFC3339) }
	current := documentAt(stamp(-2*time.Second), stamp(-time.Second), 200, configBody(healthyMembers()...))
	missing := documentAt(stamp(-2*time.Second), stamp(-time.Second), 200, configBody(healthyMembers()[:2]...))
	servers := "--expect-servers=openbao-0,openbao-1,openbao-2"
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"proven", []string{"--input", write(current), servers, "--replace", "openbao-1"}, 0},
		{"hold", []string{"--input", write(missing), servers, "--replace", "openbao-1"}, 1},
		{"stale", []string{"--input", write(documentAt(stamp(-time.Hour), stamp(-time.Hour+time.Second), 200, configBody(healthyMembers()...))), servers, "--replace", "openbao-1"}, 2},
		{"help", []string{"-h"}, 2},
		{"unknown flag", []string{"--input", write(current), servers, "--replace", "openbao-1", "--force"}, 2},
		{"flag twice", []string{"--input", write(current), servers, "--replace", "openbao-1", "--replace", "openbao-2"}, 2},
		{"flag twice after a -- value", []string{"--expect-servers", "--", "--replace", "--", "--expect-servers", "openbao-1", "--replace", "openbao-1", "--input", write(current)}, 2},
		{"flag twice in = form", []string{"--input", write(current), servers, "--replace=openbao-1", "-replace=openbao-2"}, 2},
		{"replace not expected", []string{"--input", write(current), servers, "--replace", "openbao-9"}, 2},
		{"positional argument", []string{"--input", write(current), servers, "--replace", "openbao-1", "extra"}, 2},
		{"missing input", []string{servers, "--replace", "openbao-1"}, 2},
		{"unopenable input", []string{"--input", filepath.Join(t.TempDir(), "absent.json"), servers, "--replace", "openbao-1"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binary, tc.args...)
			out, _ := cmd.Output()
			if code := cmd.ProcessState.ExitCode(); code != tc.code {
				t.Fatalf("exit %d, want %d; stdout %s", code, tc.code, out)
			}
		})
	}
}
