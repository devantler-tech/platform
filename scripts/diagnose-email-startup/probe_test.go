package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// probeFixture executes the embedded Bash against isolated synthetic paths.
// Native Linux callers retain real stat/proc lock observation and hold their
// own temporary file lock; no production path or Kubernetes client is used.
func probeFixture(t *testing.T, native func(string) func(), health int, missingKey bool) measurement {
	t.Helper()
	root := filepath.Join(t.TempDir(), "fixture with spaces")
	if e := os.MkdirAll(root, 0700); e != nil {
		t.Fatal(e)
	}
	write := func(name, body string) string {
		p := filepath.Join(root, name)
		if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		return p
	}
	config := write("startup-config", publicConfig)
	certificate := write("tls.crt", "synthetic public certificate")
	key := write("tls.key", "synthetic disposable key placeholder")
	if missingKey {
		if e := os.Remove(key); e != nil {
			t.Fatal(e)
		}
	}
	lock := write("data/LOCK", "")
	comm := write("comm", "stalwart\n")
	locks := write("locks", "1: POSIX ADVISORY WRITE 1 08:01:42 0 EOF\n")
	tcp := write("tcp", "  sl local_address rem_address st\n  0: 00000000:1F90 00000000:0000 0A\n")
	tcp6 := write("tcp6", "  sl local_address rem_address st\n")
	temp := filepath.Join(root, "temporary")
	if e := os.Mkdir(temp, 0700); e != nil {
		t.Fatal(e)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	pairs := []string{
		"/var/lib/stalwart/data/LOCK", quote(lock), "/var/lib/stalwart/data", quote(filepath.Dir(lock)),
		"/etc/stalwart/config.json", quote(config), "/etc/stalwart/tls/tls.crt", quote(certificate), "/etc/stalwart/tls/tls.key", quote(key),
		"/proc/1/comm", quote(comm), "/proc/net/tcp6", quote(tcp6), "/proc/net/tcp", quote(tcp), "/tmp", quote(temp),
	}
	source := podProbe
	if native == nil {
		pairs = append(pairs, "/proc/locks", quote(locks))
	} else {
		cleanup := native(lock)
		defer cleanup()
		old := `"$task_owner" == 1`
		if strings.Count(source, old) != 1 {
			t.Fatal("native owner binding no longer has one fixed site")
		}
		source = strings.Replace(source, old, fmt.Sprintf(`"$task_owner" == %d`, os.Getpid()), 1)
	}
	source = strings.NewReplacer(pairs...).Replace(source)
	bin := filepath.Join(root, "bin")
	if e := os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	command := func(name, body string) {
		if e := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\nset -euo pipefail\n"+body), 0700); e != nil {
			t.Fatal(e)
		}
	}
	command("id", "[[ \"$*\" == '-u' ]]\nprintf '2000\\n'\n")
	command("curl", "[[ \"${@: -1}\" == 'http://127.0.0.1:8080/healthz/ready' ]]\nprintf '"+fmt.Sprint(health)+"'\n")
	if native == nil {
		command("stat", "[[ \"$1\" == '-Lc' && \"$2\" == '%d %i' && \"$3\" == "+quote(lock)+" ]]\nprintf '2049 42\\n'\n")
	}
	if runtime.GOOS == "darwin" {
		command("sha256sum", "exec /usr/bin/shasum -a 256 \"$@\"\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5e9)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-euo", "pipefail", "-c", source)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LC_ALL=C"}
	raw, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("isolated probe failed: %v, %s", e, raw)
	}
	m, e := decodeMeasurement(raw)
	if e != nil {
		t.Fatalf("isolated probe returned incomplete schema: %v", e)
	}
	if m.ConfigSHA256 != hashBytes([]byte(publicConfig)) {
		t.Fatal("actual file digest differs")
	}
	return m
}

// TestEmbeddedProbeExercisesRealShell verifies the exact reader and phase data.
func TestEmbeddedProbeExercisesRealShell(t *testing.T) {
	for _, tc := range []struct {
		name       string
		health     int
		missingKey bool
		want       string
	}{
		{"healthy", 200, false, "OBSERVED_HEALTHY_SAMPLE"},
		{"unreadable required input", 200, true, "REQUIRED_RUNTIME_INPUT_UNAVAILABLE"},
		{"health failure", 503, false, "LOCAL_HEALTH_NOT_READY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := probeFixture(t, nil, tc.health, tc.missingKey)
			if got := classify(m, true, true); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
