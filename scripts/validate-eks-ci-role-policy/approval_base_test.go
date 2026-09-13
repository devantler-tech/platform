package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	digestX = strings.Repeat("1", 64)
	digestA = strings.Repeat("a", 64)
	digestB = strings.Repeat("b", 64)
	digestC = strings.Repeat("c", 64)
)

// approvalSource renders a minimal validator source carrying the approval
// record. An empty previous omits that constant, as on a base that predates it.
func approvalSource(expected string, previous string) []byte {
	var source bytes.Buffer
	source.WriteString("package main\n\n")
	source.WriteString("const expectedRenderedSurfaceSHA = \"" + expected + "\"\n")
	if previous != "" {
		source.WriteString("const previousRenderedSurfaceSHA = \"" + previous + "\"\n")
	}
	return source.Bytes()
}

// The #3740 scenario: two branches re-approve from base X. Branch A merges and
// the base now approves A. Branch B still declares X as the aggregate it
// supersedes, so its approval describes a tree without A's delta.
func TestValidateApprovalBaseRejectsReapprovalAgainstStaleBase(t *testing.T) {
	base := approvalSource(digestA, digestX)
	head := approvalSource(digestB, digestX)

	err := validateApprovalBase(head, base)
	if err == nil {
		t.Fatal("expected a re-approval computed against a stale base to be rejected")
	}
	message := err.Error()
	for _, want := range []string{
		"stale rendered authorization surface approval",
		"supersedes " + digestX,
		"approves " + digestA,
		"re-derive the aggregate against the current base",
		"set previousRenderedSurfaceSHA to " + digestA,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("rejection is missing %q, so it is not failing for the stale base: %v", want, err)
		}
	}
}

// The same approval re-derived against the current base passes.
func TestValidateApprovalBaseAcceptsReapprovalAgainstCurrentBase(t *testing.T) {
	base := approvalSource(digestA, digestX)
	head := approvalSource(digestB, digestA)

	if err := validateApprovalBase(head, base); err != nil {
		t.Fatalf("expected a re-approval against the current base to pass: %v", err)
	}
}

// Negative control: a change that moves nothing inherits both constants from
// its base and must not start failing.
func TestValidateApprovalBaseAcceptsUnmovedSurface(t *testing.T) {
	base := approvalSource(digestA, digestX)

	if err := validateApprovalBase(base, base); err != nil {
		t.Fatalf("expected an unmoved approval to pass: %v", err)
	}
}

// The first change carrying the record merges onto a base without it, and must
// not fail merely because the base predates the constant.
func TestValidateApprovalBaseAcceptsBaseWithoutRecord(t *testing.T) {
	base := approvalSource(digestA, "")

	if err := validateApprovalBase(approvalSource(digestA, digestX), base); err != nil {
		t.Fatalf("expected an unmoved approval over a pre-record base to pass: %v", err)
	}
	if err := validateApprovalBase(approvalSource(digestB, digestA), base); err != nil {
		t.Fatalf("expected a re-approval over a pre-record base to pass: %v", err)
	}
	err := validateApprovalBase(approvalSource(digestB, digestX), base)
	if err == nil || !strings.Contains(err.Error(), "stale rendered authorization surface approval") {
		t.Fatalf("expected a stale re-approval over a pre-record base to be rejected as stale, got: %v", err)
	}
}

// A conflict resolved by keeping the base's aggregate but this branch's
// superseded value leaves a record that describes no approval at all.
func TestValidateApprovalBaseRejectsRecordChangedWithoutMove(t *testing.T) {
	base := approvalSource(digestA, digestX)
	head := approvalSource(digestA, digestC)

	err := validateApprovalBase(head, base)
	if err == nil || !strings.Contains(err.Error(), "changed without moving expectedRenderedSurfaceSHA") {
		t.Fatalf("expected an orphaned previousRenderedSurfaceSHA change to be rejected, got: %v", err)
	}
}

