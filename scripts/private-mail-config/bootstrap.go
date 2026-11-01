package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// A normal administration Service must never select the recovery listener.
// Stalwart's supported recovery override changes its port, not its bind address.
const bootstrapRecoveryPort = "18080"

type bootstrapRuntime struct {
	server, cli, url, dataDir          string
	applicationFile, applicationDigest string
	serverArgs                         []string
	startup, shutdown                  time.Duration
	proxyClientIP                      *bool
}

// bootstrapCommand runs the bounded bootstrap lifecycle using private file inputs.
func bootstrapCommand(args []string, log io.Writer) int {
	f := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dir := f.String("input-dir", "/etc/private-mail", "mounted private inputs")
	var proxy proxyOption
	f.Var(&proxy, "proxy-client-ip", "explicit deployment transport override")
	if f.Parse(args) != nil || f.NArg() != 0 || !filepath.IsAbs(*dir) {
		_, _ = fmt.Fprintln(log, "Email bootstrap refused invalid arguments.")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 3*time.Minute)
	defer timeout()
	applicationDigest, err := pinnedApplicationDigest()
	if err != nil {
		_, _ = fmt.Fprintln(log, "Email bootstrap refused invalid administration asset metadata.")
		return 2
	}
	runtime := bootstrapRuntime{
		server: "/usr/local/bin/stalwart", cli: "/usr/local/bin/stalwart-cli", url: "http://127.0.0.1:" + bootstrapRecoveryPort,
		dataDir: "/var/lib/stalwart", serverArgs: []string{"--config", "/etc/stalwart/config.json"},
		startup: time.Minute, shutdown: 30 * time.Second,
		applicationFile: applicationAssetPath(applicationDigest), applicationDigest: applicationDigest,
		proxyClientIP: proxy.value,
	}
	if err := bootstrap(ctx, *dir, runtime); err != nil {
		_, _ = fmt.Fprintln(log, "Email bootstrap failed; serving must remain stopped.", err)
		return 2
	}
	if _, err := fmt.Fprintln(log, "Email configuration verified; recovery process stopped."); err != nil {
		return 2
	}
	return 0
}

type boundedOutput struct{ bytes.Buffer }

// Write retains only the configured amount of bootstrap diagnostics.
func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1048576 {
		return 0, errConfiguration
	}
	return b.Buffer.Write(p)
}

// The initializer owns this child, waits for it to flush, and never launches a
// serving process. Kubernetes may start the separate server only on exit zero.
func bootstrap(ctx context.Context, inputDir string, runtime bootstrapRuntime) (result error) {
	v, err := loadInputs(inputDir)
	if err != nil {
		return errConfiguration
	}
	if runtime.proxyClientIP != nil {
		v.UseProxyClientIP = *runtime.proxyClientIP
	}
	if verifyApplicationAsset(runtime.applicationFile, runtime.applicationDigest) != nil {
		return errConfiguration
	}
	lock, err := os.OpenFile(filepath.Join(runtime.dataDir, ".private-mail-bootstrap.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errConfiguration
	}
	defer func() {
		if lock.Close() != nil {
			result = errConfiguration
		}
	}()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errConfiguration
	}
	defer func() {
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) != nil {
			result = errConfiguration
		}
	}()
	work, err := os.MkdirTemp("", "private-mail-bootstrap-")
	if err != nil {
		return errConfiguration
	}
	defer func() {
		if os.RemoveAll(work) != nil {
			result = errConfiguration
		}
	}()
	planPath := filepath.Join(work, "plan.ndjson")
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	m := management{client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, url: runtime.url, password: v.administratorPassword}
	// Refuse an already active management service. Together with the exclusive
	// volume lock, child liveness and RocksDB locking this fences accidental reuse.
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, runtime.url+"/healthz/ready", nil)
	if err != nil {
		return errConfiguration
	}
	if response, probeErr := m.client.Do(probe); probeErr == nil {
		_ = response.Body.Close()
		return errConfiguration
	}
	child := exec.Command(runtime.server, runtime.serverArgs...)
	child.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + work, "TMPDIR=" + work,
		"STALWART_RECOVERY_MODE=1", "STALWART_RECOVERY_MODE_PORT=" + bootstrapRecoveryPort,
		"STALWART_RECOVERY_ADMIN=bootstrap:" + v.administratorPassword}
	// Native diagnostics may include credentials or private configuration.
	child.Stdout, child.Stderr = io.Discard, io.Discard
	if child.Start() != nil {
		return errConfiguration
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			if stopOwned(child, done, runtime.shutdown) != nil {
				result = errConfiguration
			}
		}
	}()
	ready, cancel := context.WithTimeout(ctx, runtime.startup)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			stopped = true
			return errConfiguration
		case <-ready.Done():
			return errConfiguration
		case <-ticker.C:
			if _, err = m.request(ready, "/healthz/ready", nil); err == nil {
				goto prepared
			}
		}
	}
prepared:
	select {
	case <-done:
		stopped = true
		return errConfiguration
	default:
	}
	inventory, err := m.request(ctx, "/jmap", inventoryRequest)
	if err != nil || validateInventory(inventory) != nil || validateRecipientOwnership(inventory, v) != nil {
		return fmt.Errorf("initial inventory: %w", errConfiguration)
	}
	v.accountIDs, err = managedAccountIDs(inventory, v)
	if err != nil {
		return fmt.Errorf("initial inventory: %w", errConfiguration)
	}
	plan, err := render(v)
	if err != nil || os.WriteFile(planPath, plan, 0600) != nil {
		return errConfiguration
	}
	applyCtx, applyCancel := context.WithTimeout(ctx, time.Minute)
	defer applyCancel()
	cli := exec.CommandContext(applyCtx, runtime.cli, "apply", "--file", planPath, "--json")
	cli.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + work, "TMPDIR=" + work,
		"STALWART_URL=" + runtime.url, "STALWART_USER=bootstrap", "STALWART_PASSWORD=" + v.administratorPassword}
	var output boundedOutput
	cli.Stdout, cli.Stderr, cli.WaitDelay = &output, io.Discard, 5*time.Second
	if cli.Run() != nil || validateApply(plan, output.Bytes()) != nil {
		return fmt.Errorf("native apply: %w", errConfiguration)
	}
	if m.verify(ctx, plan) != nil {
		return fmt.Errorf("policy readback: %w", errConfiguration)
	}
	// Recovery intentionally authenticates only the recovery administrator.
	// Normal account authentication is proved after this process has stopped.
	inventory, err = m.request(ctx, "/jmap", inventoryRequest)
	if err != nil || validateInventory(inventory) != nil || validateRecipientOwnership(inventory, v) != nil {
		return errConfiguration
	}
	// A successful configuration cannot mask an early child exit or a forced
	// kill. A clean, bounded shutdown is the final promotion barrier.
	if stopOwned(child, done, runtime.shutdown) != nil {
		stopped = true
		return errConfiguration
	}
	stopped = true
	return nil
}

// stopOwned terminates the bootstrap child within the cleanup grace period.
func stopOwned(child *exec.Cmd, done <-chan error, grace time.Duration) error {
	select {
	case <-done:
		return errConfiguration
	default:
	}
	if child.Process.Signal(syscall.SIGTERM) != nil {
		_ = child.Process.Kill()
		<-done
		return errConfiguration
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return errConfiguration
		}
		return nil
	case <-timer.C:
		_ = child.Process.Kill()
		<-done
		return errConfiguration
	}
}
