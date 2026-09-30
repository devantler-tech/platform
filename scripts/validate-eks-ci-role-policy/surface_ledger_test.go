package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// deltaFixture is a render whose ClusterRoleBindings the surface always selects.
const deltaFixture = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: alpha
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: view
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: alpha-readers
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: bravo
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: edit
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: bravo-editors
`

// testSurfaceEntry builds one ClusterRole surface entry without going through selection.
func testSurfaceEntry(t *testing.T, name string, verb string) string {
	t.Helper()
	identity := resourceIdentity{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", name: name}
	document := map[string]any{
		"apiVersion": identity.apiVersion,
		"kind":       identity.kind,
		"metadata":   map[string]any{"name": name},
		"rules":      []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{verb}}},
	}
	entry, err := authorizationSurfaceEntry(identity, document)
	if err != nil {
		t.Fatalf("authorizationSurfaceEntry(%s) error = %v", name, err)
	}
	return entry
}

// fixtureLedger evaluates a render and returns its ledger, failing unless both bindings were selected.
func fixtureLedger(t *testing.T, rendered string) []string {
	t.Helper()
	entries, _, _, err := evaluateRenderedSurface([]byte(rendered))
	if err != nil {
		t.Fatalf("evaluateRenderedSurface() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("fixture selected %d surface entries, want 2", len(entries))
	}
	return surfaceLedger(entries)
}

// TestCommittedLedgerParses keeps the embedded approval well formed and sorted.
func TestCommittedLedgerParses(t *testing.T) {
	lines, err := parseSurfaceLedger(approvedSurfaceLedger)
	if err != nil {
		t.Fatalf("parseSurfaceLedger(approved-surface.txt) error = %v", err)
	}
	if len(lines) < 100 {
		t.Fatalf("approved-surface.txt approves %d documents, want the full production surface", len(lines))
	}
}

// TestSurfaceLedgerLineCoversTheWholeEntry proves a content-only change moves the digest and
// nothing but that document's line, so the ledger is exactly as strict as one aggregate digest.
func TestSurfaceLedgerLineCoversTheWholeEntry(t *testing.T) {
	alpha, bravo := testSurfaceEntry(t, "alpha", "get"), testSurfaceEntry(t, "bravo", "get")
	changed := testSurfaceEntry(t, "alpha", "delete")
	before, after := surfaceLedger([]string{alpha, bravo}), surfaceLedger([]string{changed, bravo})
	if before[1] != after[1] {
		t.Fatalf("an unrelated document's line moved: %q -> %q", before[1], after[1])
	}
	if before[0] == after[0] {
		t.Fatal("a changed rule kept the same ledger line")
	}
	if !strings.HasPrefix(after[0], "rbac.authorization.k8s.io/v1|ClusterRole||alpha ") {
		t.Fatalf("ledger line = %q, want the identity first", after[0])
	}
	if !surfaceLedgerLinePattern.MatchString(after[0]) {
		t.Fatalf("ledger line %q does not match the approved line shape", after[0])
	}
}

// TestDescribeLedgerDeltaNamesOnlyTheChangedDocument pairs the approved and rendered line of one identity.
func TestDescribeLedgerDeltaNamesOnlyTheChangedDocument(t *testing.T) {
	alpha, bravo := testSurfaceEntry(t, "alpha", "get"), testSurfaceEntry(t, "bravo", "get")
	changed := testSurfaceEntry(t, "bravo", "delete")
	got := describeLedgerDelta(surfaceLedger([]string{alpha, bravo}), surfaceLedger([]string{alpha, changed}))
	want := []string{"- " + surfaceLedgerLine(bravo), "+ " + surfaceLedgerLine(changed)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("describeLedgerDelta() = %q, want %q", got, want)
	}
}

// TestDescribeLedgerDeltaReportsAddedAndRemovedDocuments keeps additions and removals apart.
func TestDescribeLedgerDeltaReportsAddedAndRemovedDocuments(t *testing.T) {
	alpha, bravo, charlie := testSurfaceEntry(t, "alpha", "get"), testSurfaceEntry(t, "bravo", "get"),
		testSurfaceEntry(t, "charlie", "get")
	got := describeLedgerDelta(surfaceLedger([]string{alpha, bravo}), surfaceLedger([]string{bravo, charlie}))
	want := []string{"- " + surfaceLedgerLine(alpha), "+ " + surfaceLedgerLine(charlie)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("describeLedgerDelta() = %q, want %q", got, want)
	}
}

// TestDescribeLedgerDeltaTreatsADuplicateAsAChange compares lines as a multiset in both directions.
func TestDescribeLedgerDeltaTreatsADuplicateAsAChange(t *testing.T) {
	line := surfaceLedgerLine(testSurfaceEntry(t, "alpha", "get"))
	if got := describeLedgerDelta([]string{line}, []string{line, line}); !reflect.DeepEqual(got, []string{"+ " + line}) {
		t.Fatalf("duplicated render: describeLedgerDelta() = %q", got)
	}
	if got := describeLedgerDelta([]string{line, line}, []string{line}); !reflect.DeepEqual(got, []string{"- " + line}) {
		t.Fatalf("duplicated approval: describeLedgerDelta() = %q", got)
	}
	if got := describeLedgerDelta([]string{line}, []string{line}); len(got) != 0 {
		t.Fatalf("identical ledgers: describeLedgerDelta() = %q, want none", got)
	}
}

// TestParseSurfaceLedgerAcceptsCommentsAndClusterScopedLines reads the documented file shape.
func TestParseSurfaceLedgerAcceptsCommentsAndClusterScopedLines(t *testing.T) {
	digest := strings.Repeat("a", 64)
	text := "# header\n\nv1|Namespace||aws " + digest + "\n\nv1|ServiceAccount|aws|aws " + digest + "\n"
	got, err := parseSurfaceLedger(text)
	if err != nil {
		t.Fatalf("parseSurfaceLedger() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parseSurfaceLedger() = %q, want the two data lines", got)
	}
}

// TestParseSurfaceLedgerRejectsMalformedFiles fails closed on anything but sorted, well-formed lines.
func TestParseSurfaceLedgerRejectsMalformedFiles(t *testing.T) {
	digest := strings.Repeat("a", 64)
	tests := map[string]struct {
		text string
		want string
	}{
		"empty":            {text: "# only a comment\n", want: "approves no authorization surface"},
		"short digest":     {text: "v1|Namespace||aws abc\n", want: "line 1 is not"},
		"uppercase digest": {text: "v1|Namespace||aws " + strings.Repeat("A", 64) + "\n", want: "line 1 is not"},
		"missing name":     {text: "v1|Namespace|| " + digest + "\n", want: "line 1 is not"},
		"extra field":      {text: "v1|Namespace|a|b|c " + digest + "\n", want: "line 1 is not"},
		"conflict marker":  {text: "<<<<<<< HEAD\n", want: "line 1 is not"},
		"trailing space":   {text: "v1|Namespace||aws " + digest + " \n", want: "line 1 is not"},
		"unsorted":         {text: "v1|ServiceAccount|aws|aws " + digest + "\n\nv1|Namespace||aws " + digest + "\n", want: "line 3 is out of order"},
		"no separator":     {text: "v1|Namespace||aws " + digest + "\nv1|ServiceAccount|aws|aws " + digest + "\n", want: "line 2: separate entries"},
		"two separators":   {text: "v1|Namespace||aws " + digest + "\n\n\nv1|ServiceAccount|aws|aws " + digest + "\n", want: "line 3: separate entries"},
		"trailing blank":   {text: "v1|Namespace||aws " + digest + "\n\n", want: "line 2: separate entries"},
		"late comment":     {text: "v1|Namespace||aws " + digest + "\n\n# note\n", want: "line 3 is not"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSurfaceLedger(test.text); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseSurfaceLedger() error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestValidateRenderedMismatchNamesTheLinesToApprove keeps the gate's message and lists every moved line.
func TestValidateRenderedMismatchNamesTheLinesToApprove(t *testing.T) {
	err := validateRendered([]byte(deltaFixture))
	var mismatch *surfaceMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("validateRendered() error = %v, want a surface mismatch", err)
	}
	for _, line := range fixtureLedger(t, deltaFixture) {
		if !strings.Contains(err.Error(), "\n  + "+line) {
			t.Fatalf("validateRendered() error does not ask to approve %q:\n%v", line, err)
		}
	}
	if !strings.HasPrefix(mismatch.Error(), "unapproved rendered authorization surface: ") {
		t.Fatalf("mismatch message = %q, want the gate's prefix", mismatch.Error())
	}
}

// TestIndependentApprovalsMergeLineByLine is the property platform#3182 asks for: two changes to
// different documents each touch only their own line, so applying both approvals yields exactly the
// ledger of the combined render, with no aggregate to re-derive.
func TestIndependentApprovalsMergeLineByLine(t *testing.T) {
	base := fixtureLedger(t, deltaFixture)
	first := fixtureLedger(t, strings.Replace(deltaFixture, "name: view", "name: admin", 1))
	second := fixtureLedger(t, strings.Replace(deltaFixture, "name: edit", "name: view", 1))
	combined := fixtureLedger(t, strings.Replace(strings.Replace(deltaFixture,
		"name: view", "name: admin", 1), "name: edit", "name: view", 1))

	merged := append([]string(nil), base...)
	for index := range base {
		if first[index] != base[index] {
			merged[index] = first[index]
		}
		if second[index] != base[index] {
			if merged[index] != base[index] {
				t.Fatalf("both approvals touched line %d, want disjoint lines", index)
			}
			merged[index] = second[index]
		}
	}
	if delta := describeLedgerDelta(merged, combined); len(delta) != 0 {
		t.Fatalf("merged approvals differ from the combined render: %q", delta)
	}
}

// TestSurfaceEntryKeyToleratesAMalformedEntry keeps a malformed entry from panicking the ledger.
func TestSurfaceEntryKeyToleratesAMalformedEntry(t *testing.T) {
	if got := surfaceEntryKey("no identity fields"); got != "malformed entry" {
		t.Fatalf("surfaceEntryKey() = %q, want malformed entry", got)
	}
}
