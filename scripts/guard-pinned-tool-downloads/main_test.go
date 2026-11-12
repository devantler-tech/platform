// Package main holds the contract for pinned tool downloads in the platform's workflows (#6065).
//
// A pinned tool is one a workflow step downloads to a file and then checks against its reviewed
// SHA-256 pin. One transient network error on such a download used to turn a required check red,
// because curl tried once. Every such download now retries a bounded number of times, within a
// bounded total time, and the checksum check that follows it is untouched: curl's retry only
// covers the transfer, so a file whose checksum fails is never retried as if it were a network
// error, and never accepted.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Bounds every pinned download must stay inside. Attempts: at most this many retries after the
// first try. Time: retries start only within the retry window, and each attempt has its own limit,
// so the whole download ends within roughly (retries+1) × per-attempt limit.
const (
	maxRetries       = 5
	maxRetryWindow   = 300
	maxAttemptSecond = 600
)

// download is one curl statement in a workflow step script.
type download struct {
	where     string            // file:step
	statement string            // the statement, continuation lines joined
	script    string            // the whole step script
	env       map[string]string // the step's own literal environment
	offset    int               // where the statement starts in the script
}

var curlCommand = regexp.MustCompile(`(^|[\s;(&|"])curl\s`)

// statements returns the curl statements of a step script, continuation lines joined, with their
// offsets. Comment lines are not commands.
func statements(script string) []download {
	var out []download
	lines := strings.Split(script, "\n")
	offset := 0
	for i := 0; i < len(lines); i++ {
		start := offset
		line := lines[i]
		offset += len(line) + 1
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !curlCommand.MatchString(trimmed) {
			continue
		}
		statement := trimmed
		for strings.HasSuffix(strings.TrimSpace(lines[i]), "\\") && i+1 < len(lines) {
			i++
			offset += len(lines[i]) + 1
			statement = strings.TrimSuffix(strings.TrimSpace(statement), "\\") + " " + strings.TrimSpace(lines[i])
		}
		out = append(out, download{statement: statement, script: script, offset: start})
	}
	return out
}

