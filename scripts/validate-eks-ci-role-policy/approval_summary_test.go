package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A provider update moves one approved-surface line. The job summary must name
// that exact edit, taken from the real validation of a changed render.
func TestApprovalSummaryNamesTheExactLedgerEdit(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)
	providerPackage := regexp.MustCompile(`(package: ghcr\.io/devantler-tech/provider-upjet-unifi:)v[0-9]+\.[0-9]+\.[0-9]+`)
	if !providerPackage.Match(rendered) {
		t.Fatal("rendered surface has no UniFi provider package to update")
	}
	changed := providerPackage.ReplaceAll(rendered, []byte("${1}v9.8.7"))

	validationErr := validateAuthorization(role, boundary, changed)
	var mismatch *surfaceMismatchError
	if !errors.As(validationErr, &mismatch) {
		t.Fatalf("updated provider error = %v, want an approved-surface mismatch", validationErr)
	}

	summary := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(summary, []byte("earlier step\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if written, err := writeApprovalSummary(validationErr, summary); err != nil || !written {
		t.Fatalf("write approval summary: written=%v error=%v", written, err)
	}
	contents, err := os.ReadFile(summary) //nolint:gosec // Test-owned temporary file.
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	if !strings.HasPrefix(text, "earlier step\n") {
		t.Fatalf("summary replaced earlier content:\n%s", text)
	}
	if !strings.Contains(text, "### Production authorization approval needed") || !strings.Contains(text, surfaceLedgerFile) {
		t.Fatalf("summary does not name the approval or its file:\n%s", text)
	}
	block := text[strings.Index(text, "```diff\n")+len("```diff\n"):]
	block = block[:strings.Index(block, "```\n")]
	if got, want := strings.Split(strings.TrimSuffix(block, "\n"), "\n"), mismatch.delta; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summary lines = %q, want exactly the ledger edit %q", got, want)
	}
	removed, added := 0, 0
	for _, line := range mismatch.delta {
		if !strings.Contains(line, "|Provider||provider-upjet-unifi ") {
			t.Fatalf("provider update moved an unrelated ledger line: %s", line)
		}
		switch line[0] {
		case '-':
			removed++
		case '+':
			added++
		}
	}
	if removed != 1 || added != 1 {
		t.Fatalf("provider update edit = %d removed, %d added; want one of each: %v", removed, added, mismatch.delta)
	}
}

func TestApprovalSummaryWritesNothingWithoutAShowableMismatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{name: "passing validation"},
		{name: "other validation error", err: errors.New("unapproved role manifest fingerprint: 0")},
		{name: "empty mismatch", err: &surfaceMismatchError{}},
		{name: "unsafe ledger line", err: errors.Join(errors.New("other"), &surfaceMismatchError{delta: []string{"+ a|b|c|d`e 0"}}), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := filepath.Join(t.TempDir(), "summary.md")
			written, err := writeApprovalSummary(tc.err, summary)
			if (err != nil) != tc.wantErr || written {
				t.Fatalf("written=%v error=%v, want written=false error %v", written, err, tc.wantErr)
			}
			if _, statErr := os.Stat(summary); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("summary written for %s (stat error %v)", tc.name, statErr)
			}
		})
	}
	if written, err := writeApprovalSummary(&surfaceMismatchError{delta: []string{"+ line"}}, ""); err != nil || written {
		t.Fatalf("outside GitHub Actions: written=%v error=%v", written, err)
	}
}
