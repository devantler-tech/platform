package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type storagePreparer interface {
	prepareStorage(context.Context, string, map[string]json.RawMessage, io.Reader) (string, error)
}

func TestStorageSeedCreatesOnceAndRefusesUncertaintyOrChangedInputs(t *testing.T) {
	previous := credentialPaths
	t.Cleanup(func() { credentialPaths = previous })
	credentialPaths = []string{"infrastructure/email/admin", "infrastructure/email/maintainer", "infrastructure/email/agent"}
	for _, scenario := range []string{"create", "existing", "conflict", "uncertain", "malformed-existing", "deleted-existing", "entropy", "changed-before", "changed-after", "changed-readback", "missing-mount", "wrong-version"} {
		t.Run(scenario, func(t *testing.T) {
			posts, adminReads := 0, 0
			var stored map[string]string
			if scenario == "existing" {
				stored = map[string]string{"passphrase": strings.Repeat("b", 64)}
			}
			if scenario == "malformed-existing" {
				stored = map[string]string{"password": strings.Repeat("b", 64)}
			}
			expected := map[string]json.RawMessage{}
			for _, path := range credentialPaths {
				expected[path] = json.RawMessage(`{"password":"` + strings.Repeat("a", 64) + `"}`)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/sys/health" && r.Header.Get("X-Vault-Token") != "synthetic-seed-token-only" {
					t.Error("seed used the wrong credential")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				encode := func(value any) { _ = json.NewEncoder(w).Encode(value) }
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/health":
					version := serverVersion
					if scenario == "wrong-version" {
						version = "unexpected"
					}
					encode(map[string]any{"initialized": true, "sealed": false, "version": version})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/sys/mounts":
					version := "2"
					if scenario == "missing-mount" {
						version = "1"
					}
					encode(map[string]any{"data": map[string]any{"secret/": map[string]any{"type": "kv", "options": map[string]string{"version": version}}}})
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/secret/data/") && r.URL.Path != "/v1/secret/data/"+storageCredentialPath:
					path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
					value, ok := expected[path]
					if !ok {
						t.Error("seed read an undeclared account")
						w.WriteHeader(http.StatusNotFound)
						return
					}
					if path == credentialPaths[0] {
						adminReads++
						if scenario == "changed-before" || scenario == "changed-after" && adminReads > 1 {
							value = json.RawMessage(`{"password":"` + strings.Repeat("c", 64) + `"}`)
						}
					}
					encode(map[string]any{"data": map[string]any{"data": value, "metadata": map[string]any{"version": 1}}})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/secret/metadata/"+storageCredentialPath:
					if scenario == "deleted-existing" {
						encode(map[string]any{"data": map[string]any{"current_version": 1}})
					} else {
						w.WriteHeader(http.StatusNotFound)
					}
				case r.URL.Path == "/v1/secret/data/"+storageCredentialPath:
					switch r.Method {
					case http.MethodPost:
						posts++
						var body struct {
							Data    map[string]string  `json:"data"`
							Options struct{ CAS *int } `json:"options"`
						}
						if json.NewDecoder(r.Body).Decode(&body) != nil || body.Options.CAS == nil || *body.Options.CAS != 0 || len(body.Data) != 1 || body.Data["passphrase"] != strings.Repeat("03", 32) {
							t.Error("seed did not use the generated key and create-only CAS")
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if scenario == "conflict" {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, "PRIVATE_SYNTHETIC_REMOTE_ERROR")
							return
						}
						stored = body.Data
						if scenario == "uncertain" {
							w.WriteHeader(http.StatusInternalServerError)
							_, _ = io.WriteString(w, "PRIVATE_SYNTHETIC_REMOTE_ERROR")
							return
						}
						encode(map[string]any{"data": map[string]any{"version": 1}})
					case http.MethodGet:
						if stored == nil {
							w.WriteHeader(http.StatusNotFound)
							return
						}
						version := 1
						if scenario == "changed-readback" {
							version = 2
						}
						encode(map[string]any{"data": map[string]any{"data": stored, "metadata": map[string]any{"version": version}}})
					default:
						t.Error("seed used an undeclared method")
						w.WriteHeader(http.StatusMethodNotAllowed)
					}
				default:
					t.Error("seed used an undeclared operation")
					w.WriteHeader(http.StatusForbidden)
				}
			}))
			defer server.Close()
			api, err := newAPI(server.URL)
			if err != nil {
				t.Fatal("cannot prepare the loopback fixture")
			}
			preparer, ok := any(api).(storagePreparer)
			if !ok {
				t.Fatal("bounded storage seed operation is missing")
			}
			var entropy io.Reader = bytes.NewReader(bytes.Repeat([]byte{3}, 32))
			if scenario == "entropy" || scenario == "existing" {
				entropy = strings.NewReader("")
			}
			state, err := preparer.prepareStorage(context.Background(), "synthetic-seed-token-only", expected, entropy)
			if scenario == "create" || scenario == "existing" {
				want := "CREATED"
				if scenario == "existing" {
					want = "EXISTING"
				}
				if err != nil || state != want {
					t.Fatal("confirmed storage preparation did not preserve the expected result")
				}
			} else if err == nil || state != "" || strings.Contains(err.Error(), "PRIVATE_SYNTHETIC") {
				t.Fatal("unsafe storage preparation was accepted or reflected private data")
			}
			wantPosts := 0
			switch scenario {
			case "create", "conflict", "uncertain", "changed-after", "changed-readback":
				wantPosts = 1
			}
			if posts != wantPosts {
				t.Fatal("seed replaced, retried, or wrote before its prerequisites")
			}
		})
	}
}