// words splits a shell statement into words, keeping quoted text together and dropping the quotes.
// It reports the unquoted operators it met (|, ||, &&, ;) separately.
func words(statement string) (args []string, operators []string) {
	var cur strings.Builder
	inWord := false
	quote := byte(0)
	flush := func() {
		if inWord {
			args = append(args, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(statement); i++ {
		ch := statement[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else {
				cur.WriteByte(ch)
			}
		case ch == '\'' || ch == '"':
			quote = ch
			inWord = true
		case ch == ' ' || ch == '\t':
			flush()
		case ch == '|' || ch == '&' || ch == ';':
			flush()
			op := string(ch)
			if i+1 < len(statement) && (statement[i+1] == '|' || statement[i+1] == '&') {
				op += string(statement[i+1])
				i++
			}
			operators = append(operators, op)
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	flush()
	return args, operators
}

// pinned reports whether a curl statement is a pinned tool download: it writes to a file and the
// same step checks a SHA-256 after it.
func (d download) pinned() bool {
	args, _ := words(d.statement)
	toFile := false
	for _, a := range args {
		if a == "-o" || a == "--output" {
			toFile = true
		}
	}
	return toFile && strings.Contains(d.script[d.offset:], "sha256sum")
}

func flagValue(args []string, name string) (int, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			v, err := strconv.Atoi(args[i+1])
			return v, err == nil
		}
	}
	return 0, false
}

func has(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// problems lists every way a pinned download breaks the contract.
func (d download) problems() []string {
	args, operators := words(d.statement)
	var out []string
	// A short-option cluster such as -fsSL carries -f.
	fail := has(args, "--fail")
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'f') {
			fail = true
		}
	}
	if !fail {
		out = append(out, "no --fail: an HTTP error page would be saved as the tool")
	}
	if n, ok := flagValue(args, "--retry"); !ok || n < 1 || n > maxRetries {
		out = append(out, fmt.Sprintf("needs --retry between 1 and %d", maxRetries))
	}
	if !has(args, "--retry-all-errors") {
		out = append(out, "needs --retry-all-errors: plain --retry does not retry a failed TLS connect")
	}
	if n, ok := flagValue(args, "--retry-max-time"); !ok || n < 1 || n > maxRetryWindow {
		out = append(out, fmt.Sprintf("needs --retry-max-time between 1 and %d seconds", maxRetryWindow))
	}
	if n, ok := flagValue(args, "--max-time"); !ok || n < 1 || n > maxAttemptSecond {
		out = append(out, fmt.Sprintf("needs --max-time between 1 and %d seconds per attempt", maxAttemptSecond))
	}
	// curl resets a partial file before it retries; it cannot reset a pipe, and a swallowed
	// failure would hand a missing file to the checksum and install that follow.
	for _, op := range operators {
		switch op {
		case "|":
			out = append(out, "pipes its output: a retried transfer cannot be reset in a pipe")
		case "||", "&":
			out = append(out, "does not stop the step when the download fails ("+op+")")
		}
	}
	// A word, not a substring: a checksum file such as tool.sha256sum is an ordinary download.
	if has(args, "sha256sum", "shasum") {
		out = append(out, "verifies inside the download statement: a checksum must be checked after it, on its own")
	}
	return out
}

// step is one workflow or composite-action step: its script and its literal environment.
type step struct {
	run string
	env map[string]string
}

// scripts returns every step of a workflow or composite action that runs a script, keyed by a
// readable place.
func scripts(t *testing.T, path string) map[string]step {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := map[string]step{}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			var s step
			line := 0
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if k.Value == "run" && v.Kind == yaml.ScalarNode {
					s.run, line = v.Value, v.Line
				}
				if k.Value == "env" && v.Kind == yaml.MappingNode {
					s.env = map[string]string{}
					for j := 0; j+1 < len(v.Content); j += 2 {
						if value := v.Content[j+1].Value; !strings.Contains(value, "${{") {
							s.env[v.Content[j].Value] = value
						}
					}
				}
			}
			if line > 0 {
				out[fmt.Sprintf("%s:%d", filepath.ToSlash(path), line)] = s
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	return out
}

func repositoryDownloads(t *testing.T) []download {
	t.Helper()
	root := filepath.Join("..", "..")
	var files []string
	for _, pattern := range []string{".github/workflows/*.yaml", ".github/workflows/*.yml", ".github/actions/*/action.yaml", ".github/actions/*/action.yml"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	sort.Strings(files)
	if len(files) < 10 {
		t.Fatalf("found only %d workflow files; the scan would be vacuous", len(files))
	}
	var out []download
	for _, f := range files {
		for where, s := range scripts(t, f) {
			for _, d := range statements(s.run) {
				d.env = s.env
				d.where = strings.TrimPrefix(where, filepath.ToSlash(root)+"/")
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].where+out[i].statement < out[j].where+out[j].statement })
	return out
}

// Every pinned tool download in the workflows retries within bounds, and the same step checks the
// checksum after it.
func TestEveryPinnedToolDownloadRetriesWithinBounds(t *testing.T) {
	pinned := 0
	kubectl := 0
	for _, d := range repositoryDownloads(t) {
		if !d.pinned() {
			continue
		}
		pinned++
		if strings.Contains(d.statement, "dl.k8s.io/release/") {
			kubectl++
		}
		for _, p := range d.problems() {
			t.Errorf("%s: %s\n  %s", d.where, p, d.statement)
		}
	}
	// The scope is not allowed to go quiet: these counts are what the workflows held when the
	// contract was written (#6065). A drop means the scan lost them, not that they went away.
	if pinned < 44 || kubectl < 35 {
		t.Fatalf("found %d pinned downloads (%d kubectl); expected at least 44 (35): the scan is not seeing the workflows", pinned, kubectl)
	}
	t.Logf("pinned tool downloads checked: %d (%d kubectl)", pinned, kubectl)
}

// The contract refuses each way a download can break it, and admits the shape the workflows use.
func TestPinnedDownloadProblems(t *testing.T) {
	const good = `curl -fsSL --retry 3 --retry-all-errors --retry-delay 5 --retry-max-time 120 --connect-timeout 10 --max-time 120 "https://dl.k8s.io/release/${V}/bin/linux/amd64/kubectl" -o "$p"`
	for _, tc := range []struct{ name, statement, want string }{
		{"the workflows' shape", good, ""},
		{"long options", `curl --disable --fail --silent --show-error --location --max-time 60 --retry 3 --retry-all-errors --retry-max-time 120 https://x.invalid/t --output "$p"`, ""},
		{"no retry", strings.Replace(good, "--retry 3 ", "", 1), "needs --retry between"},
		{"unbounded attempts", strings.Replace(good, "--retry 3 ", "--retry 10 ", 1), "needs --retry between"},
		{"plain retry", strings.Replace(good, "--retry-all-errors ", "", 1), "needs --retry-all-errors"},
		{"no retry window", strings.Replace(good, "--retry-max-time 120 ", "", 1), "needs --retry-max-time"},
		{"unbounded retry window", strings.Replace(good, "--retry-max-time 120 ", "--retry-max-time 0 ", 1), "needs --retry-max-time"},
		{"no per-attempt limit", strings.Replace(good, "--max-time 120 ", "", 1), "needs --max-time"},
		{"no fail", strings.Replace(good, "-fsSL", "-sSL", 1), "no --fail"},
		{"swallowed failure", good + " || true", "does not stop the step"},
		{"piped", strings.Replace(good, ` -o "$p"`, ` -o - | tar xz`, 1), "pipes its output"},
		{"checksum inside", good + ` && sha256sum --check sums`, "verifies inside the download statement"},
		{"a checksum file", strings.Replace(good, "kubectl\"", "kubectl.sha256sum\"", 1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(download{statement: tc.statement}.problems(), "; ")
			if tc.want == "" && got != "" {
				t.Fatalf("refused a good download: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want a problem naming %q", got, tc.want)
			}
		})
	}
}

// A download is in scope when it writes a file that the same step checks afterwards, and only then.
func TestPinnedScope(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         bool
	}{
		{"checked after", "curl -fsSL https://x.invalid/t -o \"$p\"\nprintf '%s  %s\\n' \"$S\" \"$p\" | sha256sum --check\n", true},
		{"continued lines", "curl -fsSL \\\n  https://x.invalid/t \\\n  --output \"$p\"\nsha256sum --check sums\n", true},
		{"no checksum", "curl -fsSL https://x.invalid/t -o \"$p\"\n", false},
		{"checksum only before", "sha256sum --check sums\ncurl -fsSL https://x.invalid/t -o \"$p\"\n", false},
		{"not a file", "curl -fsS http://127.0.0.1/healthz\nsha256sum --check sums\n", false},
		{"a comment", "# curl -fsSL https://x.invalid/t -o p\nsha256sum --check sums\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := statements(tc.script)
			got := len(found) == 1 && found[0].pinned()
			if got != tc.want {
				t.Fatalf("pinned=%v (statements %d), want %v", got, len(found), tc.want)
			}
		})
	}
}

// simpleAssignment is a line such as kubectl_path="$RUNNER_TEMP/kubectl": a variable the download
// writes to, set without running anything.
var simpleAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*="[^"$` + "`" + `]*(\$\{?RUNNER_TEMP\}?[^"$` + "`" + `]*)?"$`)

// checkedPrefix is the part of a step that downloads and checks one kubectl: the step's simple
// assignments, the download, and everything up to and including the first checksum line after it.
func (d download) checkedPrefix() (string, bool) {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	for _, line := range strings.Split(d.script[:d.offset], "\n") {
		if simpleAssignment.MatchString(strings.TrimSpace(line)) {
			b.WriteString(strings.TrimSpace(line) + "\n")
		}
	}
	rest := d.script[d.offset:]
	end := strings.Index(rest, "sha256sum")
	if end < 0 {
		return "", false
	}
	if nl := strings.IndexByte(rest[end:], '\n'); nl >= 0 {
		end += nl
	} else {
		end = len(rest)
	}
	b.WriteString(rest[:end] + "\n")
	b.WriteString("printf CHECKED\n")
	return b.String(), true
}

// Run each kubectl download with its real checksum line against a stand-in curl that serves the
// wrong bytes: the step must fail at the checksum, after exactly one download. A retry covers the
// transfer only, so it can neither re-download after a mismatch nor let the wrong file through.
// The control gives the step the checksum of the bytes served: then it passes, after one download.
func TestChecksumMismatchFailsOnceAndIsNeverRetried(t *testing.T) {
	bin := t.TempDir()
	served := "not the reviewed kubectl\n"
	fake := "#!/bin/sh\n" +
		"echo x >>\"$CURL_CALLS\"\n" +
		"out=''\nwhile [ $# -gt 0 ]; do case \"$1\" in -o|--output) out=$2; shift ;; esac; shift; done\n" +
		"[ -n \"$out\" ] || exit 64\nprintf '%s' \"$SERVED\" >\"$out\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(served))
	servedSum := hex.EncodeToString(sum[:])
	ran := 0
	for _, d := range repositoryDownloads(t) {
		if !d.pinned() || !strings.Contains(d.statement, "dl.k8s.io/release/") {
			continue
		}
		prefix, ok := d.checkedPrefix()
		if !ok {
			t.Errorf("%s: no checksum line after the download", d.where)
			continue
		}
		run := func(t *testing.T, replaceSum bool) (string, int, error) {
			t.Helper()
			work := t.TempDir()
			calls := filepath.Join(work, "calls")
			script := prefix
			if replaceSum {
				script = regexp.MustCompile(`[0-9a-f]{64}`).ReplaceAllString(script, servedSum)
			}
			cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", script)
			cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "RUNNER_TEMP=" + work,
				"CURL_CALLS=" + calls, "SERVED=" + served, "KUBECTL_VERSION=v0.0.0"}
			for k, v := range d.env {
				if replaceSum && k == "KUBECTL_SHA256" {
					v = servedSum
				}
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			out, err := cmd.CombinedOutput()
			raw, _ := os.ReadFile(calls)
			return string(out), strings.Count(string(raw), "x"), err
		}
		t.Run(d.where, func(t *testing.T) {
			out, calls, err := run(t, false)
			if err == nil || strings.Contains(out, "CHECKED") {
				t.Fatalf("the wrong bytes passed the checksum: %s", out)
			}
			// The refusal must be the checksum's own verdict, not a usage error of a sha256sum that
			// does not take the workflow's GNU options.
			if !strings.Contains(out, "FAILED") {
				t.Fatalf("the step failed, but not at a checksum mismatch: %s", out)
			}
			if calls != 1 {
				t.Fatalf("downloaded %d times; a checksum mismatch must not download again", calls)
			}
			out, calls, err = run(t, true)
			if err != nil || !strings.Contains(out, "CHECKED") || calls != 1 {
				t.Fatalf("control: the matching checksum did not pass after one download (%d): %v %s", calls, err, out)
			}
		})
		ran++
	}
	if ran < 35 {
		t.Fatalf("exercised %d kubectl downloads; expected at least 35", ran)
	}
}
