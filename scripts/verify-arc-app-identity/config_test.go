package main

import (
	"bytes"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFixture(t *testing.T) string {
	t.Helper()
	f := newFixture(t)
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.bao.Certificate().Raw}))
	root := t.TempDir()
	writeFixture(t, root, bootstrapPath, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: variables-cluster\n  namespace: flux-system\ndata:\n  github_app_client_id: Iv1.synthetic\n")
	writeFixture(t, root, storePath, "apiVersion: external-secrets.io/v1\nkind: SecretStore\nmetadata:\n  name: openbao\n  namespace: arc-runners\nspec:\n  provider:\n    vault:\n      server: "+baoServer+"\n      path: secret\n      version: v2\n      caBundle: "+ca+"\n      auth:\n        kubernetes:\n          mountPath: kubernetes\n          role: arc-secret-reader\n          serviceAccountRef:\n            name: arc-secret-reader\n")
	return root
}
func writeFixture(t *testing.T, root, path, body string) {
	t.Helper()
	name := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func mutateFixture(t *testing.T, root, path, from, to string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, path, strings.Replace(string(body), from, to, 1))
}

func TestReviewedTLSConfiguration(t *testing.T) {
	root := configFixture(t)
	config, result := loadConfiguration(root)
	if result != pass || config.clientID != "Iv1.synthetic" || config.store.server != baoServer {
		t.Fatalf("valid reviewed configuration got %s", result)
	}
}

func TestDedicatedReviewedTLSListener(t *testing.T) {
	root := configFixture(t)
	config, status := loadConfiguration(root)
	port, ok := baoListenerPort(config.store.server)
	if status != pass || !ok || port != baoTLSPort {
		t.Fatal("separate reviewed TLS listener was not accepted")
	}
	for _, server := range []string{"https://" + baoTLSName + ":08204", "https://" + baoTLSName + ":8443", baoServer + "/other", baoServer + "?other", "https://openbao-active.openbao.svc.cluster.local:8204"} {
		if _, ok := baoListenerPort(server); ok {
			t.Fatal("invalid listener declaration accepted")
		}
	}
}
func TestConfigurationCannotSelectAnotherIdentityOrTransport(t *testing.T) {
	for _, tc := range []struct {
		name, path, from, to string
		want                 outcome
	}{
		{"plaintext", storePath, "https://", "http://", holdTransport},
		{"other server", storePath, baoTLSName, "other.example", holdTransport},
		{"other role", storePath, "role: arc-secret-reader", "role: external-secrets", failConfig},
		{"other account", storePath, "name: arc-secret-reader", "name: external-secrets", failConfig},
		{"other namespace", storePath, "namespace: arc-runners", "namespace: default", failConfig},
		{"other store", storePath, "path: secret", "path: management", failConfig},
		{"other mount", storePath, "mountPath: kubernetes", "mountPath: other", failConfig},
		{"other auth", storePath, "kubernetes:", "tokenSecretRef:", failConfig},
		{"malformed CA", storePath, "caBundle:", "caBundle: invalid #", holdTransport},
		{"missing client", bootstrapPath, "github_app_client_id:", "other:", failConfig},
		{"duplicate role", storePath, "role: arc-secret-reader", "role: arc-secret-reader\n          role: other", failConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := configFixture(t)
			mutateFixture(t, root, tc.path, tc.from, tc.to)
			_, got := loadConfiguration(root)
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
func TestPreflightRejectsPlaintextWithoutExecutionOrCredentials(t *testing.T) {
	root := configFixture(t)
	mutateFixture(t, root, storePath, "https://", "http://")
	var output bytes.Buffer
	called := false
	code := run([]string{"--preflight"}, root, &output, func(configuration) outcome { called = true; return pass })
	if code == 0 || output.String() != "ARC_APP_IDENTITY=HOLD_TRANSPORT\n" || called {
		t.Fatalf("current plaintext source must stop before runtime: exit=%d, outcome=%q, called=%v", code, output.String(), called)
	}
}
func TestCLIEmitsOnlyOutcomesAndRequiresProtectedInvocation(t *testing.T) {
	root := configFixture(t)
	for _, tc := range []struct {
		name   string
		args   []string
		result outcome
		env    bool
		want   string
	}{
		{"preflight", []string{"--preflight"}, pass, false, "ARC_APP_IDENTITY=TRANSPORT_CONFIG_READY\n"},
		{"local verify", []string{"--verify"}, pass, false, "ARC_APP_IDENTITY=HOLD_INVOCATION\n"},
		{"unknown argument", []string{"--key=sensitive-key-canary"}, pass, true, "ARC_APP_IDENTITY=HOLD_INVOCATION\n"},
		{"protected success", []string{"--verify"}, pass, true, "ARC_APP_IDENTITY=PASS\n"},
		{"protected missing entry", []string{"--verify"}, holdEntry, true, "ARC_APP_IDENTITY=HOLD_ENTRY\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, value := range map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REPOSITORY": "devantler-tech/platform", "ARC_IDENTITY_CONFIRM": "verify-arc-app-identity"} {
				if !tc.env {
					value = ""
				}
				t.Setenv(name, value)
			}
			var output bytes.Buffer
			called := false
			code := run(tc.args, root, &output, func(configuration) outcome { called = true; return tc.result })
			if output.String() != tc.want {
				t.Fatalf("unexpected output %q", output.String())
			}
			wantCalled := tc.name == "protected success" || tc.name == "protected missing entry"
			if called != wantCalled {
				t.Fatalf("runtime called=%v", called)
			}
			if strings.Contains(tc.want, "=PASS") && code != 0 {
				t.Fatal("valid proof did not succeed")
			}
		})
	}
}
