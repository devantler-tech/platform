package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func liveFixture(t *testing.T) (configuration, map[string][]byte) {
	t.Helper()
	root := configFixture(t)
	config, status := loadConfiguration(root)
	if status != pass {
		t.Fatal(status)
	}
	responses := map[string][]byte{}
	for path, command := range map[string]string{bootstrapPath: "--namespace=flux-system get configmap variables-cluster --output=json", storePath: "--namespace=arc-runners get secretstore openbao --output=json"} {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		value, err := yamlObject(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		responses[command] = encoded
	}
	return config, responses
}
func TestLiveReviewedAgreementUsesOnlyNonsecretReads(t *testing.T) {
	config, responses := liveFixture(t)
	var calls []string
	ca, status := liveConfiguration(context.Background(), config, func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		body, ok := responses[call]
		if !ok {
			t.Fatalf("unexpected read %s", call)
		}
		return body, nil
	})
	if status != pass || trustedRoots(ca) == nil || len(calls) != 2 {
		t.Fatalf("live configuration got %s, calls %d", status, len(calls))
	}
}
func TestLiveConfigurationRejectsDriftAndFailedReads(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		failure        bool
		want           outcome
	}{
		{"bootstrap App drift", "Iv1.synthetic", "Iv1.other", false, failIdentity},
		{"plaintext drift", "https://", "http://", false, holdTransport},
		{"reader drift", "arc-secret-reader", "external-secrets", false, failConfig},
		{"API failure", "", "", true, failConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, responses := liveFixture(t)
			_, got := liveConfiguration(context.Background(), config, func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				body, ok := responses[call]
				if !ok {
					t.Fatal("unexpected command")
				}
				if tc.failure {
					return nil, errors.New("sensitive-error-canary")
				}
				return []byte(strings.Replace(string(body), tc.from, tc.to, 1)), nil
			})
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRuntimeRequiresTLSBeforeReaderTokenAndAlwaysStopsForward(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		tlsFails, readerMissing bool
		want                    outcome
	}{
		{"safe orchestration", false, false, pass},
		{"TLS fails", true, false, failTransport},
		{"reader missing", false, true, "HOLD_READER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, responses := liveFixture(t)
			var events []string
			operations := runtimeOperations{
				execute: func(_ context.Context, args ...string) ([]byte, error) {
					call := strings.Join(args, " ")
					if body, ok := responses[call]; ok {
						return body, nil
					}
					if call != "--namespace=arc-runners create token arc-secret-reader --duration=5m" {
						t.Fatal("unexpected command")
					}
					events = append(events, "reader")
					if tc.readerMissing {
						return nil, errors.New("sensitive-canary")
					}
					return []byte("synthetic.reader.jwt"), nil
				},
				forward: func(_ context.Context, port int) (string, func(), error) {
					if port != 8200 {
						t.Fatal("forward lost reviewed listener port")
					}
					events = append(events, "forward")
					return "https://127.0.0.1:12345", func() { events = append(events, "joined") }, nil
				},
				handshake: func(_ context.Context, endpoint string, ca []byte) error {
					events = append(events, "TLS")
					if endpoint != "https://127.0.0.1:12345" || trustedRoots(ca) == nil {
						t.Fatal("TLS lost endpoint or trust binding")
					}
					if tc.tlsFails {
						return errors.New("sensitive-TLS-canary")
					}
					return nil
				},
				identity: func(_ context.Context, options verificationOptions) outcome {
					events = append(events, "identity")
					if options.githubURL != "https://api.github.com" || options.expectedClientID != config.clientID || options.readerJWT != "synthetic.reader.jwt" || options.baoClient == nil {
						t.Fatal("identity lost reviewed bindings")
					}
					return pass
				},
			}
			if got := verifyRuntime(context.Background(), config, operations); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			want := "forward|TLS|reader|identity|joined"
			if tc.tlsFails {
				want = "forward|TLS|joined"
			} else if tc.readerMissing {
				want = "forward|TLS|reader|joined"
			}
			if strings.Join(events, "|") != want {
				t.Fatalf("unsafe credential or cleanup ordering: %v", events)
			}
		})
	}
}

func TestListenerTLSRejectsWrongServiceIdentity(t *testing.T) {
	f := newFixture(t)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.bao.Certificate().Raw})
	if listenerTLS(context.Background(), f.bao.URL, ca) == nil {
		t.Fatal("listener accepted a certificate for another hostname")
	}
	if listenerTLS(context.Background(), strings.Replace(f.bao.URL, "https:", "http:", 1), ca) == nil {
		t.Fatal("listener accepted plaintext")
	}
}

func TestForwardedServiceTargetPortCanDiffer(t *testing.T) {
	if got := forwardingAddress("Forwarding from 127.0.0.1:12345 -> 8202"); got != "https://127.0.0.1:12345" {
		t.Fatalf("Service port translation rejected: %q", got)
	}
}

func TestForwardingReadinessCannotSelectAnotherEndpoint(t *testing.T) {
	for _, line := range []string{"Forwarding from 0.0.0.0:12345 -> 8202", "Forwarding from other.example:12345 -> 8202", "Forwarding from 127.0.0.1:65536 -> 8202", "Forwarding from 127.0.0.1:01234 -> 8202", "Forwarding from 127.0.0.1:12345 -> 0", "Forwarding from 127.0.0.1:12345 -> 8202 extra"} {
		if forwardingAddress(line) != "" {
			t.Fatal("unsafe readiness endpoint was accepted")
		}
	}
}
