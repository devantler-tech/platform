package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) != 1 {
		_, _ = fmt.Fprintln(os.Stdout, "EMAIL_DATA_RECOVERY=UNVERIFIED stage=dispatch cleanup=NOT_STARTED")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runRecovery(ctx, os.Getenv, kubectl{}, os.Stdout))
}
func runRecovery(ctx context.Context, env func(string) string, k kubernetes, output io.Writer) int {
	i, err := authorizeDataRecovery(env)
	if err != nil {
		_, _ = fmt.Fprintln(output, "EMAIL_DATA_RECOVERY=UNVERIFIED stage=dispatch cleanup=NOT_STARTED")
		return 2
	}
	d := &recoveryDriver{i: i, k: k, refs: map[string]ownedReference{}, stage: "preflight"}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err = d.execute(runCtx); err != nil {
		cleanup := "NOT_STARTED"
		if d.started || d.uncertain || len(d.refs) != 0 {
			cleanup = "RETAINED"
		}
		_, _ = fmt.Fprintf(output, "EMAIL_DATA_RECOVERY=UNVERIFIED stage=%s cleanup=%s\n", d.stage, cleanup)
		return 2
	}
	d.stage = "cleanup"
	cleanupCtx, finish := context.WithTimeout(context.Background(), 10*time.Minute)
	defer finish()
	if err = d.cleanupRestore(cleanupCtx); err != nil {
		_, _ = fmt.Fprintln(output, "EMAIL_DATA_RECOVERY=UNVERIFIED stage=cleanup cleanup=RETAINED")
		return 2
	}
	if _, err := fmt.Fprintln(output, "EMAIL_DATA_RECOVERY=PASS messages=VERIFIED storage=ENCRYPTED source=RETAINED restore_cleanup=VERIFIED backup=RETAINED_24H"); err != nil {
		return 2
	}
	return 0
}
