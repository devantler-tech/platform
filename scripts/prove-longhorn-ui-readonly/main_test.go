package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestProofRequiresEveryPositiveAndNegativeResponse(t *testing.T) {
	for _, broken := range []int{-1, 0, 1, 2, 3, 4, 5} {
		t.Run(string(rune('A'+broken+1)), func(t *testing.T) {
			calls := 0
			execute := func(_ context.Context, args ...string) ([]byte, error) {
				if strings.Join(args[:len(args)-1], " ") != "--context=admin@prod --request-timeout=20s -n oauth2-proxy exec deployment/auth-proxy -c traefik -- /bin/sh -c" {
					t.Fatalf("unexpected command surface: %q", args)
				}
				code := []string{"200", "404", "200", "403", "403", "403"}[calls]
				if calls == broken {
					code = "500"
				}
				calls++
				return []byte("HTTP/1.1 " + code + " Expected\r\n"), nil
			}
			err := prove(context.Background(), execute)
			if (err == nil) != (broken == -1) {
				t.Fatalf("broken response %d: %v", broken, err)
			}
			if broken == -1 && calls != 6 {
				t.Fatalf("only %d probes ran", calls)
			}
		})
	}
}

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
		w.WriteHeader([]int{200, 404, 200, 403, 403, 403}[seen])
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
	if err != nil || seen != 6 {
		t.Fatalf("native request proof: seen=%d error=%v", seen, err)
	}
}

func TestProofRefusesTransportErrorsAndUnexpectedOutput(t *testing.T) {
	for _, output := range []string{"", "HTTP/1.1 302 Redirect\r\n", "private value\nHTTP/1.1 200 OK\n", "HTTP/1.1 200 OK\nbody", "HTTP/1.1 2000 Invalid\n"} {
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
