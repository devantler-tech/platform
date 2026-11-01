package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestNegativeResourceReachesTheActionPredicate guards against a vacuous denial.
func TestNegativeResourceReachesTheActionPredicate(t *testing.T) {
	if len(absent) <= 253 || len(absent) > 512 {
		t.Fatal("negative resource must be impossible in Kubernetes and bounded")
	}
	for _, path := range []string{"../../k8s/bases/infrastructure/controllers/auth-proxy/cilium-network-policy.yaml", "../../k8s/providers/hetzner/infrastructure/controllers/longhorn/cilium-network-policy.yaml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err = yaml.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		found := 0
		var inspect func(any)
		inspect = func(value any) {
			switch v := value.(type) {
			case map[string]any:
				if method, ok := v["method"].(string); ok && method == "^POST$" {
					expression, ok := v["path"].(string)
					if ok && strings.HasPrefix(expression, "^/v1/volumes/") {
						pattern := regexp.MustCompile(expression)
						prefix := "/v1/volumes/" + absent + "?action="
						if !pattern.MatchString(prefix+"snapshotList") || pattern.MatchString(prefix+"UNRECOGNIZED-READ-ONLY-PROBE&action=snapshotList") {
							t.Error("negative probe does not isolate the complete-query action restriction")
						}
						found++
					}
				}
				for _, child := range v {
					inspect(child)
				}
			case []any:
				for _, child := range v {
					inspect(child)
				}
			}
		}
		inspect(value)
		if found != 1 {
			t.Fatal("expected one volume action rule at each boundary")
		}
	}
}

// TestProofRequiresEveryPositiveAndNegativeResponse rejects each failed check.
func TestProofRequiresEveryPositiveAndNegativeResponse(t *testing.T) {
	for broken := -1; broken < 14; broken++ {
		t.Run(strconv.Itoa(broken+1), func(t *testing.T) {
			calls := 0
			execute := func(_ context.Context, args ...string) ([]byte, error) {
				if !socketProbeArguments(args) {
					t.Fatalf("unexpected command surface: %q", args)
				}
				code := []string{"200", "404", "200", "403", "403", "403", "200", "200", "200", "200", "200", "200", "200", "200"}[calls]
				if calls == broken {
					code = "500"
				}
				calls++
				if strings.Contains(args[len(args)-1], ".js?") {
					return []byte("HTTP/1.1 " + code + " Expected\r\nContent-Type: application/javascript\r\n\r\n"), nil
				}
				return []byte("HTTP/1.1 " + code + " Expected\r\n"), nil
			}
			err := prove(context.Background(), execute)
			if (err == nil) != (broken == -1) {
				t.Fatalf("broken response %d: %v", broken, err)
			}
			if calls != 14 {
				t.Fatalf("only %d probes ran", calls)
			}
			if broken >= 0 && !strings.Contains(err.Error(), "received HTTP 500") {
				t.Fatalf("missing safe status diagnostic: %v", err)
			}
		})
	}
}

// These fixed arguments keep the socket write side open while the request's
// builtin reader awaits the response. Piping printf into BusyBox nc sends a
// premature SHUT_WR and can make Traefik cancel a legitimate request with 499.
func socketProbeArguments(args []string) bool {
	return len(args) == 28 && strings.Join(args[:22], " ") == `--context=admin@prod --request-timeout=20s -n oauth2-proxy exec deployment/auth-proxy -c traefik -- /bin/sh -c exec 9>&1; exec "$@" longhorn-ui-probe /bin/busybox timeout -s KILL 15 /bin/busybox nc -w 10` &&
		strings.Join(args[24:27], " ") == "-e /bin/sh -c"
}

