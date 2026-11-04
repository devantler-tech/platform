// Command zone runs a single World at Ruin zone simulation.
//
// It is the authoritative realtime tier taking shape: one fixed-timestep tick
// loop, one process, one zone. With -replicate it runs the full replication
// pipeline in-process — tracker delta → wire-encode → decode → verify — and
// reports the payload sizes. With -listen it serves the real zone socket per
// the transport ADR (docs/design/zone-transport.md): a WebSocket-over-TLS
// endpoint at /zone carrying one wire-codec message per binary frame, with
// token-gated admission. With -agones (default off, -listen only) the socket
// server additionally speaks the Agones GameServer lifecycle through the
// official SDK, which is what makes the binary deployable on the fleet: Ready
// once a player could actually connect, Health on a cadence, Shutdown on
// exit. Nakama identity and handoff policy live in their own inert packages;
// the concrete allocation adapter and RPC registration remain later children.
// The socket is opt-in: without -listen the command behaves exactly as before.
//
//	zone                     # 600 deterministic ticks, then print the state hash
//	zone -ticks 1800         # a different fixed count
//	zone -realtime -duration 3s   # drive the fixed loop from real time for 3s
//	zone -replicate 1        # also track observer 1, wire-encode its delta stream
//	zone -allocation-id gs-123 -listen :8443 -tls-cert cert.pem -tls-key key.pem  # serve the zone socket (wss)
//	zone -allocation-id local-zone -listen :8443 -tls-cert cert.pem -tls-key key.pem -agones
//	zone -listen :8443 -tls-cert cert.pem -tls-key key.pem -agones -agones-admission-public-key public.pem
//	zone -allocation-id gs-123 -mint-token 1  # developer helper: mint an admission token, print it, exit
//
// The admission-token secret is read from the environment variable named by
// -admission-secret-env for local socket exercises (hex-encoded, at least 32
// bytes decoded); the flag carries the variable's NAME, never the secret
// itself. Sealed Agones admission generates a per-GameServer secret in memory
// and does not read that environment variable.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	"github.com/devantler-tech/world-at-ruin/server/agones"
	"github.com/devantler-tech/world-at-ruin/server/sim"
	"github.com/devantler-tech/world-at-ruin/server/wire"
	"github.com/devantler-tech/world-at-ruin/server/zoneclaim"
	"github.com/devantler-tech/world-at-ruin/server/zonesock"
)

