package main

import (
	"bytes"
	"strings"
	"testing"
)

const keylessPolicy = `apiVersion: v1alpha1
kind: ImageVerificationConfig
rules:
  - image: registry.example/zone
    keyless:
      issuer: https://issuer.example
      subjectRegex: ^release/v[0-9]+$
`

const installedFixture = `[{"imagePattern":"registry.example/zone","skip":false,"deny":false,"keylessVerifier":{"issuer":"https://issuer.example","subjectRegex":"^release/v[0-9]+$"}}]`

func TestCompleteInstalledPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        int
	}{
		{"exact", installedFixture, 0},
		{"issuer drift", strings.ReplaceAll(installedFixture, "https://issuer.example", "https://stale.example"), 1},
		{"subject drift", strings.ReplaceAll(installedFixture, "^release/v[0-9]+$", "^branch/main$"), 1},
		{"skip drift", strings.ReplaceAll(installedFixture, `"skip":false`, `"skip":true`), 1},
		{"deny drift", strings.ReplaceAll(installedFixture, `"deny":false`, `"deny":true`), 1},
		{"missing flag", strings.ReplaceAll(installedFixture, `"skip":false,`, ""), 2},
		{"null flag", strings.ReplaceAll(installedFixture, `"deny":false`, `"deny":null`), 2},
		{"wrong flag type", strings.ReplaceAll(installedFixture, `"deny":false`, `"deny":"false"`), 2},
		{"missing verifier", `[{"imagePattern":"registry.example/zone","skip":false,"deny":false}]`, 2},
		{"malformed JSON", "{", 2},
		{"trailing input", installedFixture + "{}", 2},
		{"unknown decision", strings.ReplaceAll(installedFixture, `"skip":false`, `"unknown":true,"skip":false`), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			got := compare(keylessPolicy, strings.NewReader(tc.input), &out, &diagnostic)
			if got != tc.want {
				t.Fatalf("status = %d, want %d: %s", got, tc.want, &diagnostic)
			}
			if strings.Contains(diagnostic.String(), "stale.example") || strings.Contains(diagnostic.String(), "branch/main") {
				t.Fatal("diagnostic leaked installed signer values")
			}
		})
	}
}

func TestPublicKeyAndActionDecisions(t *testing.T) {
	policy := `apiVersion: v1alpha1
kind: ImageVerificationConfig
rules:
  - image: registry.example/keyed
    publicKey:
      certificate: expected certificate
  - image: registry.example/skipped
    skip: true
  - image: registry.example/denied
    deny: true
`
	runtime := `[{"imagePattern":"registry.example/keyed","skip":false,"deny":false,"publicKeyVerifier":{"certificate":"expected certificate"}},{"imagePattern":"registry.example/skipped","skip":true,"deny":false},{"imagePattern":"registry.example/denied","skip":false,"deny":true}]`
	for _, tc := range []struct {
		name, input string
		want        int
	}{
		{"all decisions", runtime, 0},
		{"certificate drift", strings.ReplaceAll(runtime, "expected certificate", "stale certificate"), 1},
		{"missing certificate", strings.ReplaceAll(runtime, `"certificate":"expected certificate"`, `"certificate":""`), 2},
		{"reordered decisions", `[{"imagePattern":"registry.example/skipped","skip":true,"deny":false},{"imagePattern":"registry.example/keyed","skip":false,"deny":false,"publicKeyVerifier":{"certificate":"expected certificate"}},{"imagePattern":"registry.example/denied","skip":false,"deny":true}]`, 1},
		{"incomplete rule set", `[{"imagePattern":"registry.example/skipped","skip":true,"deny":false}]`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compare(policy, strings.NewReader(tc.input), &bytes.Buffer{}, &bytes.Buffer{}); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExactSubjectAndCombinedVerifiers(t *testing.T) {
	policy := `apiVersion: v1alpha1
kind: ImageVerificationConfig
rules:
  - image: registry.example/exact
    keyless:
      issuer: https://issuer.example
      subject: release
      subjectRegex: ^release$
    publicKey:
      certificate: expected certificate
`
	runtime := `[{"imagePattern":"registry.example/exact","skip":false,"deny":false,"keylessVerifier":{"issuer":"https://issuer.example","subject":"release","subjectRegex":"^release$"},"publicKeyVerifier":{"certificate":"expected certificate"}}]`
	for _, tc := range []struct {
		name, input string
		want        int
	}{
		{"every applicable verifier", runtime, 0},
		{"exact subject drift", strings.Replace(runtime, `"subject":"release"`, `"subject":"main"`, 1), 1},
		{"regex drift despite exact subject", strings.Replace(runtime, "^release$", "^main$", 1), 1},
		{"public key drift despite keyless", strings.Replace(runtime, "expected certificate", "stale certificate", 1), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compare(policy, strings.NewReader(tc.input), &bytes.Buffer{}, &bytes.Buffer{}); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDeclaredPolicyFailsClosed(t *testing.T) {
	for _, malformed := range []string{
		"", "rules: []", keylessPolicy + "---\nkind: other\n",
		strings.ReplaceAll(keylessPolicy, "subjectRegex:", "unknownMatcher:"),
		strings.ReplaceAll(keylessPolicy, "^release/v[0-9]+$", "("),
		strings.ReplaceAll(keylessPolicy, "https://issuer.example", ""),
	} {
		if _, err := parsePolicy([]byte(malformed)); err == nil {
			t.Fatal("malformed policy accepted")
		}
	}
}
