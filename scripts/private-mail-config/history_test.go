package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func historyInputs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{
		"source-username":      "owner@icloud.com",
		"source-password":      "private-source-canary",
		"destination-password": "private-destination-canary",
		"addresses":            "ned@devantler.tech,alias@devantler.tech",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestHistoryChecksPrivateInputsWithoutConnecting(t *testing.T) {
	dir := historyInputs(t)
	var output bytes.Buffer
	if got := command([]string{"history", "--input-dir", dir, "--check"}, &output); got != 0 {
		t.Fatalf("valid mounted history inputs refused (exit %d): %s", got, &output)
	}
	if output.String() != "MAIL_HISTORY_INPUTS=READY addresses=2\n" {
		t.Fatalf("unexpected public receipt: %q", output.String())
	}
	for _, private := range []string{"icloud.com", "ned@", "alias@", "canary", dir} {
		if strings.Contains(output.String(), private) {
			t.Fatal("private input reached the receipt")
		}
	}
}

func TestHistoryRefusesIncompleteOrForeignAddressInventory(t *testing.T) {
	for _, inventory := range []string{"", "alias@devantler.tech", "ned@devantler.tech,ned@devantler.tech", "ned@devantler.tech,other@example.org", "ned@devantler.tech\ncommand"} {
		t.Run(inventory, func(t *testing.T) {
			dir := historyInputs(t)
			if err := os.WriteFile(filepath.Join(dir, "addresses"), []byte(inventory), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if command([]string{"history", "--input-dir", dir, "--check"}, &output) == 0 {
				t.Fatal("invalid address inventory accepted")
			}
			if strings.Contains(output.String(), inventory) && inventory != "" {
				t.Fatal("invalid private inventory echoed")
			}
		})
	}
}

func TestHistoryRefusesUnsafeCredentialProjection(t *testing.T) {
	for _, kind := range []string{"missing", "public", "escape", "multiline", "empty"} {
		t.Run(kind, func(t *testing.T) {
			dir := historyInputs(t)
			path := filepath.Join(dir, "source-password")
			var err error
			switch kind {
			case "missing":
				err = os.Remove(path)
			case "public":
				err = os.Chmod(path, 0644)
			case "escape":
				outside := filepath.Join(t.TempDir(), "password")
				if err = os.WriteFile(outside, []byte("private-escape-canary"), 0600); err == nil {
					err = os.Remove(path)
				}
				if err == nil {
					err = os.Symlink(outside, path)
				}
			case "multiline":
				err = os.WriteFile(path, []byte("first\nsecond"), 0600)
			case "empty":
				err = os.WriteFile(path, nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if command([]string{"history", "--input-dir", dir, "--check"}, &output) == 0 {
				t.Fatal("unsafe credential input accepted")
			}
			if strings.Contains(output.String(), "canary") || strings.Contains(output.String(), dir) {
				t.Fatal("private input echoed on failure")
			}
		})
	}
}

func TestHistoryCopiesThenVerifiesWithoutLeakingToolOutput(t *testing.T) {
	v, err := loadHistoryInputs(historyInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	source := historySnapshot{"INBOX": {messages: 3, uidNext: 9, uidValidity: 1}, "Private folder": {messages: 2, uidNext: 4, uidValidity: 2}}
	beforeDestination := historySnapshot{"INBOX": {messages: 1}, "Keep me": {messages: 4}}
	afterDestination := historySnapshot{"INBOX": {messages: 3}, "Private folder": {messages: 2}, "Keep me": {messages: 4}}
	observations := 0
	observe := func(_ context.Context, side string, _ historyConfig) (historySnapshot, error) {
		observations++
		if side == "source" {
			return source, nil
		}
		if observations < 3 {
			return beforeDestination, nil
		}
		return afterDestination, nil
	}
	calls := 0
	execute := func(_ context.Context, args []string, output io.Writer) error {
		calls++
		if strings.Contains(strings.Join(args, " "), "private-source-canary") || strings.Contains(strings.Join(args, " "), "private-destination-canary") {
			t.Fatal("a password was put in process arguments")
		}
		if (calls == 2) != containsHistoryArg(args, "--dry") {
			t.Fatal("the second pass must only verify")
		}
		for _, required := range []string{"--noexpunge1", "--noexpunge2", "--nouidexpunge2", "--noresyncflags", "--noreleasecheck", "--nolog", "--noemailreport1", "--noemailreport2", "--syncduplicates", "--ssl1", "--ssl2"} {
			if !containsHistoryArg(args, required) {
				t.Fatalf("missing copy boundary %s", required)
			}
		}
		for _, forbidden := range []string{"--delete1", "--delete2", "--delete2duplicates", "--password1", "--password2", "--addheader", "--showpasswords", "--useuid"} {
			if containsHistoryArg(args, forbidden) {
				t.Fatalf("unsafe copy option %s", forbidden)
			}
		}
		// Identity is Message-Id alone, on both passes: never Received, which
		// made a few copies look missing on every run (#5481), and no weaker
		// fallback such as Date, which could match an unrelated message.
		var identity []string
		for i, arg := range args {
			if arg == "--useheader" && i+1 < len(args) {
				identity = append(identity, args[i+1])
			}
		}
		if !reflect.DeepEqual(identity, []string{"Message-Id"}) {
			t.Fatalf("messages must be identified by Message-Id alone, got %q", identity)
		}
		for _, peer := range []string{"SSL_verifycn_name=imap.mail.me.com", "SSL_verifycn_name=mail.devantler.tech", "SSL_verify_mode=1", "SSL_verifycn_scheme=imap"} {
			if !containsHistoryArg(args, peer) {
				t.Fatalf("missing verified TLS property %s", peer)
			}
		}
		_, err := io.WriteString(output, "private-source-canary Private folder private-message-subject\nThe sync looks good, all 5 identified messages in host1 are on host2.\nThere is no unidentified message on host1.\nDetected 0 errors\n")
		return err
	}
	var receipt bytes.Buffer
	if err := importHistory(context.Background(), v, observe, execute, &receipt); err != nil {
		t.Fatal(err)
	}
	unknown := " transferred=? would_transfer=? skipped=? h1_duplicates=? h2_duplicates=? h2_crossduplicates=? h1_noheader=? h2_noheader=? in1_not_in2=? in2_not_in1=?\n"
	want := "MAIL_HISTORY_IMPORT_PASS pass=copy exit=0 observed=5 identified=5 errors=0" + unknown +
		"MAIL_HISTORY_IMPORT_PASS pass=verify exit=0 observed=5 identified=5 errors=0" + unknown +
		"MAIL_HISTORY_IMPORT=COMPLETE folders=2 messages=5 source=PRESERVED routing=UNCHANGED\n"
	if calls != 2 || observations != 4 || receipt.String() != want {
		t.Fatalf("copy/reconciliation incomplete: calls=%d observations=%d receipt=%q", calls, observations, &receipt)
	}
}

// TestHistoryIdentifiedCountLiesBetweenLargestFolderAndTotal pins the success
// rule for imapsync's identified count: it counts distinct identities, so a
// message filed in two folders counts once and the import still completes,
// while a count below the largest folder (part of the mailbox unexamined) or
// above the total (the source changed) refuses.
func TestHistoryIdentifiedCountLiesBetweenLargestFolderAndTotal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identified int
		complete   bool
	}{
		{"same message in two folders", 4, true},
		{"every message distinct", 5, true},
		{"part of a folder unexamined", 2, false},
		{"more than the source holds", 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := loadHistoryInputs(historyInputs(t))
			if err != nil {
				t.Fatal(err)
			}
			source := historySnapshot{"INBOX": {messages: 3, uidNext: 9, uidValidity: 1}, "Sent": {messages: 2, uidNext: 5, uidValidity: 2}}
			observe := func(_ context.Context, side string, _ historyConfig) (historySnapshot, error) {
				if side == "source" {
					return source, nil
				}
				return historySnapshot{"INBOX": {messages: 3}, "Sent": {messages: 2}}, nil
			}
			execute := func(_ context.Context, _ []string, output io.Writer) error {
				_, err := fmt.Fprintf(output, "The sync looks good, all %d identified messages in host1 are on host2.\nThere is no unidentified message on host1.\nDetected 0 errors\n", tc.identified)
				return err
			}
			var receipt bytes.Buffer
			err = importHistory(context.Background(), v, observe, execute, &receipt)
			completed := strings.Contains(receipt.String(), "MAIL_HISTORY_IMPORT=COMPLETE folders=2 messages=5 ")
			if tc.complete != (err == nil && completed) {
				t.Fatalf("identified=%d of 5 (largest folder 3): complete=%v err=%v receipt=%q", tc.identified, completed, err, &receipt)
			}
		})
	}
}

// TestHistoryPassLinesCarryOnlyTheToolsCounts feeds the copy passes the tool's
// real closing statistics, wrapped in lines that name private folders and a
// subject. Each pass line must hold exactly the fixed counts, the exit status
// and nothing else, whether the import succeeds or fails.
func TestHistoryPassLinesCarryOnlyTheToolsCounts(t *testing.T) {
	v, err := loadHistoryInputs(historyInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	observe := func(_ context.Context, _ string, _ historyConfig) (historySnapshot, error) {
		return historySnapshot{"INBOX": {messages: 7, uidNext: 9, uidValidity: 1}}, nil
	}
	exitStatus := exec.Command("sh", "-c", "exit 111").Run()
	if exitStatus == nil {
		t.Fatal("could not produce a tool exit status")
	}
	statistics := func(transferred string, inOneNotTwo, inTwoNotOne int) string {
		return fmt.Sprintf("Host1: folder [Client secret project] selected 7 messages, duplicates 1\n"+
			"msg Client secret project/12 {420} copied to Client secret project/30 1.00 msgs/s\n"+
			"- msg INBOX/5 {9} could not append ( Subject:[private-message-subject] ) to folder INBOX: NO private-server-reply\n"+
			"Messages found in host1 not in host2    : 99 messages\n"+
			"++++ Statistics\n"+
			"Transfer started on                     : private-date-canary\n"+
			"Messages transferred                    : %s\n"+
			"Messages skipped                        : 4\n"+
			"Messages found duplicate on host1       : 1\n"+
			"Messages found duplicate on host2       : 2\n"+
			"Messages found crossduplicate on host2  : 0\n"+
			"Messages void (noheader) on host1       : 0  \n"+
			"Messages void (noheader) on host2       : 0\n"+
			"Messages found in host1 not in host2    : %d messages\n"+
			"Messages found in host2 not in host1    : %d messages\n"+
			"Messages found in host1 not in host2    : 77 messages\n"+
			"Messages found in private-folder-canary : 5 messages\n"+
			"The sync is not finished, there are %d among 7 identified messages in host1 that are not on host2.\n"+
			"There is no unidentified message on host1.\n"+
			"Detected 1 errors\n", transferred, inOneNotTwo, inTwoNotOne, inOneNotTwo)
	}
	calls := 0
	execute := func(_ context.Context, _ []string, output io.Writer) error {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(output, statistics("3 ", 0, 0))
			return nil
		}
		_, _ = io.WriteString(output, statistics("0 \t(could be 2 without --dry mode)", 2, 3))
		return exitStatus
	}
	var receipt bytes.Buffer
	if err := importHistory(context.Background(), v, observe, execute, &receipt); err == nil || err.Error() != "copy" {
		t.Fatalf("a pass reporting an error must fail at copy, got %v", err)
	}
	want := "MAIL_HISTORY_IMPORT_PASS pass=copy exit=0 observed=7 identified=7 errors=1 transferred=3 would_transfer=? skipped=4 h1_duplicates=1 h2_duplicates=2 h2_crossduplicates=0 h1_noheader=0 h2_noheader=0 in1_not_in2=0 in2_not_in1=0\n"
	if receipt.String() != want {
		t.Fatalf("copy pass line:\n got %q\nwant %q", &receipt, want)
	}

	// The verification pass reports the dry transfers and the tool's exit status.
	calls = 1
	result := historyReport{}
	runErr := execute(context.Background(), nil, &result)
	got := historyPassLine("verify", runErr, &result, 7)
	want = "MAIL_HISTORY_IMPORT_PASS pass=verify exit=111 observed=7 identified=7 errors=1 transferred=0 would_transfer=2 skipped=4 h1_duplicates=1 h2_duplicates=2 h2_crossduplicates=0 h1_noheader=0 h2_noheader=0 in1_not_in2=2 in2_not_in1=3\n"
	if got != want {
		t.Fatalf("verify pass line:\n got %q\nwant %q", got, want)
	}
	if got := historyPassLine("verify", errors.New("private-tool-error-canary"), &historyReport{}, 0); strings.Contains(got, "private") || !strings.Contains(got, " exit=? ") {
		t.Fatalf("an error without an exit status must read as unknown: %q", got)
	}
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	if got := historyPassLine("copy", killed, &historyReport{}, 0); !strings.Contains(got, " exit=signal ") {
		t.Fatalf("a killed tool must read as signal, not a negative status: %q", got)
	}

	// A tool whose output breaks the line bound must still run to its end:
	// the report keeps draining, so the tool is never blocked on a full pipe,
	// exits with its own status, and the pass is judged a failure.
	overlong := exec.Command("sh", "-c", `i=0; while [ $i -lt 3000 ]; do printf 'private-overlong-canary-%050d' $i; i=$((i+1)); done; printf '\nDetected 0 errors\n'; exit 114`)
	var drained historyReport
	overlong.Stdout, overlong.Stderr = &drained, &drained
	done := make(chan error, 1)
	go func() { done <- overlong.Run() }()
	var overlongErr error
	select {
	case overlongErr = <-done:
	case <-time.After(30 * time.Second):
		_ = overlong.Process.Kill()
		t.Fatal("a tool printing an over-bound line was blocked instead of drained")
	}
	if !drained.bad || !drained.zero {
		t.Fatalf("the over-bound line must fail the pass while later lines are still read: bad=%v zero=%v", drained.bad, drained.zero)
	}
	if got := historyPassLine("copy", overlongErr, &drained, 0); !strings.Contains(got, " exit=114 ") || strings.Contains(got, "canary") {
		t.Fatalf("a drained over-bound run must keep the tool's exit status and nothing else: %q", got)
	}
	var recovered historyReport
	_, _ = io.WriteString(&recovered, "Detected 3 errors\nDetected 0 errors\n")
	if got := historyPassLine("copy", nil, &recovered, 0); !strings.Contains(got, " errors=3 ") || !recovered.bad {
		t.Fatalf("a later zero count must not hide the errors that failed the pass: bad=%v %q", recovered.bad, got)
	}
	var flood historyReport
	if n, err := flood.Write(make([]byte, 129<<20)); err != nil || n != 129<<20 || !flood.bad {
		t.Fatalf("output past the total bound must be discarded and fail the pass, not refused: n=%d err=%v bad=%v", n, err, flood.bad)
	}
	for _, line := range []string{receipt.String(), got} {
		for _, private := range []string{"Client", "secret", "private", "subject", "reply", "INBOX", "canary"} {
			if strings.Contains(line, private) {
				t.Fatalf("pass line leaked %q: %q", private, line)
			}
		}
	}
}

func containsHistoryArg(args []string, wanted string) bool {
	for _, arg := range args {
		if arg == wanted {
			return true
		}
	}
	return false
}

func TestHistoryNeverCompletesAnIncompleteCopy(t *testing.T) {
	for _, broken := range []string{"copy-failure", "verification-failure", "unidentified", "missing-summary", "partial-summary", "reported-error", "missing-folder", "missing-message", "removed-destination", "source-changed", "read-failed"} {
		t.Run(broken, func(t *testing.T) {
			v, err := loadHistoryInputs(historyInputs(t))
			if err != nil {
				t.Fatal(err)
			}
			observations, executions := 0, 0
			observe := func(_ context.Context, side string, _ historyConfig) (historySnapshot, error) {
				observations++
				if broken == "read-failed" {
					return nil, errors.New("private-server-error-canary")
				}
				if side == "source" {
					if broken == "source-changed" && observations > 2 {
						return historySnapshot{"INBOX": {messages: 2, uidNext: 8, uidValidity: 1}}, nil
					}
					return historySnapshot{"INBOX": {messages: 2, uidNext: 7, uidValidity: 1}}, nil
				}
				if broken == "missing-folder" && observations > 2 {
					return historySnapshot{}, nil
				}
				if broken == "missing-message" && observations > 2 {
					return historySnapshot{"INBOX": {messages: 1}}, nil
				}
				if broken == "removed-destination" && observations <= 2 {
					return historySnapshot{"INBOX": {messages: 2}, "Keep me": {messages: 1}}, nil
				}
				return historySnapshot{"INBOX": {messages: 2}}, nil
			}
			execute := func(_ context.Context, _ []string, output io.Writer) error {
				executions++
				if broken == "copy-failure" || broken == "verification-failure" && executions == 2 {
					return errors.New("private-tool-error-canary")
				}
				if broken == "missing-summary" {
					return nil
				}
				if broken == "unidentified" {
					_, _ = io.WriteString(output, "There are 1 unidentified messages\nDetected 0 errors\n")
					return nil
				}
				if broken == "partial-summary" {
					_, _ = io.WriteString(output, "The sync looks good, all 1 identified messages in host1 are on host2.\nThere is no unidentified message on host1.\nDetected 0 errors\n")
					return nil
				}
				if broken == "reported-error" {
					_, _ = io.WriteString(output, "Detected 1 errors\n")
				}
				_, err := io.WriteString(output, "The sync looks good, all 2 identified messages in host1 are on host2.\nThere is no unidentified message on host1.\nDetected 0 errors\n")
				return err
			}
			var receipt bytes.Buffer
			if err := importHistory(context.Background(), v, observe, execute, &receipt); err == nil {
				t.Fatal("incomplete copy reported success")
			}
			if strings.Contains(receipt.String(), "MAIL_HISTORY_IMPORT=COMPLETE") {
				t.Fatalf("failure wrote a completion receipt: %q", &receipt)
			}
			for _, line := range strings.SplitAfter(receipt.String(), "\n") {
				if line != "" && !strings.HasPrefix(line, "MAIL_HISTORY_IMPORT_PASS ") || strings.Contains(line, "canary") || strings.Contains(line, "private") {
					t.Fatalf("failure receipt holds more than pass lines: %q", &receipt)
				}
			}
		})
	}
}
