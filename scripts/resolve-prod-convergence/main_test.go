package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testDigest = "sha256:6cb49cc4c4c2c66b451150ffcd393959fe3630715fddb733046cad04174ab046"

const fixtureWorkflow = `name: CI
jobs:
  changes:
    steps:
      - name: checkout
        uses: actions/checkout@example
      - name: Filter paths
        uses: dorny/paths-filter@example
        id: filter
        with:
          filters: |
            k8s:
              - 'k8s/**'
              - 'ksail.prod.yaml'
              - 'talos*/**'
            docs:
              - 'docs/**'
`

// fixture is a throwaway repository whose history exercises every verdict:
//
//	base ── docsOnly ── deployInput   (main)
//	   └── ejected                     (never merged)
type fixture struct {
	dir         string
	base        string
	docsOnly    string
	deployInput string
	ejected     string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "--quiet", "--initial-branch=main")

	writeFile(t, dir, ".github/workflows/ci.yaml", fixtureWorkflow)
	writeFile(t, dir, "k8s/app.yaml", "replicas: 1\n")
	writeFile(t, dir, "docs/readme.md", "v1\n")
	base := commitAll(t, dir, "base")

	writeFile(t, dir, "docs/readme.md", "v2\n")
	docsOnly := commitAll(t, dir, "docs only")

	writeFile(t, dir, "talos-prod/patch.yaml", "x: 1\n")
	deployInput := commitAll(t, dir, "deploy input")

	gitCmd(t, dir, "checkout", "--quiet", "-b", "ejected", base)
	writeFile(t, dir, "k8s/app.yaml", "replicas: 2\n")
	ejected := commitAll(t, dir, "ejected")
	gitCmd(t, dir, "checkout", "--quiet", "main")

	return fixture{dir: dir, base: base, docsOnly: docsOnly, deployInput: deployInput, ejected: ejected}
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-C", dir,
		"-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
		// Throwaway fixture repository only: no signing key exists in a test run.
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, message string) string {
	t.Helper()
	gitCmd(t, dir, "add", "--all")
	gitCmd(t, dir, "commit", "--quiet", "-m", message)
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

type certificate struct {
	SourceRepositoryDigest string `json:"sourceRepositoryDigest,omitempty"`
	SourceRepositoryURI    string `json:"sourceRepositoryURI,omitempty"`
}

// verifiedAttestation mirrors the shape of `gh attestation verify --format json`.
func verifiedAttestation(commit string) map[string]any {
	return attestationWith(certificate{
		SourceRepositoryDigest: commit,
		SourceRepositoryURI:    "https://github.com/devantler-tech/platform",
	}, provenancePredicate, strings.TrimPrefix(testDigest, "sha256:"))
}

func attestationWith(cert certificate, predicateType, subjectHex string) map[string]any {
	return map[string]any{
		"verificationResult": map[string]any{
			"signature": map[string]any{"certificate": cert},
			"statement": map[string]any{
				"predicateType": predicateType,
				"subject":       []any{map[string]any{"digest": map[string]string{"sha256": subjectHex}}},
				// A forged predicate must never be believed over the certificate.
				"predicate": map[string]any{
					"buildDefinition": map[string]any{
						"resolvedDependencies": []any{map[string]any{"digest": map[string]string{"gitCommit": strings.Repeat("f", 40)}}},
					},
				},
			},
		},
	}
}

type ghStub struct {
	output []byte
	exit   int
	calls  [][]string
}

func (g *ghStub) runner() runner {
	return func(name string, args ...string) ([]byte, int, error) {
		if name == "gh" {
			g.calls = append(g.calls, args)
			return g.output, g.exit, nil
		}
		return execRunner(name, args...)
	}
}

func stubFor(t *testing.T, attestations ...map[string]any) *ghStub {
	t.Helper()
	out, err := json.Marshal(attestations)
	if err != nil {
		t.Fatal(err)
	}
	return &ghStub{output: out}
}

func configFor(f fixture) config {
	return config{
		digest:        testDigest,
		subject:       defaultSubject,
		repo:          defaultRepo,
		identityRegex: defaultIdentityRegex,
		gitDir:        f.dir,
		mainRef:       "main",
		workflow:      ".github/workflows/ci.yaml",
		filter:        "k8s",
	}
}

func TestResolveVerdicts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tests := []struct {
		name   string
		commit string
		want   verdict
	}{
		{name: "prod runs main", commit: f.deployInput, want: converged},
		{name: "ancestor with a deploy input changed since", commit: f.docsOnly, want: behind},
		{name: "older ancestor still missing a deploy input", commit: f.base, want: behind},
		{name: "ejected merge-group artifact", commit: f.ejected, want: diverged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolve(configFor(f), stubFor(t, verifiedAttestation(tt.commit)).runner())
			if got.verdict != tt.want {
				t.Fatalf("verdict = %s (%s), want %s", got.verdict, got.detail, tt.want)
			}
		})
	}
}