func TestValidateApprovalBaseRejectsMalformedRecords(t *testing.T) {
	valid := approvalSource(digestA, digestX)
	for name, test := range map[string]struct {
		head []byte
		base []byte
		want string
	}{
		"head without record": {
			head: approvalSource(digestB, ""),
			base: valid,
			want: "previousRenderedSurfaceSHA is missing",
		},
		"record supersedes itself": {
			head: approvalSource(digestB, digestB),
			base: valid,
			want: "equals expectedRenderedSurfaceSHA",
		},
		"uppercase digest": {
			head: approvalSource(strings.ToUpper(digestB), digestA),
			base: valid,
			want: "expectedRenderedSurfaceSHA is not a lowercase SHA-256 digest",
		},
		"short previous": {
			head: approvalSource(digestB, digestA[:63]),
			base: valid,
			want: "previousRenderedSurfaceSHA is not a lowercase SHA-256 digest",
		},
		"base without aggregate": {
			head: valid,
			base: []byte("package main\n"),
			want: "base validator source: expectedRenderedSurfaceSHA is missing",
		},
		"malformed base aggregate": {
			head: approvalSource(digestB, digestA),
			base: approvalSource(digestA[:63], digestX),
			want: "base validator source: expectedRenderedSurfaceSHA is not a lowercase SHA-256 digest",
		},
		"implicit iota value": {
			head: []byte("package main\n\nconst (\n\tfirst = iota\n\texpectedRenderedSurfaceSHA\n)\n" +
				"const previousRenderedSurfaceSHA = \"" + digestA + "\"\n"),
			base: valid,
			want: "expectedRenderedSurfaceSHA must be a string literal",
		},
		"non-literal aggregate": {
			head: []byte("package main\n\nconst expectedRenderedSurfaceSHA = other\n" +
				"const previousRenderedSurfaceSHA = \"" + digestA + "\"\n"),
			base: valid,
			want: "must be a string literal",
		},
		"unparseable source": {
			head: []byte("package main\nconst ("),
			base: valid,
			want: "parse head validator source",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateApprovalBase(test.head, test.base)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected rejection containing %q, got: %v", test.want, err)
			}
		})
	}
}

// The committed record must be well-formed, and the parser must read the real
// validator source the way CI will, not only the synthetic fixtures above.
func TestCommittedApprovalRecordMatchesSource(t *testing.T) {
	if err := validateApprovalRecord(expectedRenderedSurfaceSHA, previousRenderedSurfaceSHA); err != nil {
		t.Fatalf("committed approval record is invalid: %v", err)
	}
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read validator source: %v", err)
	}
	approval, err := parseSurfaceApproval(source, "validator source")
	if err != nil {
		t.Fatalf("parse validator source: %v", err)
	}
	if approval.expected != expectedRenderedSurfaceSHA || approval.previous != previousRenderedSurfaceSHA {
		t.Fatalf("parsed record %+v does not match the compiled constants", approval)
	}
	if err := validateApprovalBase(source, source); err != nil {
		t.Fatalf("validator source must pass against itself: %v", err)
	}
}

func TestRunCLIApprovalBase(t *testing.T) {
	directory := t.TempDir()
	write := func(name string, contents []byte) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	base := write("base.go", approvalSource(digestA, digestX))
	current := write("current.go", approvalSource(digestB, digestA))
	stale := write("stale.go", approvalSource(digestB, digestX))

	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"approval-base", base, current}, &stdout, &stderr); code != 0 {
		t.Fatalf("current approval exit %d, stderr: %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"approval-base", base, stale}, &stdout, &stderr); code != 1 {
		t.Fatalf("stale approval exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "re-derive the aggregate against the current base") {
		t.Fatalf("stale approval stderr lacks the fix: %s", stderr.String())
	}

	stderr.Reset()
	if code := runCLI([]string{"approval-base", filepath.Join(directory, "absent.go"), current}, &stdout, &stderr); code != 1 {
		t.Fatalf("unreadable base exit %d, want 1", code)
	}

	stderr.Reset()
	if code := runCLI([]string{"approval-base", base}, &stdout, &stderr); code != 2 {
		t.Fatalf("incomplete approval-base arguments exit %d, want 2", code)
	}
}
