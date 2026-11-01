package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Catch any client change that sends credentials/message commands, leaks the
// provider banner, uses a private hostname, or reports a refused NOOP as success.
func TestSMTPClientDoesNotSendMail(t *testing.T) {
	if _, err := os.ReadFile("probe-client.sh"); err != nil {
		t.Fatalf("the SMTP probe client is missing: %v", err)
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal("native curl is required for this client test")
	}
	for _, tc := range []struct {
		name, reply, want string
		exit              int
	}{
		{"accepted", "250 probe accepted", "SMTP_PROVIDER_SESSION=VERIFIED\n", 0},
		{"refused", "550 probe refused", "SMTP_PROVIDER_SESSION=UNVERIFIED curl_exit=8 response=550\n", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			commands := make(chan []string, 1)
			serverErr := make(chan error, 1)
			go func() {
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					serverErr <- acceptErr
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				_, _ = fmt.Fprint(conn, "220 PRIVATE_FIXTURE_PROVIDER_BANNER ESMTP\r\n")
				reader := bufio.NewReader(conn)
				var seen []string
				for {
					line, readErr := reader.ReadString('\n')
					if readErr != nil {
						serverErr <- readErr
						return
					}
					line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
					seen = append(seen, line)
					switch line {
					case "EHLO email-transport-probe.invalid":
						_, _ = fmt.Fprint(conn, "250-fixture.invalid\r\n250 AUTH PLAIN\r\n")
					case "NOOP":
						_, _ = fmt.Fprintf(conn, "%s\r\n", tc.reply)
					case "QUIT":
						_, _ = fmt.Fprint(conn, "221 closing\r\n")
						commands <- seen
						return
					default:
						_, _ = fmt.Fprint(conn, "550 forbidden command\r\n")
					}
				}
			}()
			dir := t.TempDir()
			// A real curlrc would cause authentication if --disable were lost.
			if err = os.WriteFile(filepath.Join(dir, ".curlrc"), []byte("user = fixture:PRIVATE_FIXTURE_PASSWORD\n"), 0600); err != nil {
				t.Fatal(err)
			}
			wrapper := `#!/bin/sh
set -eu
found=false
for argument do
  case "$argument" in
    smtp://mx01.mail.icloud.com:25/email-transport-probe.invalid) found=true ;;
    *://*) exit 91 ;;
  esac
done
"$found"
exec "$NATIVE_CURL_BINARY" "$@" --connect-to "mx01.mail.icloud.com:25:$SMTP_FIXTURE_ADDRESS"
`
			if err = os.WriteFile(filepath.Join(dir, "curl"), []byte(wrapper), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "probe-client.sh")
			for _, value := range os.Environ() {
				key := strings.ToLower(strings.SplitN(value, "=", 2)[0])
				if key == "path" || key == "home" || key == "curl_home" || key == "xdg_config_home" || strings.HasSuffix(key, "proxy") {
					continue
				}
				cmd.Env = append(cmd.Env, value)
			}
			cmd.Env = append(cmd.Env, "PATH="+dir+":/usr/bin:/bin", "HOME="+dir, "CURL_HOME="+dir, "XDG_CONFIG_HOME="+dir,
				"NATIVE_CURL_BINARY="+curl, "SMTP_FIXTURE_ADDRESS="+listener.Addr().String())
			output, runErr := cmd.CombinedOutput()
			gotExit := 0
			if runErr != nil {
				var ok bool
				var exitErr *exec.ExitError
				exitErr, ok = runErr.(*exec.ExitError)
				if !ok {
					t.Fatal(runErr)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tc.exit || string(output) != tc.want {
				t.Fatalf("client result: exit=%d output=%q; want exit=%d output=%q", gotExit, output, tc.exit, tc.want)
			}
			select {
			case seen := <-commands:
				want := []string{"EHLO email-transport-probe.invalid", "NOOP", "QUIT"}
				if !reflect.DeepEqual(seen, want) {
					t.Fatalf("unexpected SMTP transcript: %q", seen)
				}
			case err = <-serverErr:
				t.Fatal(err)
			case <-time.After(12 * time.Second):
				t.Fatal("SMTP fixture did not finish")
			}
		})
	}
}