// Run the real request writer and bounded reader with socket stdin/stdout and
// captured FD9, as nc -e supplies them. Bash supports the same read -n/-t
// builtins used in the pinned BusyBox ash. This does not claim to execute the
// image's netcat/timeout; the protected production dispatch tests those tools.
func runSocketProbeFixture(ctx context.Context, args []string, address string, afterStart ...func(*net.TCPConn)) ([]byte, error) {
	if !socketProbeArguments(args) {
		return nil, errors.New("probe does not retain a response-reading socket process")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer func() { _ = connection.Close() }()
	socket, err := connection.(*net.TCPConn).File()
	if err != nil {
		return nil, err
	}
	defer func() { _ = socket.Close() }()
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	command := exec.CommandContext(ctx, "/bin/bash", "-c", args[27])
	command.Stdin, command.Stdout, command.Stderr = socket, socket, io.Discard
	command.ExtraFiles = []*os.File{nil, nil, nil, nil, nil, nil, writer}
	command.WaitDelay = time.Second
	if err = command.Start(); err != nil {
		return nil, err
	}
	_ = writer.Close()
	for _, control := range afterStart {
		control(connection.(*net.TCPConn))
	}
	out, readErr := io.ReadAll(io.LimitReader(reader, 65537))
	err = command.Wait()
	if readErr != nil || len(out) > 65536 {
		return nil, errors.New("fixture capture did not complete")
	}
	return out, err
}

func TestProbeRetainsItsWriteSideUntilTheResponse(t *testing.T) {
	for _, halfClose := range []bool{false, true} {
		t.Run(strconv.FormatBool(halfClose), func(t *testing.T) {
			requestReceived := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				connection, response, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = connection.Close() }()
				close(requestReceived)
				// Characterize the observed cancellation race: a server waiting
				// before its response sees EOF from nc's old printf pipeline.
				if err = connection.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
					t.Error(err)
					return
				}
				_, readErr := response.ReadByte()
				code := "499 Client Closed Request"
				var timeout net.Error
				if errors.As(readErr, &timeout) && timeout.Timeout() {
					code = "200 OK"
				}
				_, _ = response.WriteString("HTTP/1.1 " + code + "\r\nContent-Length: 0\r\n\r\n")
				_ = response.Flush()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			calls := 0
			err := prove(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
				calls++
				if calls != 1 {
					code := []string{"404", "200", "403", "403", "403", "200", "200", "200", "200", "200", "200", "200", "200"}[calls-2]
					response := "HTTP/1.1 " + code + " Expected\r\n"
					if strings.Contains(args[len(args)-1], ".js?") {
						response += "Content-Type: application/javascript\r\n\r\n"
					}
					return []byte(response), nil
				}
				var controls []func(*net.TCPConn)
				if halfClose {
					controls = append(controls, func(c *net.TCPConn) {
						select {
						case <-requestReceived:
							_ = c.CloseWrite()
						case <-ctx.Done():
						}
					})
				}
				return runSocketProbeFixture(ctx, args, strings.TrimPrefix(server.URL, "http://"), controls...)
			})
			if (err != nil) != halfClose || halfClose && !strings.Contains(err.Error(), "received HTTP 499") {
				t.Fatalf("socket write lifetime was not observed: half-close=%t err=%v", halfClose, err)
			}
		})
	}
}

// TestNativeProbeSendsOnlyFixedHarmlessRequests exercises the actual shell
// request writer and reader across a real TCP connection.
func TestNativeProbeSendsOnlyFixedHarmlessRequests(t *testing.T) {
	seen := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "longhorn.platform.devantler.tech" || r.ContentLength != 0 {
			t.Error("probe changed the host or sent a body")
		}
		if r.Method != "GET" && (r.Method != "POST" || !strings.Contains(r.URL.Path, absent) || r.URL.Query()["action"][0] != "UNRECOGNIZED-READ-ONLY-PROBE") {
			t.Error("probe could issue a real write")
		}
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "application/javascript")
		}
		w.Header().Set("X-Private-Fixture", "private header must be discarded")
		w.WriteHeader([]int{200, 404, 200, 403, 403, 403, 200, 200, 200, 200, 200, 200, 200, 200}[seen])
		seen++
		_, _ = w.Write([]byte("private response body must be discarded"))
	}))
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := prove(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		return runSocketProbeFixture(ctx, args, address)
	})
	if err != nil || seen != 14 {
		t.Fatalf("native request proof: seen=%d error=%v", seen, err)
	}
}

// TestProofRefusesTransportErrorsAndUnexpectedOutput guards private error handling.
func TestProofRefusesTransportErrorsAndUnexpectedOutput(t *testing.T) {
	for _, output := range []string{"", "HTTP/1.1 302 Redirect\r\n", "private value\nHTTP/1.1 200 OK\n", "HTTP/1.1 200 OK\nbody", "HTTP/1.1 2000 Invalid\n", "HTTP/1.1 000 Invalid\n", "HTTP/1.1 999 Invalid\n"} {
		err := prove(context.Background(), func(context.Context, ...string) ([]byte, error) { return []byte(output), nil })
		if err == nil || strings.Contains(err.Error(), output) && output != "" {
			t.Fatalf("unexpected output accepted or leaked: %v", err)
		}
	}
	err := prove(context.Background(), func(context.Context, ...string) ([]byte, error) { return nil, errors.New("private transport detail") })
	if err == nil || strings.Contains(err.Error(), "private transport detail") {
		t.Fatalf("transport detail accepted or leaked: %v", err)
	}
}

