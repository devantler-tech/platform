package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This child process exercises the actual pipe, exit and cancellation path.
// All content is synthetic; no external command or cluster is contacted.
func TestMigrationCommandFixture(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	emit := func(destination io.Writer, data string) {
		if _, err := io.WriteString(destination, data); err != nil {
			os.Exit(3)
		}
	}
	switch os.Args[len(os.Args)-1] {
	case "success":
		emit(os.Stdout, "complete response")
	case "stdout-overflow":
		emit(os.Stdout, strings.Repeat("x", 17<<20))
	case "stderr-overflow":
		emit(os.Stderr, strings.Repeat("private synthetic detail", 4000))
	case "oversized-conflict":
		emit(os.Stderr, "Error from server (Conflict)"+strings.Repeat("x", 65<<10))
		os.Exit(1)
	case "partial-failure":
		emit(os.Stdout, "incomplete response")
		emit(os.Stderr, "private synthetic detail")
		os.Exit(1)
	case "conflict":
		emit(os.Stderr, "Error from server (Conflict): resource version changed")
		os.Exit(1)
	case "wait":
		time.Sleep(time.Minute)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestMigrationCommandRejectsConflictWithoutNaturalEOF(t *testing.T) {
	for _, exit := range []string{"0", "1"} {
		t.Run("exit-"+exit, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "owned-child.pid")
			t.Cleanup(func() {
				raw, err := os.ReadFile(pidFile)
				if err != nil {
					t.Error(err)
					return
				}
				pid, err := strconv.Atoi(string(raw))
				if err != nil || pid <= 0 {
					t.Fatal("invalid synthetic child PID")
				}
				child, err := os.FindProcess(pid)
				if err == nil {
					_ = child.Kill()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			const script = `sleep 60 &
printf '%s' "$!" > "$1"
printf '%s' 'Error from server (Conflict): incomplete private response' >&2
exit "$2"`
			output, err := execute(ctx, nil, "bash", "-c", script, "fixture", pidFile, exit)
			if err == nil || errors.Is(err, errConflict) || len(output) != 0 || strings.Contains(err.Error(), "private") {
				t.Fatal("forced pipe closure supplied an accepted response or retry")
			}
		})
	}
}

func TestMigrationCommandRequiresCompleteBoundedOutput(t *testing.T) {
	for _, mode := range []string{"success", "stdout-overflow", "stderr-overflow", "oversized-conflict", "partial-failure", "conflict", "wait"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if mode == "wait" {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
			}
			output, err := execute(ctx, nil, os.Args[0], "-test.run=^TestMigrationCommandFixture$", "--", mode)
			if mode == "success" {
				if err != nil || string(output) != "complete response" {
					t.Fatal("complete bounded command refused")
				}
				return
			}
			if err == nil || len(output) != 0 || strings.Contains(err.Error(), "private") {
				t.Fatal("overflow, partial output, cancellation or private error escaped")
			}
			if errors.Is(err, errConflict) != (mode == "conflict") {
				t.Fatal("only a complete bounded conflict response may be retried")
			}
		})
	}
}
