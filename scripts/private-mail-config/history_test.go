package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if calls != 2 || observations != 4 || receipt.String() != "MAIL_HISTORY_IMPORT=COMPLETE folders=2 messages=5 source=PRESERVED routing=UNCHANGED\n" {
		t.Fatalf("copy/reconciliation incomplete: calls=%d observations=%d receipt=%q", calls, observations, &receipt)
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
	for _, broken := range []string{"copy-failure", "verification-failure", "unidentified", "missing-summary", "partial-summary", "reported-error", "missing-folder", "missing-message", "removed-destination", "source-changed", "source-shrank", "source-revalidated", "source-folder-gone", "source-overgrew", "read-failed"} {
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
					if observations > 2 {
						// Each case changes the source in a way growth cannot explain.
						switch broken {
						case "source-changed": // its next UID went back
							return historySnapshot{"INBOX": {messages: 2, uidNext: 6, uidValidity: 1}}, nil
						case "source-shrank":
							return historySnapshot{"INBOX": {messages: 1, uidNext: 7, uidValidity: 1}}, nil
						case "source-revalidated":
							return historySnapshot{"INBOX": {messages: 2, uidNext: 7, uidValidity: 2}}, nil
						case "source-folder-gone":
							return historySnapshot{"Other": {messages: 2, uidNext: 7, uidValidity: 1}}, nil
						case "source-overgrew": // more new messages than new UIDs
							return historySnapshot{"INBOX": {messages: 4, uidNext: 8, uidValidity: 1}}, nil
						}
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
			err = importHistory(context.Background(), v, observe, execute, &receipt)
			if err == nil {
				t.Fatal("incomplete copy reported success")
			}
			if strings.HasPrefix(broken, "source-") && err.Error() != "source-change" {
				t.Fatalf("a source change was reported as %v", err)
			}
			if receipt.Len() != 0 {
				t.Fatalf("failure wrote a completion receipt: %q", &receipt)
			}
		})
	}
}

// Until MX moves the source keeps receiving mail, so a copy must still complete
// when the source only grew while it ran: more messages, higher next UIDs and a
// new folder. The receipt counts the messages observed at the start.
func TestHistoryCompletesWhileTheSourceKeepsReceivingMail(t *testing.T) {
	v, err := loadHistoryInputs(historyInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	sourceReads := 0
	observe := func(_ context.Context, side string, _ historyConfig) (historySnapshot, error) {
		if side == "source" {
			sourceReads++
			if sourceReads > 1 {
				// One message arrived and a rule filed another into a new folder.
				return historySnapshot{"INBOX": {messages: 3, uidNext: 9, uidValidity: 1}, "Receipts": {messages: 1, uidNext: 2, uidValidity: 5}}, nil
			}
			return historySnapshot{"INBOX": {messages: 2, uidNext: 7, uidValidity: 1}}, nil
		}
		return historySnapshot{"INBOX": {messages: 3}}, nil
	}
	execute := func(_ context.Context, _ []string, output io.Writer) error {
		_, err := io.WriteString(output, "The sync looks good, all 3 identified messages in host1 are on host2.\nThere is no unidentified message on host1.\nDetected 0 errors\n")
		return err
	}
	var receipt bytes.Buffer
	if err := importHistory(context.Background(), v, observe, execute, &receipt); err != nil {
		t.Fatalf("a copy of a growing source was refused as %v", err)
	}
	if receipt.String() != "MAIL_HISTORY_IMPORT=COMPLETE folders=1 messages=2 source=PRESERVED routing=UNCHANGED\n" {
		t.Fatalf("unexpected receipt %q", &receipt)
	}
}