// TestProofReportsEveryDistinctFailure guards the diagnostic aggregation.
func TestProofReportsEveryDistinctFailure(t *testing.T) {
	calls := 0
	err := prove(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
		code := []string{"200", "404", "200", "403", "403", "403", "200", "200", "200", "200", "200", "200", "200", "200"}[calls]
		if calls == 0 || calls == 2 {
			code = "503"
		}
		calls++
		out := "HTTP/1.1 " + code + " private reason\r\n"
		if strings.Contains(args[len(args)-1], ".js?") {
			out += "Content-Type: text/javascript; charset=utf-8\r\n\r\n"
		}
		return []byte(out), nil
	})
	if err == nil || calls != 14 || !strings.Contains(err.Error(), "probe 1:") || !strings.Contains(err.Error(), "probe 3:") || strings.Contains(err.Error(), "private reason") {
		t.Fatalf("distinct failures missing or response detail leaked: %v", err)
	}
}

// TestProofRejectsHTMLAndMalformedJavaScriptHeaders rejects SPA fallback responses.
func TestProofRejectsHTMLAndMalformedJavaScriptHeaders(t *testing.T) {
	for _, headers := range []string{"Content-Type: text/html\r\n", "", "Content-Type: private-type\r\n", "Content-Type: application/javascript\r\nContent-Type: text/html\r\n"} {
		calls := 0
		err := prove(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
			code := []string{"200", "404", "200", "403", "403", "403", "200", "200", "200", "200", "200", "200", "200", "200"}[calls]
			calls++
			out := "HTTP/1.1 " + code + " private reason\r\n"
			if strings.Contains(args[len(args)-1], ".js?") {
				out += headers + "\r\n"
			}
			return []byte(out), nil
		})
		if err == nil || calls != 14 || !strings.Contains(err.Error(), "JavaScript") || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), headers) && headers != "" {
			t.Fatalf("HTML or malformed headers accepted or leaked: %v", err)
		}
	}
}

// TestNativeReaderRejectsMalformedOrOversizedHeaders prevents filtering from
// repairing bad MIME or accepting a truncated/unbounded native header block.
func TestNativeReaderRejectsMalformedOrOversizedHeaders(t *testing.T) {
	for name, headers := range map[string]string{
		"space-folded":          "Content-Type: application/javascript\r\n text/html\r\n\r\n",
		"tab-folded":            "Content-Type: application/javascript\r\n\ttext/html\r\n\r\n",
		"truncated":             "Content-Type: application/javascript\r\n",
		"NUL in MIME":           "Content-Type: application/java\x00script\r\n\r\n",
		"NUL in ignored header": "Content-Type: application/javascript\r\nX-Private-Fixture: a\x00b\r\n\r\n",
		"oversized line":        "Content-Type: application/javascript\r\nX-Private-Fixture: " + strings.Repeat("x", 65536) + "\r\n\r\n",
		"aggregate budget":      "Content-Type: application/javascript\r\n" + strings.Repeat("X-Private-Fixture: "+strings.Repeat("x", 12000)+"\r\n", 6) + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				connection, writer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = connection.Close() }()
				// Rejection can close the socket before the oversized write ends.
				_, _ = writer.WriteString("HTTP/1.1 200 OK\r\n" + headers)
				_ = writer.Flush()
			}))
			defer server.Close()
			address := strings.TrimPrefix(server.URL, "http://")
			calls := 0
			err := prove(t.Context(), func(ctx context.Context, args ...string) ([]byte, error) {
				code := []string{"200", "404", "200", "403", "403", "403", "200", "200", "200", "200", "200", "200", "200", "200"}[calls]
				calls++
				program := args[len(args)-1]
				if calls != 8 {
					response := "HTTP/1.1 " + code + " Expected\r\n"
					if strings.Contains(program, ".js?") {
						response += "Content-Type: application/javascript\r\n\r\n"
					}
					return []byte(response), nil
				}
				return runSocketProbeFixture(ctx, args, address)
			})
			if err == nil || calls != 14 || !strings.Contains(err.Error(), "probe 8") || strings.Contains(err.Error(), "private") {
				t.Fatalf("folded header accepted or response leaked: %v", err)
			}
		})
	}
}
