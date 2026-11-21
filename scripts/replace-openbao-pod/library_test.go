package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// judgeLibrary runs scripts/judge-openbao-replacement.lib.sh with stand-ins for
// the collector and both judges. Each stand-in records its arguments and
// exits with the code given for it; the collector writes a capture only when
// it succeeds, as the real one does.
func judgeLibrary(t *testing.T, env map[string]string, script string) (string, string, int) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	private := filepath.Join(dir, "private")
	for _, d := range []string{bin, private} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(dir, "calls")
	stub := func(name, body string) {
		path := filepath.Join(bin, name)
		content := "#!/usr/bin/env bash\necho \"" + name + " $*\" >> \"$CALLS\"\n" + body
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stub("capture-openbao-replacement", `[[ "$CAPTURE_CODE" == 0 ]] || exit "$CAPTURE_CODE"
printf '%s' "$CAPTURE_HEALTH" > "$4/health.json"; echo '{}' > "$4/quorum.json"`)
	stub("judge-openbao-health", `exit "$HEALTH_CODE"`)
	stub("judge-openbao-quorum", `exit "$QUORUM_CODE"`)
	cmd := exec.Command("bash", "-c", "set -euo pipefail; source ../judge-openbao-replacement.lib.sh; "+script)
	cmd.Env = append(os.Environ(),
		"OPENBAO_BIN="+bin, "PRIVATE="+private, "CALLS="+calls,
		"OPENBAO_SERVERS=openbao-0,openbao-1,openbao-2", "OPENBAO_VERSION=2.7.2",
		"REPLACEMENT_SERVER=openbao-2", "PREVIOUS_VERSION=",
		"CAPTURE_CODE=0", "HEALTH_CODE=0", "QUORUM_CODE=0",
		`CAPTURE_HEALTH={"servers":[{"name":"openbao-0","podUID":"uid-0"},{"name":"openbao-2","podUID":"uid-2"}]}`)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	recorded, _ := os.ReadFile(calls)
	return strings.TrimSpace(string(out)), string(recorded), code
}

func TestJudgementCombinesBothJudges(t *testing.T) {
	cases := []struct {
		capture, health, quorum string
		want                    string
	}{
		{"0", "0", "0", "0 PROVEN"},
		{"0", "1", "0", "1 HOLD"},
		{"0", "0", "1", "1 HOLD"},
		{"0", "1", "2", "2 UNKNOWN"},
		{"0", "2", "1", "2 UNKNOWN"},
		{"0", "0", "3", "2 UNKNOWN"},
		{"2", "0", "0", "2 UNKNOWN"},
	}
	for _, c := range cases {
		out, _, _ := judgeLibrary(t, map[string]string{"CAPTURE_CODE": c.capture, "HEALTH_CODE": c.health, "QUORUM_CODE": c.quorum},
			`s=0; judge_openbao_replacement "$PRIVATE" "" || s=$?; echo "$s $(openbao_verdict "$s")"`)
		if out != c.want {
			t.Fatalf("capture=%s health=%s quorum=%s: got %q, want %q", c.capture, c.health, c.quorum, out, c.want)
		}
	}
}

// A failed capture must never leave the previous capture to be judged.
func TestFailedCaptureNeverJudgesAnOlderOne(t *testing.T) {
	out, calls, _ := judgeLibrary(t, map[string]string{"CAPTURE_CODE": "2"},
		`echo old > "$PRIVATE/health.json"; echo old > "$PRIVATE/quorum.json"
s=0; judge_openbao_replacement "$PRIVATE" "" || s=$?; echo "$s"; ls "$PRIVATE" | wc -l | tr -d ' '`)
	if out != "2\n0" || strings.Contains(calls, "judge-openbao") {
		t.Fatalf("got %q, calls %q", out, calls)
	}
}

func TestRollingUpgradeFlagsComeOnlyFromADeclaredUpgrade(t *testing.T) {
	_, calls, _ := judgeLibrary(t, nil, `judge_openbao_replacement "$PRIVATE" "openbao-0"`)
	if strings.Contains(calls, "--previous-version") || strings.Contains(calls, "--require-current") {
		t.Fatalf("rolling flags without a declared upgrade: %q", calls)
	}
	if !strings.Contains(calls, "judge-openbao-quorum --input") || !strings.Contains(calls, "--replace openbao-2 --max-age 5m") {
		t.Fatalf("quorum not judged for the named server: %q", calls)
	}
	_, calls, _ = judgeLibrary(t, map[string]string{"PREVIOUS_VERSION": "2.6.3"}, `judge_openbao_replacement "$PRIVATE" ""`)
	if !strings.Contains(calls, "--expect-version 2.7.2 --previous-version 2.6.3 --max-age 5m") {
		t.Fatalf("declared upgrade not passed on: %q", calls)
	}
	_, calls, _ = judgeLibrary(t, map[string]string{"PREVIOUS_VERSION": "2.6.3"}, `judge_openbao_replacement "$PRIVATE" "openbao-0,openbao-2"`)
	if !strings.Contains(calls, "--previous-version 2.6.3 --require-current openbao-0,openbao-2 --max-age 5m") {
		t.Fatalf("replaced servers not required on the new release: %q", calls)
	}
}

func TestPodUIDComesFromExactlyOneServerEntry(t *testing.T) {
	out, _, code := judgeLibrary(t, nil, `judge_openbao_replacement "$PRIVATE" ""; openbao_pod_uid "$PRIVATE" openbao-2`)
	if code != 0 || out != "uid-2" {
		t.Fatalf("got %d %q", code, out)
	}
	for _, health := range []string{
		`{"servers":[{"name":"openbao-0","podUID":"uid-0"}]}`,
		`{"servers":[{"name":"openbao-2","podUID":"uid-2"},{"name":"openbao-2","podUID":"uid-x"}]}`,
		`{"servers":[{"name":"openbao-2"}]}`,
	} {
		_, _, code := judgeLibrary(t, map[string]string{"CAPTURE_HEALTH": health}, `judge_openbao_replacement "$PRIVATE" ""; openbao_pod_uid "$PRIVATE" openbao-2`)
		if code == 0 {
			t.Fatalf("accepted %s", health)
		}
	}
}
