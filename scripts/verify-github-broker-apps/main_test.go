package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeApp struct {
	slug, clientID string
	id             int64
	key            *rsa.PrivateKey
	pem            string
}

func newFakeApp(t *testing.T, slug string, id int64, clientID string) fakeApp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return fakeApp{slug: slug, clientID: clientID, id: id, key: key, pem: string(block)}
}

// fakeGitHub answers GET /app for whichever App's key signed the JWT, the way
// GitHub identifies the App from the token alone.
func fakeGitHub(t *testing.T, apps []fakeApp, status int) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		for _, app := range apps {
			if rsa.VerifyPKCS1v15(&app.key.PublicKey, crypto.SHA256, digest[:], signature) == nil {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": app.id, "slug": app.slug, "client_id": app.clientID,
					"owner": map[string]string{"login": "devantler-tech"},
				})
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
}

func environment(one, two fakeApp) map[string]string {
	return map[string]string{
		"BOTANTLER_1_APP_ID": "1025824", "BOTANTLER_1_CLIENT_ID": one.clientID, "BOTANTLER_1_PRIVATE_KEY": one.pem,
		"BOTANTLER_2_APP_ID": "4126012", "BOTANTLER_2_CLIENT_ID": two.clientID, "BOTANTLER_2_PRIVATE_KEY": two.pem,
	}
}

func TestRun(t *testing.T) {
	one := newFakeApp(t, "botantler-1", 1025824, "Iv23fixtureone")
	two := newFakeApp(t, "botantler-2", 4126012, "Iv23fixturetwo")
	other := newFakeApp(t, "someone-else", 1025824, "Iv23fixtureone")

	cases := []struct {
		name   string
		apps   []fakeApp
		status int
		mutate func(map[string]string)
		want   string
		code   int
	}{
		{name: "both keys belong to their Apps", apps: []fakeApp{one, two}, status: http.StatusOK, want: "GITHUB_BROKER_APPS=VERIFIED apps=2\n"},
		{name: "an unreviewed App ID", apps: []fakeApp{one, two}, status: http.StatusOK,
			mutate: func(e map[string]string) { e["BOTANTLER_1_APP_ID"] = "1025825" },
			want:   "GITHUB_BROKER_APPS=UNVERIFIED app=botantler-1 stage=config\n", code: 2},
		{name: "the App IDs swapped", apps: []fakeApp{one, two}, status: http.StatusOK,
			mutate: func(e map[string]string) { e["BOTANTLER_1_APP_ID"], e["BOTANTLER_2_APP_ID"] = "4126012", "1025824" },
			want:   "GITHUB_BROKER_APPS=UNVERIFIED app=botantler-1 stage=config\n", code: 2},
		{name: "a missing client ID", apps: []fakeApp{one, two}, status: http.StatusOK,
			mutate: func(e map[string]string) { e["BOTANTLER_2_CLIENT_ID"] = "" },
			want:   "GITHUB_BROKER_APPS=UNVERIFIED app=botantler-2 stage=config\n", code: 2},
		{name: "a malformed key", apps: []fakeApp{one, two}, status: http.StatusOK,
			mutate: func(e map[string]string) { e["BOTANTLER_2_PRIVATE_KEY"] = "not a key" },
			want:   "GITHUB_BROKER_APPS=UNVERIFIED app=botantler-2 stage=key\n", code: 2},
		{name: "the other App's key", apps: []fakeApp{one, two}, status: http.StatusOK,
			mutate: func(e map[string]string) { e["BOTANTLER_2_PRIVATE_KEY"] = one.pem },
			want:   "GITHUB_BROKER_APPS=UNVERIFIED app=botantler-2 stage=identity\n", code: 2},
		{name: "a key GitHub knows as another App", apps: []fakeApp{other, two}, status: http.StatusOK,
			mutate: func(e map[string]string) { e["BOTANTLER_1_PRIVATE_KEY"] = other.pem },
			want:   "GITHUB_BROKER_APPS=UNVERIFIED app=botantler-1 stage=identity\n", code: 2},
		{name: "GitHub refuses the JWT", apps: []fakeApp{one, two}, status: http.StatusUnauthorized,
			want: "GITHUB_BROKER_APPS=UNVERIFIED app=botantler-1 stage=github\n", code: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := fakeGitHub(t, tc.apps, tc.status)
			defer server.Close()
			env := environment(one, two)
			if tc.mutate != nil {
				tc.mutate(env)
			}
			var out bytes.Buffer
			code := run(context.Background(), func(k string) string { return env[k] }, server.Client(), server.URL, time.Now, &out)
			if code != tc.code || out.String() != tc.want {
				t.Fatalf("got exit %d and %q, want exit %d and %q", code, out.String(), tc.code, tc.want)
			}
			for _, secret := range []string{one.pem, two.pem, other.pem} {
				body := strings.Split(secret, "\n")[1]
				if strings.Contains(out.String(), body) {
					t.Fatal("the status line carries key material")
				}
			}
		})
	}
}