func main() {
	ticks := flag.Int("ticks", 600, "run this many fixed ticks deterministically, then print the state hash")
	realtime := flag.Bool("realtime", false, "drive the fixed loop from the wall clock instead of a fixed count")
	duration := flag.Duration("duration", 2*time.Second, "run length in -realtime mode (and, when set explicitly, -listen mode)")
	replicate := flag.Uint64("replicate", 0, "observer entity ID: track its replication deltas, wire-encode and decode-verify each, and print payload stats (fixed-tick mode)")
	interest := flag.Int64("interest", 12_000, "observer interest radius in mm for -replicate and -listen")
	listen := flag.String("listen", "", "serve the zone socket (WebSocket at /zone) on this address; empty (the default) keeps the socket off")
	tlsCert := flag.String("tls-cert", "", "PEM certificate chain file for -listen (identity is a DNS name per the transport ADR)")
	tlsKey := flag.String("tls-key", "", "PEM private key file for -listen")
	insecurePlaintext := flag.Bool("insecure-plaintext", false, "LOCAL DEVELOPMENT ONLY: serve -listen without TLS (ws://)")
	secretEnv := flag.String("admission-secret-env", "WAR_ZONE_ADMISSION_SECRET", "NAME of the environment variable holding the hex-encoded admission-token secret")
	allocation := flag.String("allocation-id", "", "allocation/GameServer ID this process admits (required with -listen or -mint-token)")
	mintObserver := flag.Uint64("mint-token", 0, "developer helper: mint an admission token for this observer entity ID using the admission secret, print it, and exit")
	mintTTL := flag.Duration("mint-ttl", 5*time.Minute, "expiry window for -mint-token")
	withAgones := flag.Bool("agones", false, "register with the local Agones SDK sidecar (Ready/Health/Shutdown); requires -listen, since Ready must mean a connectable endpoint")
	healthInterval := flag.Duration("agones-health-interval", agones.DefaultHealthInterval, "heartbeat cadence for -agones; keep it under half the fleet's health periodSeconds")
	admissionPublicKey := flag.String("agones-admission-public-key", "", "PEM RSA public-key file for sealed per-GameServer admission; requires -agones (empty keeps the local environment-secret path)")
	var claims claimOptions
	var movement movementOptions
	flag.BoolVar(&movement.enabled, "movement-intents", false, "accept authenticated v3 movement intents (experimental; default off)")
	flag.Uint64Var(&movement.holdTicks, "movement-hold-ticks", 3, "maximum fixed ticks to hold movement input (1..300; experimental)")
	flag.BoolVar(&claims.enabled, "private-claims", false, "require a private durable claim before socket admission (experimental; default off)")
	flag.StringVar(&claims.endpoint, "claim-url", "", "private HTTPS claim endpoint ending in /v1/claim")
	flag.StringVar(&claims.caFile, "claim-ca", "", "PEM trust roots for the private claim service")
	flag.StringVar(&claims.certFile, "claim-cert", "", "PEM workload client certificate")
	flag.StringVar(&claims.keyFile, "claim-key", "", "PEM workload client private key")
	flag.Parse()
	if err := movement.validate(*listen != "", *mintObserver != 0); err != nil {
		fatalf("%v", err)
	}
	if err := claims.validate(*listen != "", *withAgones, *admissionPublicKey != "", *insecurePlaintext, *mintObserver != 0); err != nil {
		fatalf("%v", err)
	}

	durationSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "duration" {
			durationSet = true
		}
	})

	if *mintObserver != 0 {
		runMint(*secretEnv, *allocation, sim.EntityID(*mintObserver), *mintTTL)
		return
	}
	if *listen != "" && *replicate != 0 {
		fatalf("-listen and -replicate are mutually exclusive")
	}
	if *withAgones && *listen == "" {
		fatalf("-agones requires -listen: Agones Ready must mean a player can actually connect, and only -listen opens the zone socket")
	}
	if *admissionPublicKey != "" && !*withAgones {
		fatalf("-agones-admission-public-key requires -agones")
	}

	w := sim.NewDemoWorld()
	switch {
	case *listen != "":
		d := time.Duration(0) // run until signalled
		if durationSet {
			d = *duration
		}
		if err := runListen(w, *listen, *tlsCert, *tlsKey, *secretEnv, *allocation, *insecurePlaintext, *interest, d, *withAgones, *healthInterval, *admissionPublicKey, claims, movement); err != nil {
			fatalf("%v", err)
		}
	case *realtime:
		runRealtime(w, *duration)
	case *replicate != 0:
		runReplicate(w, *ticks, sim.EntityID(*replicate), *interest)
	default:
		for range *ticks {
			sim.DriveDemoTick(w)
			w.Step()
		}
	}
	fmt.Printf("zone: entities=%d tick=%d hash=%016x\n", w.Count(), w.Tick, w.Hash())
}

// admissionSecret reads and decodes the admission secret from the environment
// variable envName. Fail-closed: a missing or undersized secret is fatal, and
// the error names the variable, never its content.
func admissionSecret(envName string) []byte {
	raw := os.Getenv(envName)
	if raw == "" {
		fatalf("admission secret: environment variable %s is unset or empty (hex-encoded, >=32 bytes decoded)", envName)
	}
	secret, err := hex.DecodeString(raw)
	if err != nil {
		fatalf("admission secret: %s is not valid hex", envName)
	}
	return secret
}

// runMint is the developer-side helper for exercising -listen: it mints an
// admission token for the given observer. Real minting belongs to
// handoff.Service with a secret unique to the allocated zone — this remains for
// exercising the socket without the later concrete Agones allocator and Nakama
// RPC adapters.
func runMint(secretEnv, allocation string, observer sim.EntityID, ttl time.Duration) {
	token, err := zonesock.MintToken(admissionSecret(secretEnv), allocation, observer, time.Now().Add(ttl))
	if err != nil {
		fatalf("mint token: %v", err)
	}
	fmt.Println(token)
}

