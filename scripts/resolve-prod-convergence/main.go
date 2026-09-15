// Command resolve-prod-convergence reports whether production runs what main says.
//
// Every production deploy promotes one digest of the platform manifests to
// `:latest` and attests where it was built. A merge-group commit is exactly the
// commit main advances to, so the signed source commit of the published digest
// answers "is prod on main?" without touching the cluster.
//
// Usage:
//
//	go run ./scripts/resolve-prod-convergence --digest sha256:<64 hex> [--git-dir .] [--main-ref origin/main]
//
// It prints one line, "<VERDICT> <detail>", and exits with:
//
//	0  CONVERGED  the attested commit is main, or an ancestor with no deploy-input change since
//	1  BEHIND     an ancestor of main, but a deploy input changed since (prod missed a merge)
//	1  DIVERGED   not reachable from main; redeploying main converges it
//	2  UNKNOWN    anything could not be read or verified; never reported as CONVERGED
//
// The source commit is taken from the attestation's signing certificate, which
// GitHub fills in from the workflow's OIDC token. The provenance predicate is
// deliberately not read: the workflow that signs it controls its contents.
//
// That certificate names the commit that TRIGGERED the run, not the commit a job
// checked out. The merge-group heal checks out main inside the failed group's
// run, so a healed artifact is attested with the ejected group's commit and
// reads as DIVERGED although it holds main. The verdict is still actionable:
// in both cases redeploying main converges prod, and does so once.
//
// One digest can carry several attestations when identical manifests are
// published again from another run. Each attested commit is judged, and the
// least converged verdict wins.
//
// Deploy inputs are the paths-filter patterns of the `changes` job in
// .github/workflows/ci.yaml, read at the main revision being compared, so this
// command and the deploy trigger cannot disagree about what a deploy input is.
//
// The caller must have each attested commit's object locally (for an ejected
// merge-group artifact, fetch it by SHA first); a missing object is UNKNOWN.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type verdict string

const (
	converged verdict = "CONVERGED"
	behind    verdict = "BEHIND"
	diverged  verdict = "DIVERGED"
	unknown   verdict = "UNKNOWN"
)

// severity orders the verdicts a judged commit can produce, least converged last.
var severity = map[verdict]int{converged: 0, behind: 1, diverged: 2}

