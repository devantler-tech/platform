package customeragent_test

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// This characterizes the deployed distribution, rather than assuming that a
// client_credentials access token has the same claims as an ID token. Removing
// the grant, changing its audience/subject, or extending expiry must fail here.
func TestNativeCustomerAgentTokens(t *testing.T) {
	if os.Getenv("CUSTOMER_AGENT_NATIVE_TEST") != "1" {
		t.Skip("set CUSTOMER_AGENT_NATIVE_TEST=1 for the isolated native Dex proof")
	}
	fixture := nativeDexFixture(t)
	key := fixtureSigningKey(t, fixture)
	status, body := fixture.tokens(t, "business-portal-agent", "synthetic-agent-fixture", "openid")
	if status != http.StatusOK {
		t.Fatalf("native agent grant failed: status=%d error=%v", status, body["error"])
	}
	for _, field := range []string{"access_token", "id_token"} {
		claims := verifiedFixtureClaims(t, key, body[field])
		if claims["iss"] != "https://login.devantler.tech" || claims["sub"] != "ChVidXNpbmVzcy1wb3J0YWwtYWdlbnQ" || claims["aud"] != "business-portal-agent" {
			t.Fatalf("%s has an unexpected native identity", field)
		}
		iat, iatOK := claims["iat"].(float64)
		exp, expOK := claims["exp"].(float64)
		if !iatOK || !expOK || exp-iat != 300 || exp <= float64(time.Now().Unix()) {
			t.Fatalf("%s does not have the required five-minute native lifetime", field)
		}
	}
	if body["refresh_token"] != nil {
		t.Fatal("machine grant unexpectedly issued a refresh token")
	}
	if status, body := fixture.tokens(t, "business-portal-agent", "wrong-synthetic-secret", "openid"); status == http.StatusOK || body["error"] != "invalid_client" {
		t.Fatal("wrong machine credential was not rejected")
	}
	if status, body := fixture.tokens(t, "business-portal-agent", "synthetic-agent-fixture", "openid offline_access"); status == http.StatusOK || body["error"] != "invalid_scope" {
		t.Fatal("machine grant did not refuse offline access")
	}
	// oauth2.grantTypes is global. The browser client's credential can therefore
	// mint only its own identity; the portal must reject both tokens at its agent
	// boundary and reject the empty-connector subject at its human boundary.
	status, browser := fixture.tokens(t, "business-portal", "synthetic-browser-fixture", "openid")
	if status != http.StatusOK {
		t.Fatal("browser-client negative control could not obtain its own native token")
	}
	for _, field := range []string{"access_token", "id_token"} {
		claims := verifiedFixtureClaims(t, key, browser[field])
		if claims["aud"] != "business-portal" || claims["sub"] != "Cg9idXNpbmVzcy1wb3J0YWw" {
			t.Fatal("browser credential crossed the native machine identity boundary")
		}
	}
	if fixture.browserAuthStatus(t) != http.StatusBadRequest {
		t.Fatal("machine client unexpectedly accepted a browser redirect")
	}
	t.Log("native signature, issuer, audience, subject and 300-second lifetime passed for both token types; credential, refresh and browser-flow refusals passed")
}

func nativeDexConfiguration(t *testing.T) (string, []byte) {
	t.Helper()
	manifest, err := os.ReadFile("../../../k8s/providers/hetzner/apps/customer-identity/helm-release-dex.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var release struct {
		Spec struct {
			Values struct {
				Image  struct{ Repository, Tag string }
				Config map[string]any
			}
		}
	}
	if err := yaml.Unmarshal(manifest, &release); err != nil {
		t.Fatal(err)
	}
	values := release.Spec.Values
	image := values.Image.Repository + ":" + values.Image.Tag
	if !strings.HasPrefix(image, "ghcr.io/devantler-tech/platform-dex:") || !strings.Contains(image, "@sha256:") {
		t.Fatal("the native proof requires the pinned first-party Dex distribution")
	}
	// Only persistence, listening and upstream human login are fixture-specific.
	// Production issuer, clients, grant policy and expiry remain unchanged.
	config := values.Config
	config["storage"] = map[string]any{"type": "memory"}
	config["web"] = map[string]any{"http": "0.0.0.0:5556"}
	config["enablePasswordDB"] = true
	delete(config, "connectors")
	delete(config, "frontend")
	configBytes, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return image, configBytes
}

