package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Keep the production SDK and signer; inspect only the external HTTP boundary.
type r2SessionFixture struct {
	token string
	seen  int
	api   *r2HTTPFixture
	t     *testing.T
}

func (f *r2SessionFixture) RoundTrip(request *http.Request) (*http.Response, error) {
	f.t.Helper()
	if request.Header.Get("X-Amz-Security-Token") != f.token {
		f.t.Fatal("request used a missing, altered or ambient session token")
	}
	_, signed, _ := strings.Cut(request.Header.Get("Authorization"), "SignedHeaders=")
	headers, _, _ := strings.Cut(signed, ",")
	if strings.Contains(";"+headers+";", ";x-amz-security-token;") != (f.token != "") {
		f.t.Fatal("session token was not bound to the request signature")
	}
	f.seen++
	return f.api.RoundTrip(request)
}

func TestR2SessionCredentialsAreExplicitlySigned(t *testing.T) {
	t.Setenv("AWS_SESSION_TOKEN", "unapproved-ambient-session-canary")
	for _, token := range []string{"", "fixture-session-token/+_=.-" + strings.Repeat("c", 128)} {
		t.Run(map[bool]string{true: "scoped-session", false: "legacy-static"}[token != ""], func(t *testing.T) {
			config, credential := r2PrivateFacts(t)
			if token != "" {
				raw, err := json.Marshal(map[string]string{"accessKeyId": "fixture-access-key", "secretAccessKey": strings.Repeat("b", 64), "sessionToken": token})
				if err != nil || os.WriteFile(credential, raw, 0600) != nil {
					t.Fatal("could not prepare synthetic session input")
				}
			}
			facts, secrets, err := loadR2Facts(config, credential)
			if err != nil {
				t.Fatal("valid explicit credential file was refused")
			}
			store, err := newR2JournalStore(facts, secrets)
			if err != nil {
				t.Fatal("valid explicit credentials could not construct a store")
			}
			fixture := &r2SessionFixture{token: token, api: &r2HTTPFixture{}, t: t}
			attachR2Fixture(store, fixture)
			if store.Initialize(context.Background()) != nil || fixture.seen != 2 || fixture.api.unsafe {
				t.Fatal("session did not complete one signed conditional write and readback")
			}
		})
	}
}

func TestR2SessionInputsRefuseAmbiguousOrUnsafeTokens(t *testing.T) {
	for _, token := range []any{"", nil, 42, true, "short", strings.Repeat("x", 6145), "fixture-session\ncanary", "fixture-session\x00canary", "fixture-session\x1bcanary", "fixture session canary"} {
		config, credential := r2PrivateFacts(t)
		raw, err := json.Marshal(map[string]any{"accessKeyId": "fixture-access-key", "secretAccessKey": strings.Repeat("b", 64), "sessionToken": token})
		if err != nil || os.WriteFile(credential, raw, 0600) != nil {
			t.Fatal("could not prepare synthetic refusal input")
		}
		facts, secrets, err := loadR2Facts(config, credential)
		if err == nil {
			_, err = newR2JournalStore(facts, secrets)
		}
		if err == nil || strings.Contains(err.Error(), "canary") {
			t.Fatal("unsafe session input was accepted or disclosed")
		}
	}
	for _, fields := range []string{
		`"sessionToken":"fixture-session-canary","sessionToken":"fixture-session-canary"`,
		`"SessionToken":"fixture-session-canary"`,
		`"sessionToken":"fixture-session-canary","unapproved":"canary"`,
	} {
		config, credential := r2PrivateFacts(t)
		raw := `{"accessKeyId":"fixture-access-key","secretAccessKey":"` + strings.Repeat("b", 64) + `",` + fields + `}`
		if os.WriteFile(credential, []byte(raw), 0600) != nil {
			t.Fatal("could not prepare synthetic schema refusal")
		}
		if _, _, err := loadR2Facts(config, credential); err == nil {
			t.Fatal("ambiguous session credential schema was accepted")
		}
	}
}

func TestR2RefusedSessionDoesNotRefreshOrRetry(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "unapproved-ambient-access-canary")
	t.Setenv("AWS_SECRET_ACCESS_KEY", strings.Repeat("z", 64))
	store, err := newR2JournalStore(r2Settings{AccountID: strings.Repeat("a", 32), Bucket: "fixture-journal", Key: "alerts/state.json", Instance: "fixture-instance"}, r2Credentials{AccessKeyID: "fixture-access-key", SecretAccessKey: strings.Repeat("b", 64), SessionToken: "fixture-session-token-canary"})
	if err != nil {
		t.Fatal("could not construct explicit session")
	}
	fixture := &r2SessionFixture{token: "fixture-session-token-canary", api: &r2HTTPFixture{putStatus: http.StatusForbidden}, t: t}
	attachR2Fixture(store, fixture)
	if store.Initialize(context.Background()) == nil || fixture.seen != 1 || fixture.api.puts != 1 || fixture.api.gets != 0 || fixture.api.unsafe {
		t.Fatal("refused session retried, changed credentials or acknowledged storage")
	}
}