// Only docs changed between the attested commit and main, so the deploy-input
// check must report CONVERGED. Removing that check turns this into BEHIND.
func TestResolveIgnoresNonDeployChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := configFor(f)
	cfg.mainRef = f.docsOnly

	got := resolve(cfg, stubFor(t, verifiedAttestation(f.base)).runner())
	if got.verdict != converged {
		t.Fatalf("verdict = %s (%s), want CONVERGED", got.verdict, got.detail)
	}
}

func TestResolveNamesTheChangedDeployInput(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	got := resolve(configFor(f), stubFor(t, verifiedAttestation(f.docsOnly)).runner())
	if !strings.Contains(got.detail, "talos-prod/patch.yaml") || !strings.Contains(got.detail, "talos*/**") {
		t.Fatalf("detail %q does not name the changed input and its pattern", got.detail)
	}
}

func TestResolveVerifiesProvenanceAgainstThePublishingWorkflow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	stub := stubFor(t, verifiedAttestation(f.deployInput))

	resolve(configFor(f), stub.runner())
	if len(stub.calls) != 1 {
		t.Fatalf("gh was called %d times, want 1", len(stub.calls))
	}
	args := stub.calls[0]
	for _, want := range [][]string{
		{"attestation", "verify"},
		{"oci://" + defaultSubject + "@" + testDigest},
		{"--bundle-from-oci"},
		{"--repo", defaultRepo},
		{"--cert-identity-regex", defaultIdentityRegex},
		{"--predicate-type", provenancePredicate},
		{"--format", "json"},
	} {
		if !containsSequence(args, want) {
			t.Fatalf("gh args %v do not contain %v", args, want)
		}
	}
}

func containsSequence(haystack, needle []string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func TestResolveFailsClosedOnAttestationProblems(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hex := strings.TrimPrefix(testDigest, "sha256:")
	repoURI := "https://github.com/devantler-tech/platform"

	tests := []struct {
		name string
		stub *ghStub
	}{
		{name: "verification fails", stub: &ghStub{output: []byte("[]"), exit: 1}},
		{name: "no attestations", stub: &ghStub{output: []byte("[]")}},
		{name: "unparseable output", stub: &ghStub{output: []byte("not json")}},
		{name: "certificate carries no commit", stub: stubFor(t,
			attestationWith(certificate{SourceRepositoryURI: repoURI}, provenancePredicate, hex))},
		{name: "built from another repository", stub: stubFor(t,
			attestationWith(certificate{SourceRepositoryDigest: f.deployInput, SourceRepositoryURI: "https://github.com/someone/platform"}, provenancePredicate, hex))},
		{name: "wrong predicate type", stub: stubFor(t,
			attestationWith(certificate{SourceRepositoryDigest: f.deployInput, SourceRepositoryURI: repoURI}, "https://cyclonedx.org/bom", hex))},
		{name: "subject is another digest", stub: stubFor(t,
			attestationWith(certificate{SourceRepositoryDigest: f.deployInput, SourceRepositoryURI: repoURI}, provenancePredicate, strings.Repeat("0", 64)))},
		{name: "attested commit is not available locally", stub: stubFor(t,
			verifiedAttestation(strings.Repeat("a", 40)))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolve(configFor(f), tt.stub.runner())
			if got.verdict != unknown {
				t.Fatalf("verdict = %s (%s), want UNKNOWN", got.verdict, got.detail)
			}
		})
	}
}

