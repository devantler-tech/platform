package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalReads(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, source string
		want         int
	}{
		{"historical queue append", "set -euo pipefail\nf(){ local provider=$1 queue seen_k=\"\"; queue=\"${queue}path\"; }", 1},
		{"short variable append", "set -u\nf(){ local q; q=\"$q/x\"; }", 1},
		{"prepend", "set -u\nf(){ local q; q=\"prefix${q}\"; }", 1},
		{"ordinary read before assignment", "set -u\nf(){ local q; echo \"$q\"; q=ready; }", 1},
		{"array initializer still reads existing local", "set -u\nf(){ local q; local -a values=(\"$q\"); }", 1},
		{"global initializer still reads existing local", "set -u\nf(){ local q; declare -g value=\"$q\"; }", 1},
		{"initialized local expands outer value", "set -u\nq=outer\nf(){ local q=\"$q\"; }", 0},
		{"bare declaration precedes later initializer", "set -u\nf(){ local q value=\"$q\"; }", 1},
		{"initializer reads previously bare local", "set -u\nf(){ local q; local q=\"$q\"; }", 1},
		{"initialized historical queue", "set -euo pipefail\nf(){ local provider=$1 queue=\"\" seen_k=\"\"; queue=\"${queue}path\"; }", 0},
		{"assignment before read", "set -u\nf(){ local q; q=ready; echo \"$q\"; }", 0},
		{"while read assigns first", "set -u\nf(){ local q; while IFS= read -r q; do q=\"${q}x\"; done; }", 0},
		{"for assigns first", "set -u\nf(){ local q; for q in one two; do q=\"${q}x\"; done; }", 0},
		{"arithmetic for initializes first", "set -u\nf(){ local q; for ((q=0;q<3;q++)); do echo \"$q\"; done; }", 0},
		{"arithmetic assignment reads RHS first", "set -u\nf(){ local q; ((q=q+1)); }", 1},
		{"arithmetic increment reads first", "set -u\nf(){ local q; ((q++)); }", 1},
		{"for input reads before loop assignment", "set -u\nf(){ local q; for q in \"$q\"; do :; done; }", 1},
		{"positional set argument is not an option", "set -- -u\nf(){ local q; echo \"$q\"; }", 0},
		{"other function is independent", "set -u\nf(){ local q; q=one; }; g(){ q=\"${q}x\"; }", 0},
		{"safe default", "set -u\nf(){ local q; echo \"${q:-fallback}\"; q=ready; }", 0},
		{"default assigns", "set -u\nf(){ local q; : \"${q:=fallback}\"; echo \"$q\"; }", 0},
		{"printf assigns", "set -u\nf(){ local q; printf -v q %s ready; echo \"$q\"; }", 0},
		{"nounset long option", "set -o nounset\nf(){ local q; echo \"$q\"; }", 1},
		{"nounset disabled", "set -u\nset +u\nf(){ local q; echo \"$q\"; }", 0},
		{"no nounset", "set -eo pipefail\nf(){ local q; echo \"$q\"; }", 0},
		{"quoted shell example is data", "set -u\nf(){ cat <<'EXAMPLE'\nlocal q; q=\"${q}x\"\nEXAMPLE\n}", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			findings, err := lintSource("fixture.sh", strings.NewReader(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != test.want {
				t.Fatalf("got %v, want %d findings", findings, test.want)
			}
			if test.want > 0 && (!strings.Contains(findings[0], "f") || !strings.Contains(findings[0], "before assignment")) {
				t.Fatalf("diagnostic does not explain the function and problem: %v", findings)
			}
		})
	}
}

func TestScanCannotPassWithoutExaminingInput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, test := range []struct {
		name string
		path string
	}{
		{"missing input", filepath.Join(root, "missing")},
		{"empty directory", root},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if status := run([]string{test.path}, &output); status != 2 {
				t.Fatalf("unknown input returned %d: %s", status, output.String())
			}
		})
	}
}

func TestLimitRangeGuardQueueRegression(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../guard-limitrange-premise.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	anchor := "local provider=$1 queue=\"\" seen_k=\"\""
	if strings.Count(source, anchor) != 1 {
		t.Fatal("historical queue initialization anchor must be unique")
	}
	clean, err := lintSource("guard-limitrange-premise.sh", strings.NewReader(source))
	if err != nil || len(clean) != 0 {
		t.Fatalf("current guard: findings=%v error=%v", clean, err)
	}
	// Recreate the issue's historical declaration against the real current guard,
	// preserving its actual first-read path rather than a second lint model.
	mutant := strings.Replace(source, anchor, "local provider=$1 queue seen_k=\"\"", 1)
	findings, err := lintSource("mutant.sh", strings.NewReader(mutant))
	if err != nil || len(findings) != 1 || !strings.Contains(findings[0], "local queue before assignment") {
		t.Fatalf("recreated regression: findings=%v error=%v", findings, err)
	}
}

func TestScanReportsARealFileAndRefusesMalformedShell(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, source string
		status       int
	}{
		{"uninitialized local", "set -u\nf(){ local q; echo \"$q\"; }\n", 1},
		{"initialized local", "set -u\nf(){ local q=\"\"; echo \"$q\"; }\n", 0},
		{"malformed shell", "set -u\nf(){\n", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "fixture.sh")
			if err := os.WriteFile(path, []byte(test.source), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if status := run([]string{root}, &output); status != test.status {
				t.Fatalf("got exit %d, want %d: %s", status, test.status, output.String())
			}
			if test.status == 0 && !strings.Contains(output.String(), "examined=1") {
				t.Fatalf("success has no coverage evidence: %s", output.String())
			}
		})
	}
}

type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) { return 0, errors.New("fixture output refused") }

func TestFailedCoverageOutputCannotReportSuccess(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "clean.sh")
	if err := os.WriteFile(path, []byte("set -u\nf(){ local q=ready; echo \"$q\"; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := run([]string{path}, refusingWriter{}); status != 2 {
		t.Fatalf("failed coverage output reported exit %d instead of unknown", status)
	}
}