func nativeDexFixture(t *testing.T) nativeFixture {
	t.Helper()
	image, config := nativeDexConfiguration(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	// This contains only synthetic fixture references; the non-root image must
	// read the copied file. Its parent is the test's private temporary directory.
	if err := os.WriteFile(path, config, 0644); err != nil {
		t.Fatal(err)
	}
	// These literal credentials exist only in this disposable memory-backed Dex.
	id := ownedNativeContainer(t, "create", "--platform=linux/amd64",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--memory=256m", "--pids-limit=128",
		"--publish", "127.0.0.1::5556",
		"--env", "CUSTOMER_PORTAL_CLIENT_SECRET=synthetic-browser-fixture",
		"--env", "CUSTOMER_PORTAL_AGENT_CLIENT_SECRET=synthetic-agent-fixture",
		image, "dex", "serve", "/tmp/config.yaml")
	// Copy through Docker: bind mounts would read the remote daemon's paths.
	nativeDockerOutput(t, "cp", path, id+":/tmp/config.yaml")
	nativeDockerOutput(t, "start", id)
	fixture := nativeFixtureEndpoint(t, id, "5556/tcp")
	fixture.waitReady(t, "/.well-known/openid-configuration")
	return fixture
}

func (fixture nativeFixture) tokens(t *testing.T, clientID, secret, scope string) (int, map[string]any) {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scope}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.baseURL+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth(clientID, secret)
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatal("native synthetic token request failed")
	}
	defer closeFixtureResponse(t, response)
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		t.Fatal("native token response was invalid")
	}
	return response.StatusCode, body
}

func (fixture nativeFixture) browserAuthStatus(t *testing.T) int {
	t.Helper()
	response, err := fixture.get(t, "/auth?"+url.Values{"client_id": {"business-portal-agent"}, "response_type": {"code"}, "scope": {"openid"}, "redirect_uri": {"https://portal.devantler.tech/callback"}}.Encode())
	if err != nil {
		t.Fatal("machine browser-flow negative control failed")
	}
	// Dex selects the connector before it validates the redirect. Follow only
	// relative auth routes on this owned fixture, never an upstream or callback.
	for hops := 0; hops < 3 && response.StatusCode == http.StatusFound; hops++ {
		location := response.Header.Get("Location")
		if !strings.HasPrefix(location, "/auth/") {
			break
		}
		_ = response.Body.Close()
		response, err = fixture.get(t, location)
		if err != nil {
			t.Fatal("machine browser-flow connector refusal failed")
		}
	}
	defer closeFixtureResponse(t, response)
	return response.StatusCode
}

func fixtureSigningKey(t *testing.T, fixture nativeFixture) *rsa.PublicKey {
	t.Helper()
	response, err := fixture.get(t, "/keys")
	if err != nil {
		t.Fatal("native signing-key read failed")
	}
	defer closeFixtureResponse(t, response)
	var set struct{ Keys []struct{ Kty, N, E string } }
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&set) != nil || len(set.Keys) != 1 || set.Keys[0].Kty != "RSA" {
		t.Fatal("unexpected native signing-key set")
	}
	n, nErr := base64.RawURLEncoding.DecodeString(set.Keys[0].N)
	e, eErr := base64.RawURLEncoding.DecodeString(set.Keys[0].E)
	if nErr != nil || eErr != nil || len(n) < 256 {
		t.Fatal("invalid native signing key")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
}

func verifiedFixtureClaims(t *testing.T, key *rsa.PublicKey, value any) map[string]any {
	t.Helper()
	token, ok := value.(string)
	parts := strings.Split(token, ".")
	if !ok || len(parts) != 3 {
		t.Fatal("native response does not contain a signed JWT")
	}
	header, hErr := base64.RawURLEncoding.DecodeString(parts[0])
	signature, sErr := base64.RawURLEncoding.DecodeString(parts[2])
	var metadata struct{ Alg string }
	if hErr != nil || sErr != nil || json.Unmarshal(header, &metadata) != nil || metadata.Alg != "RS256" {
		t.Fatal("unexpected native JWT signing algorithm")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		t.Fatal("native JWT signature verification failed")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		t.Fatal("native JWT claims could not be decoded")
	}
	return claims
}
