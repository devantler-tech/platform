package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This fixture runs only a disposable local PostgreSQL server. It has no TCP
// listener, inherited connection environment, production data or Kubernetes client.
// A native query result proves SQL behavior, never a production storage binding.
func nativeObservationPostgres(t *testing.T, connectionOptions ...*string) (func(string) []byte, string, context.CancelFunc) {
	t.Helper()
	if len(connectionOptions) > 1 || len(connectionOptions) == 1 && connectionOptions[0] == nil {
		t.Fatal("invalid disposable connection-options fixture")
	}
	bin := os.Getenv("LONGHORN_POSTGRES_NATIVE_BIN")
	if bin == "" {
		t.Skip("set LONGHORN_POSTGRES_NATIVE_BIN to the PostgreSQL 18.6 bin directory")
	}
	// testing cancels t.Context before cleanup callbacks. Keep this independently
	// bounded context alive until the owned server has shut down gracefully.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	command := func(program string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, filepath.Join(bin, program), args...)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LC_ALL=C", "TZ=UTC"}
		if program == "psql" && len(connectionOptions) == 1 && *connectionOptions[0] != "" {
			cmd.Env = append(cmd.Env, "PGOPTIONS="+*connectionOptions[0])
		}
		out, err := cmd.CombinedOutput()
		if err != nil || len(out) > 1<<20 {
			t.Fatalf("disposable PostgreSQL command %s failed: %v", program, err)
		}
		return out
	}
	for _, program := range []string{"initdb", "postgres", "psql"} {
		version := string(command(program, "--version"))
		if !strings.Contains(version, "(PostgreSQL) 18.6") {
			t.Fatalf("native SQL fixture requires PostgreSQL 18.6: %s", program)
		}
	}
	root, err := os.MkdirTemp("/tmp", "lh-pg-sql-")
	if err != nil {
		t.Fatal(err)
	}
	removable := true
	t.Cleanup(func() {
		if removable {
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("disposable PostgreSQL directory cleanup failed: %v", err)
			}
		} else {
			t.Errorf("disposable PostgreSQL cleanup remains unverified; retained %s", root)
		}
	})
	data := filepath.Join(root, "data")
	command("initdb", "--pgdata="+data, "--username=postgres", "--auth-local=trust", "--auth-host=reject", "--no-sync")
	// macOS limits Unix socket paths more tightly than the test-name temp path.
	socket := filepath.Join(root, "socket")
	if err = os.Mkdir(socket, 0700); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(root, "postgres.log"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	server := exec.CommandContext(ctx, filepath.Join(bin, "postgres"), "-D", data, "-k", socket, "-p", "15432", "-h", "", "-c", "unix_socket_permissions=0700", "-c", "fsync=on")
	// PostgreSQL's fast shutdown asks the supervisor to terminate and join its
	// children. CommandContext's default SIGKILL must never bypass that path.
	server.Cancel = func() error { return server.Process.Signal(syscall.SIGINT) }
	server.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LC_ALL=C", "TZ=UTC"}
	server.Stdout, server.Stderr = log, log
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	removable = false
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = server.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = server.Process.Signal(syscall.SIGINT)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			// Immediate shutdown is still handled by PostgreSQL, including its
			// children. An unresponsive server retains its files for recovery.
			_ = server.Process.Signal(syscall.SIGQUIT)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				t.Error("disposable PostgreSQL exceeded its shutdown deadline")
				return
			}
		}
		if waitErr != nil && !errors.Is(waitErr, context.Canceled) && !errors.Is(waitErr, context.DeadlineExceeded) {
			t.Errorf("disposable PostgreSQL did not exit cleanly: %v", waitErr)
			return
		}
		shutdown, err := os.ReadFile(log.Name())
		_, pidErr := os.Lstat(filepath.Join(data, "postmaster.pid"))
		if err != nil || !bytes.Contains(shutdown, []byte("database system is shut down")) || !errors.Is(pidErr, os.ErrNotExist) {
			t.Error("disposable PostgreSQL has not proved native shutdown and PID-file absence")
			return
		}
		removable = true
	})
	connection := []string{"--host=" + socket, "--port=15432", "--username=postgres", "--dbname=postgres", "--no-psqlrc", "--no-password", "--set=ON_ERROR_STOP=1", "--tuples-only", "--no-align", "--quiet"}
	ready := time.Now().Add(10 * time.Second)
	for {
		probe := exec.CommandContext(ctx, filepath.Join(bin, "psql"), append(connection, "--command=SELECT 1")...)
		probe.Env = server.Env
		if out, e := probe.CombinedOutput(); e == nil && string(out) == "1\n" {
			break
		}
		if ctx.Err() != nil || time.Now().After(ready) {
			t.Fatal("disposable PostgreSQL did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func(query string) []byte {
		t.Helper()
		return command("psql", append(connection, "--command="+query)...)
	}, data, cancel
}

func TestObservationSQLNativeIgnoresPersistedLookupAndWriteDefaults(t *testing.T) {
	options := ""
	query, _, _ := nativeObservationPostgres(t, &options)
	systemID := strings.TrimSpace(string(query(`SELECT (pg_catalog.pg_control_system()).system_identifier::text;`)))
	query(`CREATE FUNCTION public.pg_is_in_recovery() RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true';`)
	query(`CREATE TYPE public.forged_control AS (system_identifier bigint); CREATE FUNCTION public.pg_control_system() RETURNS public.forged_control LANGUAGE sql IMMUTABLE AS 'SELECT ROW(12345)::public.forged_control';`)
	query(`CREATE FUNCTION public.pg_last_wal_replay_lsn() RETURNS pg_catalog.pg_lsn LANGUAGE sql IMMUTABLE AS 'SELECT ''2/100''::pg_catalog.pg_lsn';`)
	query(`ALTER ROLE postgres SET search_path TO public,pg_catalog; ALTER DATABASE postgres SET search_path TO public,pg_catalog; ALTER ROLE postgres SET default_transaction_read_only TO off;`)
	if string(query(`SELECT pg_is_in_recovery(),(pg_control_system()).system_identifier,pg_last_wal_replay_lsn();`)) != "t|12345|2/100\n" {
		t.Fatal("adverse fixture did not shadow the unqualified physical-state functions")
	}
	observation := cnpgObservationFixture(t)
	cluster, pod := observation.Cluster, observation.Pods[0]
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program != "kubectl" || len(input) != 0 || len(args) < 3 {
			t.Fatal("native SQL fixture selected an unexpected command")
		}
		if args[2] == "get" {
			return json.Marshal(pod)
		}
		if args[2] != "exec" || args[len(args)-1] != "--command="+postgresStateSQL {
			t.Fatal("native SQL fixture attempted a command outside the fixed observation")
		}
		options = ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "PGOPTIONS=") {
				options = strings.TrimPrefix(arg, "PGOPTIONS=")
			}
		}
		return query(strings.TrimPrefix(args[len(args)-1], "--command=")), nil
	}}
	got, err := c.observePostgres(t.Context(), cluster, pod)
	if err != nil || got.InRecovery || got.SystemID != systemID {
		t.Error("physical observation followed a shadow function instead of the actual database")
	}
	if string(query(`SELECT pg_catalog.current_setting('search_path'),pg_catalog.current_setting('transaction_read_only');`)) != "pg_catalog|on\n" {
		t.Error("observation did not override persisted lookup and write defaults")
	}
}

func TestObservationSQLNativeCancellationStopsTheOwnedServerCleanly(t *testing.T) {
	_, _, cancel := nativeObservationPostgres(t)
	cancel()
}
