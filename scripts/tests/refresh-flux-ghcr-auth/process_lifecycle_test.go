package refreshfluxghcrauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exercise the real Go test deadline, rather than cancelling a mock command.
// The parent owns both process groups and always cleans its deliberate RED
// fixture; it never scans or signals unrelated host processes.
func TestCapturedCommandStopsDescendantsBeforePackageTimeout(t *testing.T) {
	t.Parallel()
	assertCapturedChildStops(t, "deadline")
}

func TestCapturedCommandRejectsBackgroundChildrenAfterExit(t *testing.T) {
	for _, mode := range []string{"success", "failure"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			assertCapturedChildStops(t, mode)
		})
	}
}

func TestCapturedCommandPreservesCompletedResult(t *testing.T) {
	for _, status := range []int{0, 7} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			result := runCaptured(t, t.TempDir(), environmentMap(os.Environ()),
				"/bin/bash", "-c", `printf 'complete output'; printf 'complete error' >&2; exit "$1"`,
				"bash", strconv.Itoa(status))
			if result.exitCode != status || result.stdout != "complete output" || result.stderr != "complete error" {
				t.Fatalf("completed command result changed: %#v", result)
			}
		})
	}
}

func assertCapturedChildStops(t *testing.T, mode string) {
	t.Helper()
	pidPath := filepath.Join(t.TempDir(), "owned-pids")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, self,
		"-test.run=^TestCapturedCommandDeadlineChild$", "-test.timeout=12s")
	child.Env = append(os.Environ(), "GHCR_CAPTURE_DEADLINE_CHILD="+pidPath,
		"GHCR_CAPTURE_CHILD_MODE="+mode)
	child.Dir = rootPath
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.WaitDelay = time.Second
	output := &syncBuffer{}
	child.Stdout, child.Stderr = output, output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// The group is created by this test. The helper may get its own
		// group once the command transport is repaired.
		if pids, readErr := readOwnedPIDs(pidPath); readErr == nil {
			if group, groupErr := syscall.Getpgid(pids[1]); groupErr == nil &&
				(group == child.Process.Pid || group == pids[0]) {
				_ = syscall.Kill(-group, syscall.SIGKILL)
			}
		}
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
	}()

	waited := make(chan error, 1)
	go func() { waited <- child.Wait() }()
	sentinel := exec.CommandContext(ctx, "sleep", "60")
	sentinel.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sentinel.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sentinel.Process.Kill()
		_ = sentinel.Wait()
	}()
	var pids [2]int
	var waitErr error
	completed := false
observe:
	for {
		pids, err = readOwnedPIDs(pidPath)
		if err == nil {
			break
		}
		select {
		case waitErr = <-waited:
			completed = true
			// A fast command may finish between the file read and select.
			pids, err = readOwnedPIDs(pidPath)
			if err != nil {
				t.Fatalf("deadline fixture never started: %v\n%s", waitErr, output.String())
			}
			break observe
		case <-ctx.Done():
			t.Fatalf("deadline fixture did not publish its child identities: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}

	if !completed {
		select {
		case waitErr = <-waited:
		case <-ctx.Done():
			t.Fatalf("fixture transport did not finish: %v", ctx.Err())
		}
	}
	if waitErr == nil {
		t.Error("an incomplete fixture lifecycle was reported as successful")
	}
	if strings.Contains(output.String(), "panic: test timed out") {
		t.Error("Go reached its hard package timeout before the fixture stopped")
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pids[1], 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pids[1], 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("test-owned descendant survived helper completion: %v", err)
	}
	if err := sentinel.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("cancelling the fixture affected a separate owned process group: %v", err)
	}
}

// Invoked only by the native subprocess regression above. The shell and its
// descendant use no fake API or credential and must both stop at the deadline.
func TestCapturedCommandDeadlineChild(t *testing.T) {
	pidPath := os.Getenv("GHCR_CAPTURE_DEADLINE_CHILD")
	if pidPath == "" {
		return
	}
	script := `sleep 60 & child=$!; printf '%s %s\n' "$$" "$child" > "$1"; wait "$child"`
	status := "0"
	if mode := os.Getenv("GHCR_CAPTURE_CHILD_MODE"); mode != "deadline" {
		script = `sleep 60 >/dev/null 2>&1 & child=$!; printf '%s %s\n' "$$" "$child" > "$1"; exit "$2"`
		if mode == "failure" {
			status = "7"
		}
	}
	runCaptured(t, rootPath, environmentMap(os.Environ()), "/bin/bash", "-c", script,
		"bash", pidPath, status)
}

func readOwnedPIDs(path string) ([2]int, error) {
	var pids [2]int
	data, err := os.ReadFile(path)
	if err != nil {
		return pids, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return pids, fmt.Errorf("expected two fixture process identities")
	}
	for i, field := range fields {
		pids[i], err = strconv.Atoi(field)
		if err != nil || pids[i] <= 1 {
			return pids, fmt.Errorf("invalid fixture process identity")
		}
	}
	return pids, nil
}

func TestFixtureGroupObservationRejectsIncompleteAndForeignState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		live    bool
		refused bool
	}{
		{"absent group", "1 1 0 S\n", false, false},
		{"system nobody uid outside group", "1 1 -2 S\n", false, false},
		{"own running child", "1 1 0 S\n43 42 1000 S+\n", true, false},
		{"terminated child", "1 1 0 S\n43 42 1000 Z\n", false, false},
		{"mixed group", "43 42 1000 Z\n44 42 1000 S\n", true, false},
		{"foreign live child", "43 42 0 S\n", false, true},
		{"empty observation", "", false, true},
		{"blank observation", "\n ", false, true},
		{"malformed later row", "1 1 0 S\n43 42\n", false, true},
		{"invalid numeric identity", "43 invalid 1000 S\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, err := parseFixtureGroup([]byte(tc.data), 42, 1000)
			if (err != nil) != tc.refused || live != tc.live {
				t.Fatalf("live=%v err=%v; want live=%v refused=%v", live, err, tc.live, tc.refused)
			}
		})
	}
}

func TestFixtureGroupProbeRefusesPlausiblePartialOutputOnFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, "/bin/bash", "-c", "printf '1 1 0 S\\n'; exit 9")
	if live, err := inspectFixtureGroup(probe, 42, 1000); err == nil || live {
		t.Fatalf("failed process read was treated as a clean group: live=%v err=%v", live, err)
	}
}
