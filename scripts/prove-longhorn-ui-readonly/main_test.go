package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProofRequiresEveryPositiveAndNegativeResponse rejects each failed check.
func TestProofRequiresEveryPositiveAndNegativeResponse(t *testing.T) {
	for broken := -1; broken < 14; broken++ {
		t.Run(strconv.Itoa(broken+1), func(t *testing.T) {
			calls := 0
			execute := func(_ context.Context, args ...string) ([]byte, error) {
				if strings.Join(args[:len(args)-1], " ") != "--context=admin@prod --request-timeout=20s -n oauth2-proxy exec deployment/auth-proxy -c traefik -- /bin/sh -c" {
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

// TestNativeProbeSendsOnlyFixedHarmlessRequests exercises the real shell pipeline.
func TestNativeProbeSendsOnlyFixedHarmlessRequests(t *testing.T) {
	if _, err := exec.LookPath("nc"); err != nil {
		t.Fatal("native proof fixture requires netcat")
	}
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
	address := strings.ReplaceAll(strings.TrimPrefix(server.URL, "http://"), ":", " ")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := prove(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		program := args[len(args)-1]
		program = strings.ReplaceAll(program, "nc -w 10 127.0.0.1 8080", "nc -w 2 "+address)
		program = strings.ReplaceAll(program, "nc -w 10 longhorn-frontend.longhorn-system.svc.cluster.local 80", "nc -w 2 "+address)
		return exec.CommandContext(ctx, "/bin/sh", "-c", program).Output()
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
