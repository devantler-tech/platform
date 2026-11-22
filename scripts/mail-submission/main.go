package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/emersion/go-smtp"
)

func run(ctx context.Context, args []string, output io.Writer) int {
	if len(args) != 3 || (args[0] != "check" && args[0] != "serve") || args[1] != "--config-dir" || args[2] == "" {
		_, _ = fmt.Fprintln(output, "Usage: mail-submission <check|serve> --config-dir <private-directory>")
		return 1
	}
	cfg, err := loadConfiguration(args[2])
	if err != nil {
		_, _ = fmt.Fprintln(output, "Submission configuration refused")
		return 1
	}
	if args[0] == "check" {
		_, _ = fmt.Fprintln(output, "Submission configuration valid; no listener started")
		return 0
	}
	g, err := newGateway(cfg, output)
	if err != nil {
		_, _ = fmt.Fprintln(output, "Submission configuration refused")
		return 1
	}
	listener, err := net.Listen("tcp", cfg.Settings.ListenAddress)
	if err != nil {
		_, _ = fmt.Fprintln(output, "Submission listener unavailable")
		return 1
	}
	secureListener := newTLSListener(listener, g.cfg.ServerTLS)
	finished := make(chan error, 1)
	go func() { finished <- g.server.Serve(secureListener) }()
	select {
	case err = <-finished:
		if errors.Is(err, smtp.ErrServerClosed) {
			return 0
		}
		_, _ = fmt.Fprintln(output, "Submission listener stopped")
		return 1
	case <-ctx.Done():
		// Stop Accept and join Serve before waiting for active connections.
		// This also handles cancellation before the library registers its listener.
		_ = secureListener.Close()
		<-finished
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err = g.server.Shutdown(shutdown); err != nil {
			secureListener.closeConnections()
		}
		return 0
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}