func TestResolveRejectsAMalformedDigestBeforeVerifying(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := configFor(f)
	cfg.digest = "latest"
	stub := stubFor(t, verifiedAttestation(f.deployInput))

	if got := resolve(cfg, stub.runner()); got.verdict != unknown {
		t.Fatalf("verdict = %s, want UNKNOWN", got.verdict)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("gh was called for a malformed digest")
	}
}

func TestResolveFailsClosedWhenMainCannotBeResolved(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := configFor(f)
	cfg.mainRef = "no-such-ref"

	if got := resolve(cfg, stubFor(t, verifiedAttestation(f.base)).runner()); got.verdict != unknown {
		t.Fatalf("verdict = %s (%s), want UNKNOWN", got.verdict, got.detail)
	}
}

func TestDeployInputPatternsFailClosed(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"no changes job": "jobs:\n  other:\n    steps: []\n",
		"no filter step": "jobs:\n  changes:\n    steps:\n      - id: other\n",
		"two filter steps": "jobs:\n  changes:\n    steps:\n" +
			"      - id: filter\n        with:\n          filters: \"k8s: ['k8s/**']\"\n" +
			"      - id: filter\n        with:\n          filters: \"k8s: ['k8s/**']\"\n",
		"filters not a string": "jobs:\n  changes:\n    steps:\n      - id: filter\n        with:\n          filters: 3\n",
		"named filter missing": "jobs:\n  changes:\n    steps:\n      - id: filter\n        with:\n          filters: \"docs: ['docs/**']\"\n",
		"named filter empty":   "jobs:\n  changes:\n    steps:\n      - id: filter\n        with:\n          filters: \"k8s: []\"\n",
		"structured entry":     "jobs:\n  changes:\n    steps:\n      - id: filter\n        with:\n          filters: \"k8s: [{added: 'k8s/**'}]\"\n",
		"not yaml":             "jobs: [\n",
	}
	for name, workflow := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if patterns, err := deployInputPatterns([]byte(workflow), "k8s"); err == nil {
				t.Fatalf("deployInputPatterns() = %v, want an error", patterns)
			}
		})
	}
}

func TestDeployInputPatternsReadsTheNamedFilter(t *testing.T) {
	t.Parallel()

	got, err := deployInputPatterns([]byte(fixtureWorkflow), "k8s")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"k8s/**", "ksail.prod.yaml", "talos*/**"}
	if !slices.Equal(got, want) {
		t.Fatalf("patterns = %v, want %v", got, want)
	}
}

// The live workflow is the one production depends on: its filter must stay
// readable, or every run of this command reports UNKNOWN.
func TestDeployInputPatternsReadsTheRepositoryWorkflow(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	patterns, err := deployInputPatterns(workflow, "k8s")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(patterns, "k8s/**") {
		t.Fatalf("k8s filter %v does not contain k8s/**", patterns)
	}
	if _, err := compileGlobs(patterns); err != nil {
		t.Fatal(err)
	}
}