const (
	provenancePredicate = "https://slsa.dev/provenance/v1"
	defaultSubject      = "ghcr.io/devantler-tech/platform/manifests"
	defaultRepo         = "devantler-tech/platform"
	// The workflows that publish `:latest`: a merge-group deploy or its heal
	// (ci.yaml on a queue ref) and a manual deploy (cd.yaml on main). Any other
	// signer leaves the verdict UNKNOWN rather than trusted.
	defaultIdentityRegex = `^https://github\.com/devantler-tech/platform/\.github/workflows/(ci|cd)\.yaml` +
		`@refs/heads/(main|gh-readonly-queue/main/pr-[0-9]+-[0-9a-f]{40})$`
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type config struct {
	digest        string
	subject       string
	repo          string
	identityRegex string
	gitDir        string
	mainRef       string
	workflow      string
	filter        string
}

// runner executes a command. exit is the process exit code; err is non-nil only
// when the command could not be run at all.
type runner func(name string, args ...string) (stdout []byte, exit int, err error)

func execRunner(name string, args ...string) ([]byte, int, error) {
	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.Bytes(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return nil, -1, err
	}
	return stdout.Bytes(), 0, nil
}

type result struct {
	verdict verdict
	detail  string
}

func resolve(cfg config, run runner) result {
	res, err := evaluate(cfg, run)
	if err != nil {
		return result{verdict: unknown, detail: err.Error()}
	}
	return res
}

func evaluate(cfg config, run runner) (result, error) {
	if !digestPattern.MatchString(cfg.digest) {
		return result{}, fmt.Errorf("digest %q is not sha256:<64 hex>", cfg.digest)
	}

	commits, err := attestedCommits(cfg, run)
	if err != nil {
		return result{}, err
	}

	mainSHA, err := gitOutput(cfg, run, "rev-parse", "--verify", "--end-of-options", cfg.mainRef+"^{commit}")
	if err != nil {
		return result{}, fmt.Errorf("resolve %s: %w", cfg.mainRef, err)
	}
	if !commitPattern.MatchString(mainSHA) {
		return result{}, fmt.Errorf("%s resolved to %q, not a commit", cfg.mainRef, mainSHA)
	}

	j := judge{cfg: cfg, run: run, mainSHA: mainSHA}
	var worst result
	for i, commit := range commits {
		res, err := j.commit(commit)
		if err != nil {
			return result{}, err
		}
		if i == 0 || severity[res.verdict] > severity[worst.verdict] {
			worst = res
		}
	}
	if len(commits) > 1 {
		worst.detail += fmt.Sprintf(" (least converged of %d attested commits)", len(commits))
	}
	return worst, nil
}

// judge compares attested commits with one main revision, reading the deploy
// inputs at most once.
type judge struct {
	cfg      config
	run      runner
	mainSHA  string
	patterns []string
	matchers []*regexp.Regexp
}

func (j *judge) commit(commit string) (result, error) {
	if commit == j.mainSHA {
		return result{verdict: converged, detail: "prod runs main " + j.mainSHA}, nil
	}

	if _, exit, err := git(j.cfg, j.run, "cat-file", "-e", commit+"^{commit}"); err != nil || exit != 0 {
		return result{}, fmt.Errorf("attested commit %s is not available locally", commit)
	}

	_, exit, err := git(j.cfg, j.run, "merge-base", "--is-ancestor", commit, j.mainSHA)
	if err != nil {
		return result{}, fmt.Errorf("ancestry check: %w", err)
	}
	switch exit {
	case 0:
	case 1:
		return result{
			verdict: diverged,
			detail: fmt.Sprintf("prod's artifact was attested by a run for %s, which main %s cannot reach "+
				"(an ejected merge-group deploy, or that run's heal of main)", commit, j.mainSHA),
		}, nil
	default:
		return result{}, fmt.Errorf("ancestry check exited %d", exit)
	}

	if err := j.loadDeployInputs(); err != nil {
		return result{}, err
	}

	changed, err := gitOutputRaw(j.cfg, j.run, "diff", "--name-only", "--no-renames", "-z", commit, j.mainSHA, "--")
	if err != nil {
		return result{}, fmt.Errorf("diff %s..%s: %w", commit, j.mainSHA, err)
	}
	for _, path := range strings.Split(string(changed), "\x00") {
		if path == "" {
			continue
		}
		for i, m := range j.matchers {
			if m.MatchString(path) {
				return result{
					verdict: behind,
					detail: fmt.Sprintf("prod runs %s; main %s changed deploy input %s (pattern %s)",
						commit, j.mainSHA, path, j.patterns[i]),
				}, nil
			}
		}
	}
	return result{verdict: converged, detail: fmt.Sprintf("prod runs %s; no deploy input changed up to main %s", commit, j.mainSHA)}, nil
}

func (j *judge) loadDeployInputs() error {
	if j.matchers != nil {
		return nil
	}
	workflow, err := gitOutputRaw(j.cfg, j.run, "show", j.mainSHA+":"+j.cfg.workflow)
	if err != nil {
		return fmt.Errorf("read %s at %s: %w", j.cfg.workflow, j.mainSHA, err)
	}
	patterns, err := deployInputPatterns(workflow, j.cfg.filter)
	if err != nil {
		return err
	}
	matchers, err := compileGlobs(patterns)
	if err != nil {
		return err
	}
	j.patterns, j.matchers = patterns, matchers
	return nil
}

type attestation struct {
	VerificationResult struct {
		Signature struct {
			Certificate struct {
				SourceRepositoryDigest string `json:"sourceRepositoryDigest"`
				SourceRepositoryURI    string `json:"sourceRepositoryURI"`
			} `json:"certificate"`
		} `json:"signature"`
		Statement struct {
			PredicateType string `json:"predicateType"`
			Subject       []struct {
				Digest map[string]string `json:"digest"`
			} `json:"subject"`
		} `json:"statement"`
	} `json:"verificationResult"`
}

// attestedCommits verifies the digest's provenance and returns the distinct
// commits its signing certificates name, in the order they were returned.
func attestedCommits(cfg config, run runner) ([]string, error) {
	out, exit, err := run("gh", "attestation", "verify", "oci://"+cfg.subject+"@"+cfg.digest,
		"--bundle-from-oci",
		"--repo", cfg.repo,
		"--cert-identity-regex", cfg.identityRegex,
		"--predicate-type", provenancePredicate,
		"--format", "json")
	if err != nil {
		return nil, fmt.Errorf("run gh attestation verify: %w", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("provenance for %s did not verify (gh exited %d)", cfg.digest, exit)
	}

	var attestations []attestation
	if err := json.Unmarshal(out, &attestations); err != nil {
		return nil, fmt.Errorf("parse verification output: %w", err)
	}
	if len(attestations) == 0 {
		return nil, errors.New("verification returned no attestations")
	}

	wantURI := "https://github.com/" + cfg.repo
	wantHex := strings.TrimPrefix(cfg.digest, "sha256:")
	var commits []string
	for i, a := range attestations {
		cert := a.VerificationResult.Signature.Certificate
		if cert.SourceRepositoryURI != wantURI {
			return nil, fmt.Errorf("attestation %d was built from %q, not %s", i, cert.SourceRepositoryURI, wantURI)
		}
		if a.VerificationResult.Statement.PredicateType != provenancePredicate {
			return nil, fmt.Errorf("attestation %d has predicate type %q", i, a.VerificationResult.Statement.PredicateType)
		}
		if !namesDigest(a, wantHex) {
			return nil, fmt.Errorf("attestation %d does not name %s as its subject", i, cfg.digest)
		}
		if !commitPattern.MatchString(cert.SourceRepositoryDigest) {
			return nil, fmt.Errorf("attestation %d certificate carries no source commit", i)
		}
		if !contains(commits, cert.SourceRepositoryDigest) {
			commits = append(commits, cert.SourceRepositoryDigest)
		}
	}
	return commits, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func namesDigest(a attestation, wantHex string) bool {
	for _, s := range a.VerificationResult.Statement.Subject {
		if s.Digest["sha256"] == wantHex {
			return true
		}
	}
	return false
}

// deployInputPatterns returns the named paths-filter patterns from the
// `changes` job's `filter` step. Anything it cannot read unambiguously is an
// error, because an empty or partial list would report CONVERGED for free.
func deployInputPatterns(workflow []byte, filterName string) ([]string, error) {
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				ID   string         `yaml:"id"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &doc); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	changes, ok := doc.Jobs["changes"]
	if !ok {
		return nil, errors.New("workflow has no changes job")
	}

	var filters string
	found := 0
	for _, step := range changes.Steps {
		if step.ID != "filter" {
			continue
		}
		found++
		value, ok := step.With["filters"].(string)
		if !ok {
			return nil, errors.New("filter step has no string filters input")
		}
		filters = value
	}
	if found != 1 {
		return nil, fmt.Errorf("changes job has %d steps with id filter, want exactly 1", found)
	}

	var named map[string][]any
	if err := yaml.Unmarshal([]byte(filters), &named); err != nil {
		return nil, fmt.Errorf("parse filters: %w", err)
	}
	entries, ok := named[filterName]
	if !ok || len(entries) == 0 {
		return nil, fmt.Errorf("filter %q is missing or empty", filterName)
	}
	patterns := make([]string, 0, len(entries))
	for i, entry := range entries {
		pattern, ok := entry.(string)
		if !ok || pattern == "" {
			return nil, fmt.Errorf("filter %q entry %d is not a plain path pattern", filterName, i)
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

// compileGlobs turns paths-filter globs into anchored regular expressions. It
// supports `**`, `*` and `?`; negation, braces and classes are refused rather
// than approximated, since a wrong match would hide a deploy input.
func compileGlobs(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "{}[]") {
			return nil, fmt.Errorf("unsupported glob %q", pattern)
		}
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(pattern); {
			switch {
			case strings.HasPrefix(pattern[i:], "**/"):
				b.WriteString("(?:.*/)?")
				i += 3
			case strings.HasPrefix(pattern[i:], "**"):
				b.WriteString(".*")
				i += 2
			case pattern[i] == '*':
				b.WriteString("[^/]*")
				i++
			case pattern[i] == '?':
				b.WriteString("[^/]")
				i++
			default:
				b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
				i++
			}
		}
		b.WriteString("$")
		re, err := regexp.Compile(b.String())
		if err != nil {
			return nil, fmt.Errorf("compile glob %q: %w", pattern, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func git(cfg config, run runner, args ...string) ([]byte, int, error) {
	return run("git", append([]string{"--no-replace-objects", "-C", cfg.gitDir}, args...)...)
}

func gitOutputRaw(cfg config, run runner, args ...string) ([]byte, error) {
	out, exit, err := git(cfg, run, args...)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, fmt.Errorf("git %s exited %d", args[0], exit)
	}
	return out, nil
}

func gitOutput(cfg config, run runner, args ...string) (string, error) {
	out, err := gitOutputRaw(cfg, run, args...)
	return strings.TrimSpace(string(out)), err
}

func exitCode(v verdict) int {
	switch v {
	case converged:
		return 0
	case behind, diverged:
		return 1
	default:
		return 2
	}
}

func run(args []string, stdout io.Writer, runCmd runner) int {
	flags := flag.NewFlagSet("resolve-prod-convergence", flag.ContinueOnError)
	// The output contract is one verdict line, so flag errors and usage text are
	// reported through that line rather than printed ahead of it.
	flags.SetOutput(io.Discard)
	cfg := config{}
	flags.StringVar(&cfg.digest, "digest", "", "published manifests digest (sha256:<64 hex>)")
	flags.StringVar(&cfg.subject, "subject", defaultSubject, "OCI repository the digest belongs to")
	flags.StringVar(&cfg.repo, "repo", defaultRepo, "repository the provenance must be built from")
	flags.StringVar(&cfg.identityRegex, "identity-regex", defaultIdentityRegex, "signing workflow identity the provenance must match")
	flags.StringVar(&cfg.gitDir, "git-dir", ".", "local clone of the repository")
	flags.StringVar(&cfg.mainRef, "main-ref", "origin/main", "ref to compare production against")
	flags.StringVar(&cfg.workflow, "workflow", ".github/workflows/ci.yaml", "workflow holding the deploy-input filter")
	flags.StringVar(&cfg.filter, "filter", "k8s", "paths-filter name listing deploy inputs")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(stdout, "%s %v\n", unknown, err)
		return exitCode(unknown)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stdout, "%s unexpected arguments: %v\n", unknown, flags.Args())
		return exitCode(unknown)
	}

	res := resolve(cfg, runCmd)
	fmt.Fprintf(stdout, "%s %s\n", res.verdict, res.detail)
	return exitCode(res.verdict)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, execRunner))
}
