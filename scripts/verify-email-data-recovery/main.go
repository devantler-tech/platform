// Command verify-email-data-recovery proves that Email mailbox data restores
// from a fresh backup into an isolated server, without moving the serving claim.
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
	if len(os.Args) == 2 && os.Args[1] == "--protect-retained-volume" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		os.Exit(runRetainedVolumeProtection(ctx, os.Getenv, kubectl{}, verifyReaderCustodySource, os.Stdout))
	}
	if len(os.Args) == 2 && os.Args[1] == "--prepare-reader-inputs" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		os.Exit(runReaderInputCustody(ctx, os.Getenv, kubectl{}, verifyReaderCustodySource, loadReaderCustodyStore, os.Stdout))
	}
	if len(os.Args) == 2 && os.Args[1] == "--dispose-retained" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		os.Exit(runRetainedDisposal(ctx, os.Getenv, kubectl{}, os.Stdout))
	}
	if len(os.Args) == 2 && os.Args[1] == "--observe-retained" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		os.Exit(runRecoveryObservation(ctx, os.Getenv, kubectl{}, os.Stdout))
	}
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
	// Cleanup keeps dispatch context values but has an independent deadline:
	// cancellation after verification must not interrupt resource disposal.
	cleanupCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
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