func TestCompileGlobs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"k8s/**", "k8s/clusters/prod/app.yaml", true},
		{"k8s/**", "k8s-other/app.yaml", false},
		{"ksail.prod.yaml", "ksail.prod.yaml", true},
		{"ksail.prod.yaml", "ksailxprodxyaml", false},
		{"talos*/**", "talos-prod/patches/a.yaml", true},
		{"talos*/**", "docs/talos/a.yaml", false},
		{"tests/validate-pod-security-provider-deployment/**", "tests/validate-pod-security-provider-deployment/case/a.yaml", true},
		{"**/*.md", "docs/guide.md", true},
		{"**/*.md", "readme.md", true},
		{"docs/?.md", "docs/a.md", true},
		{"docs/?.md", "docs/ab.md", false},
	}
	for _, tt := range tests {
		matchers, err := compileGlobs([]string{tt.pattern})
		if err != nil {
			t.Fatalf("compileGlobs(%q): %v", tt.pattern, err)
		}
		if got := matchers[0].MatchString(tt.path); got != tt.want {
			t.Errorf("%q matches %q = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}

	for _, unsupported := range []string{"!k8s/**", "k8s/{a,b}.yaml", "k8s/[ab].yaml"} {
		if _, err := compileGlobs([]string{unsupported}); err == nil {
			t.Errorf("compileGlobs(%q) accepted an unsupported glob", unsupported)
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tests := []struct {
		name   string
		commit string
		args   []string
		want   int
		prefix string
	}{
		{name: "converged", commit: f.deployInput, want: 0, prefix: "CONVERGED "},
		{name: "behind", commit: f.docsOnly, want: 1, prefix: "BEHIND "},
		{name: "diverged", commit: f.ejected, want: 1, prefix: "DIVERGED "},
		{name: "missing digest", commit: f.deployInput, args: []string{}, want: 2, prefix: "UNKNOWN "},
		{name: "stray argument", commit: f.deployInput, args: []string{"--digest", testDigest, "extra"}, want: 2, prefix: "UNKNOWN "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := tt.args
			if args == nil {
				args = []string{"--digest", testDigest}
			}
			args = append([]string{"--git-dir", f.dir, "--main-ref", "main"}, args...)
			var out bytes.Buffer
			got := run(args, &out, stubFor(t, verifiedAttestation(tt.commit)).runner())
			if got != tt.want || !strings.HasPrefix(out.String(), tt.prefix) {
				t.Fatalf("run() = %d, %q; want %d with prefix %q", got, out.String(), tt.want, tt.prefix)
			}
		})
	}
}

// Identical manifests published again re-attest the same digest from another
// run, so several attested commits are legitimate. The least converged wins.
func TestResolveReportsTheLeastConvergedAttestedCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tests := []struct {
		name    string
		commits []string
		want    verdict
	}{
		{name: "main and an older ancestor", commits: []string{f.deployInput, f.docsOnly}, want: behind},
		{name: "older ancestor listed first", commits: []string{f.docsOnly, f.deployInput}, want: behind},
		{name: "main and an ejected commit", commits: []string{f.deployInput, f.ejected}, want: diverged},
		{name: "the same commit twice", commits: []string{f.deployInput, f.deployInput}, want: converged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			attestations := make([]map[string]any, 0, len(tt.commits))
			for _, commit := range tt.commits {
				attestations = append(attestations, verifiedAttestation(commit))
			}
			got := resolve(configFor(f), stubFor(t, attestations...).runner())
			if got.verdict != tt.want {
				t.Fatalf("verdict = %s (%s), want %s", got.verdict, got.detail, tt.want)
			}
		})
	}
}

// Any unverifiable commit among several still makes the whole answer UNKNOWN.
func TestResolveFailsClosedWhenAnyAttestedCommitIsUnavailable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	stub := stubFor(t, verifiedAttestation(f.deployInput), verifiedAttestation(strings.Repeat("a", 40)))
	if got := resolve(configFor(f), stub.runner()); got.verdict != unknown {
		t.Fatalf("verdict = %s (%s), want UNKNOWN", got.verdict, got.detail)
	}
}

func TestRunReportsFlagErrorsOnTheVerdictLine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	got := run([]string{"--digets", testDigest}, &out, (&ghStub{}).runner())
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if got != 2 || len(lines) != 1 || !strings.HasPrefix(lines[0], "UNKNOWN ") {
		t.Fatalf("run() = %d, %q; want exit 2 and one UNKNOWN line", got, out.String())
	}
}
