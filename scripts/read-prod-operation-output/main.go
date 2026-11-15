// Command read-prod-operation-output decrypts what a production operation
// printed (platform#6268).
//
// This repository's Actions logs are public, so prod-operation.yaml publishes
// an operation's stdout and stderr only as age ciphertext for the
// maintainer-held recipient. This command fetches a run's job logs with the
// caller's `gh` login, cuts out each encrypted block and decrypts it locally
// with `age`, so the plaintext never leaves the maintainer's machine.
//
// Usage:
//
//	go run ./scripts/read-prod-operation-output <run-id>
//
// The age identity is read from AGE_IDENTITY_FILE, then SOPS_AGE_KEY_FILE,
// then ~/.config/sops/age/keys.txt.
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
	"regexp"
	"strings"
	"time"
)

const (
	repository = "devantler-tech/platform"
	beginLine  = "----- BEGIN PROD OPERATION OUTPUT -----"
	endLine    = "----- END PROD OPERATION OUTPUT -----"
	exitPrefix = "PROD_OPERATION_EXIT="
)

var (
	runIDPattern     = regexp.MustCompile(`^[0-9]{1,20}$`)
	timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z `)
)

// block is one operation's published result in a job log.
type block struct {
	exit       string
	ciphertext string
}

// extract returns every encrypted block in a job log, with the exit status
// printed just before it. A BEGIN without an END is an error, never a block.
func extract(log string) ([]block, error) {
	var (
		blocks []block
		exit   string
		cipher strings.Builder
		inside bool
	)
	for _, raw := range strings.Split(strings.ReplaceAll(log, "\r\n", "\n"), "\n") {
		line := timestampPattern.ReplaceAllString(raw, "")
		switch {
		case inside && line == endLine:
			blocks = append(blocks, block{exit: exit, ciphertext: cipher.String()})
			cipher.Reset()
			exit, inside = "", false
		case inside:
			cipher.WriteString(line)
			cipher.WriteByte('\n')
		case line == beginLine:
			inside = true
		case strings.HasPrefix(line, exitPrefix):
			exit = strings.TrimPrefix(line, exitPrefix)
		}
	}
	if inside {
		return nil, errors.New("an encrypted block is not terminated")
	}
	return blocks, nil
}

type runner func(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func identityFile(getenv func(string) string) string {
	for _, key := range []string{"AGE_IDENTITY_FILE", "SOPS_AGE_KEY_FILE"} {
		if v := getenv(key); v != "" {
			return v
		}
	}
	return filepath.Join(getenv("HOME"), ".config", "sops", "age", "keys.txt")
}

func read(ctx context.Context, runID string, identity string, run runner, out io.Writer) error {
	if !runIDPattern.MatchString(runID) {
		return fmt.Errorf("run id %q is not a number", runID)
	}
	jobs, err := run(ctx, nil, "gh", "api", fmt.Sprintf("repos/%s/actions/runs/%s/jobs", repository, runID), "--jq", ".jobs[].id")
	if err != nil {
		return err
	}
	found := 0
	for _, job := range strings.Fields(string(jobs)) {
		if !runIDPattern.MatchString(job) {
			return fmt.Errorf("unexpected job id %q", job)
		}
		log, err := run(ctx, nil, "gh", "api", fmt.Sprintf("repos/%s/actions/jobs/%s/logs", repository, job))
		if err != nil {
			return err
		}
		blocks, err := extract(string(log))
		if err != nil {
			return fmt.Errorf("job %s: %w", job, err)
		}
		for _, b := range blocks {
			plain, err := run(ctx, strings.NewReader(b.ciphertext), "age", "--decrypt", "--identity", identity)
			if err != nil {
				return fmt.Errorf("job %s: %w", job, err)
			}
			if _, err := fmt.Fprintf(out, "== job %s, exit %s\n", job, b.exit); err != nil {
				return err
			}
			if _, err := out.Write(plain); err != nil {
				return err
			}
			found++
		}
	}
	if found == 0 {
		return errors.New("the run published no encrypted operation output")
	}
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/read-prod-operation-output <run-id>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := read(ctx, os.Args[1], identityFile(os.Getenv), execRunner, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "read-prod-operation-output:", err)
		os.Exit(1)
	}
}
