package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

const sampleLog = "2026-11-15T09:58:45.1Z PROD_RECOVERY_SOURCE=PASS\n" +
	"2026-11-15T09:58:50.1Z PROD_OPERATION_EXIT=3\n" +
	"2026-11-15T09:58:50.2Z ----- BEGIN PROD OPERATION OUTPUT -----\n" +
	"2026-11-15T09:58:50.2Z -----BEGIN AGE ENCRYPTED FILE-----\n" +
	"2026-11-15T09:58:50.2Z YWdlLWVuY3J5cHRpb24=\n" +
	"2026-11-15T09:58:50.2Z -----END AGE ENCRYPTED FILE-----\n" +
	"2026-11-15T09:58:50.3Z ----- END PROD OPERATION OUTPUT -----\n" +
	"2026-11-15T09:58:51.0Z Post job cleanup.\n"

func TestExtractStripsTimestampsAndKeepsTheExitStatus(t *testing.T) {
	blocks, err := extract(sampleLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "-----BEGIN AGE ENCRYPTED FILE-----\nYWdlLWVuY3J5cHRpb24=\n-----END AGE ENCRYPTED FILE-----\n"
	if len(blocks) != 1 || blocks[0].exit != "3" || blocks[0].ciphertext != want {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtractRefusesAnUnterminatedBlock(t *testing.T) {
	if _, err := extract(strings.Split(sampleLog, "----- END")[0]); err == nil {
		t.Fatal("unterminated block accepted")
	}
}

func fakeRunner(log string, decryptErr error, calls *[]string) runner {
	return func(_ context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "gh" && strings.HasSuffix(args[1], "/jobs"):
			return []byte("111\n"), nil
		case name == "gh":
			return []byte(log), nil
		case name == "age":
			if decryptErr != nil {
				return nil, decryptErr
			}
			in, _ := io.ReadAll(stdin)
			return []byte("plaintext of " + strings.TrimSpace(strings.Split(string(in), "\n")[1]) + "\n"), nil
		}
		return nil, errors.New("unexpected command")
	}
}

func TestReadDecryptsEachBlockWithTheIdentity(t *testing.T) {
	var calls []string
	var out bytes.Buffer
	if err := read(context.Background(), "43699809761", "/id.txt", fakeRunner(sampleLog, nil, &calls), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "== job 111, exit 3\nplaintext of YWdlLWVuY3J5cHRpb24=\n" {
		t.Fatalf("got %q", out.String())
	}
	if calls[0] != "gh api repos/devantler-tech/platform/actions/runs/43699809761/jobs --jq .jobs[].id" ||
		calls[2] != "age --decrypt --identity /id.txt" {
		t.Fatalf("calls %q", calls)
	}
}

func TestReadFailsClosed(t *testing.T) {
	var calls []string
	for name, tc := range map[string]struct {
		runID string
		log   string
		err   error
	}{
		"non-numeric run id": {"1;rm", sampleLog, nil},
		"no encrypted block": {"1", "PROD_OPERATION=UNVERIFIED\n", nil},
		"decryption refused": {"1", sampleLog, errors.New("no identity matched")},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := read(context.Background(), tc.runID, "/id.txt", fakeRunner(tc.log, tc.err, &calls), &out); err == nil || out.Len() != 0 {
				t.Fatalf("want an error and no output, got %v %q", err, out.String())
			}
		})
	}
}

func TestIdentityFileOrder(t *testing.T) {
	env := map[string]string{"HOME": "/home/me"}
	get := func(k string) string { return env[k] }
	if got := identityFile(get); got != "/home/me/.config/sops/age/keys.txt" {
		t.Fatalf("default %q", got)
	}
	env["SOPS_AGE_KEY_FILE"] = "/sops.txt"
	if got := identityFile(get); got != "/sops.txt" {
		t.Fatalf("sops %q", got)
	}
	env["AGE_IDENTITY_FILE"] = "/age.txt"
	if got := identityFile(get); got != "/age.txt" {
		t.Fatalf("age %q", got)
	}
}