// runListen serves the zone socket while driving the fixed loop from the wall
// clock, per the transport ADR: TLS is the default and plaintext exists only
// behind the explicit local-development opt-in. d <= 0 runs until the process
// is signalled. It returns errors instead of exiting so every exit path runs
// the deferred cleanup — with -agones that includes telling the sidecar to
// recycle the GameServer, which os.Exit would silently skip.
func runListen(w *sim.World, addr, certFile, keyFile, secretEnv, allocation string, insecurePlaintext bool, interestMM int64, d time.Duration, withAgones bool, healthInterval time.Duration, admissionPublicKeyFile string, claims claimOptions, movementConfig ...movementOptions) (result error) {
	var movement movementOptions
	if len(movementConfig) > 0 {
		movement = movementConfig[0]
	}
	if err := movement.validate(true, false); err != nil {
		return err
	}
	if err := claims.validate(true, withAgones, admissionPublicKeyFile != "", insecurePlaintext, false); err != nil {
		return err
	}
	if insecurePlaintext && (certFile != "" || keyFile != "") {
		return fmt.Errorf("-insecure-plaintext contradicts -tls-cert/-tls-key: choose one")
	}
	if !insecurePlaintext && (certFile == "" || keyFile == "") {
		return fmt.Errorf("-listen requires -tls-cert and -tls-key (or the explicit -insecure-plaintext local-development opt-in)")
	}
	privateClient, err := claims.client()
	if err != nil {
		return err
	}
	if privateClient != nil {
		// Registered before the hub drain, so LIFO cleanup retires idle private
		// connections only after all admitted handlers have been canceled.
		defer privateClient.Close()
	}

	boundAllocation := allocation
	var secret []byte
	var hub *zonesock.Hub
	if admissionPublicKeyFile == "" {
		// Keep the existing local path's validation order unchanged: secret
		// and verifier errors still precede certificate parsing when sealed
		// admission is not selected.
		secret = admissionSecret(secretEnv)
		verifier, err := zonesock.NewHMACVerifier(secret, boundAllocation)
		if err != nil {
			return err
		}
		hub, err = zonesock.NewHub(zonesock.Config{Verifier: verifier, InterestMM: interestMM, MovementEnabled: movement.enabled, MovementHoldTicks: movement.holdTicks})
		if err != nil {
			return err
		}
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, SessionTicketsDisabled: true}
	if !insecurePlaintext {
		// Preserve the readiness gate, then re-read projected material for
		// every new handshake. Never cache a stale pair or fall back when a
		// rotation is incomplete. Session resumption must not bypass this gate.
		if _, err := currentTLSCertificate(certFile, keyFile); err != nil {
			return err
		}
		tlsConfig.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return currentTLSCertificate(certFile, keyFile)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var prepared *agones.PreparedAdmission
	preparedActivated := false
	if admissionPublicKeyFile != "" {
		cleanPublicKeyPath := filepath.Clean(admissionPublicKeyFile)
		publicKeyPEM, err := fs.ReadFile(
			os.DirFS(filepath.Dir(cleanPublicKeyPath)),
			filepath.Base(cleanPublicKeyPath),
		)
		if err != nil {
			return fmt.Errorf("read Agones admission wrapping public key: %w", err)
		}
		prepared, err = agones.PrepareAdmission(
			ctx,
			agones.Config{HealthInterval: healthInterval},
			agones.AdmissionConfig{WrappingPublicKeyPEM: publicKeyPEM},
		)
		if err != nil {
			return err
		}
		defer func() {
			if preparedActivated {
				return
			}
			if err := prepared.Shutdown(); err != nil {
				fmt.Fprintf(os.Stderr, "zone: %v\n", err)
			}
		}()
		if allocation != "" && allocation != prepared.AllocationID() {
			return fmt.Errorf(
				"-allocation-id %q does not match observed GameServer %q",
				allocation,
				prepared.AllocationID(),
			)
		}
		boundAllocation = prepared.AllocationID()
		secret = prepared.Secret()
		verifier, err := zonesock.NewHMACVerifier(secret, boundAllocation)
		if err != nil {
			return err
		}
		cfg := zonesock.Config{Verifier: verifier, InterestMM: interestMM, MovementEnabled: movement.enabled, MovementHoldTicks: movement.holdTicks}
		if privateClient != nil {
			gate, gateErr := zoneclaim.New(prepared, privateClient)
			if gateErr != nil {
				return gateErr
			}
			hub, err = zonesock.NewClaimedHub(cfg, gate, 5*time.Second)
		} else {
			hub, err = zonesock.NewHub(cfg)
		}
		if err != nil {
			return err
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/zone", hub.Handler())
	srv := &http.Server{
		Handler:           mux,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	scheme := "wss"
	if insecurePlaintext {
		scheme = "ws"
	}
	fmt.Printf("zone: listening on %s://%s/zone\n", scheme, ln.Addr())

	serveCtx, serveFailed := context.WithCancelCause(ctx)
	defer serveFailed(nil)
	var lc *agones.Lifecycle
	go func() {
		var err error
		if insecurePlaintext {
			err = srv.Serve(ln)
		} else {
			// GetCertificate owns validated certificate refresh; keep the
			// file arguments empty so ServeTLS cannot cache a startup pair.
			err = srv.ServeTLS(ln, "", "")
		}
		serveFailed(err)
	}()
	defer func() {
		// HTTP Close stops the listener and ordinary HTTP requests, but it
		// excludes hijacked WebSockets. The hub owns those transports and
		// their pending admission work. World access remains on this owner.
		result = errors.Join(result, srv.Close())
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hub.Shutdown(drainCtx, w); err != nil {
			result = errors.Join(result, fmt.Errorf("drain zone sockets: %w", err))
		}
		if lc != nil {
			result = errors.Join(result, lc.Shutdown())
		}
	}()

	// Agones Ready fires only now — the listener is bound and the serve
	// goroutine is up, so "ready to be allocated" is true in the one sense
	// that matters: a player the fleet routes here can actually connect.
	if withAgones {
		if prepared != nil {
			lc, err = prepared.Ready()
			preparedActivated = err == nil
		} else {
			lc, err = agones.Start(ctx, agones.Config{HealthInterval: healthInterval})
		}
		if err != nil {
			return err
		}
	}

	runLoop(serveCtx, d, func() {
		sim.DriveDemoTick(w)
		hub.BeforeStep(w)
		w.Step()
		hub.Tick(w)
	})
	// A canceled serveCtx has two possible authors, and only one is an
	// error: the serve goroutine (a real serve failure) or the parent
	// signal context (a clean SIGINT/SIGTERM — whose cause under Go 1.26+
	// NAMES the signal, so comparing against context.Canceled would misread
	// a clean shutdown as a failure and exit non-zero).
	if err := context.Cause(serveCtx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// currentTLSCertificate loads the current projected pair and refuses unusable
// material both before readiness and on later handshakes. Identity remains the
// client's responsibility, since the command knows file paths, not served names.
func currentTLSCertificate(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse TLS leaf certificate: %w", err)
	}
	if now := time.Now(); now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("TLS certificate is outside its validity window (NotBefore %s, NotAfter %s): no client would accept the handshake", leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	return &cert, nil
}

// runReplicate drives the fixed demo ticks while tracking one observer's
// replication stream through the wire codec: every non-empty per-tick delta is
// encoded, decoded back and verified, and the final full snapshot (the join
// payload) runs the same round trip. Interest configuration is not part of the
// hashed step state, so the closing hash line still prints the same golden
// hash as a plain run — replication observation cannot move the sim.
func runReplicate(w *sim.World, ticks int, observer sim.EntityID, interestMM int64) {
	w.SetInterestRadius(observer, interestMM)
	tr := sim.NewSnapshotTracker(observer)

	frames, totalBytes, maxFrame := 0, 0, 0
	for range ticks {
		sim.DriveDemoTick(w)
		w.Step()
		d := tr.Update(w)
		if d.Empty() {
			continue
		}
		b, err := wire.EncodeSnapshotDelta(d)
		if err != nil {
			fatalf("tick %d: encode delta: %v", w.Tick, err)
		}
		m, err := wire.Decode(b)
		if err != nil {
			fatalf("tick %d: decode delta: %v", w.Tick, err)
		}
		if !reflect.DeepEqual(m.Delta, d) {
			fatalf("tick %d: wire round trip diverged from the tracker's delta", w.Tick)
		}
		frames++
		totalBytes += len(b)
		if len(b) > maxFrame {
			maxFrame = len(b)
		}
	}

	join := w.Snapshot(observer)
	jb, err := wire.EncodeSnapshot(join)
	if err != nil {
		fatalf("encode join snapshot: %v", err)
	}
	jm, err := wire.Decode(jb)
	if err != nil {
		fatalf("decode join snapshot: %v", err)
	}
	if !reflect.DeepEqual(jm.Snapshot, join) {
		fatalf("join snapshot wire round trip diverged")
	}

	fmt.Printf("zone: replicated observer=%d interest=%dmm deltaFrames=%d deltaBytes=%d maxDelta=%dB joinSnapshot=%dB (all decode-verified)\n",
		observer, interestMM, frames, totalBytes, maxFrame, len(jb))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zone: "+format+"\n", args...)
	os.Exit(1)
}

// runRealtime drives the world through a FixedLoop off the monotonic clock
// until d elapses or the process is asked to stop. Timing jitter changes how
// many ticks run, but never what a given tick computes — that is the point of
// decoupling the sim rate from the wall clock. It deliberately has no Agones
// wiring: with no listener there is nothing a player could connect to, so
// declaring Ready here would hand the fleet a dead allocation (-agones is
// therefore -listen-only).
func runRealtime(w *sim.World, d time.Duration) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runLoop(ctx, d, func() {
		sim.DriveDemoTick(w)
		w.Step()
	})
}

// runLoop drives the fixed loop off the wall clock, running tick once per
// fixed step, until ctx ends or d elapses (d <= 0 means no deadline).
func runLoop(ctx context.Context, d time.Duration, tick func()) {
	loop := sim.NewFixedLoop()

	// Poll faster than the sim rate so the accumulator sees fine-grained
	// deltas rather than one lumpy step per fixed interval.
	poll := time.NewTicker(time.Duration(loop.StepNanos() / 2))
	defer poll.Stop()
	var deadline <-chan time.Time // nil without a duration: never fires in the select
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		deadline = t.C
	}

	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case now := <-poll.C:
			elapsed := now.Sub(last).Nanoseconds()
			last = now
			loop.Advance(elapsed, tick)
		}
	}
}
