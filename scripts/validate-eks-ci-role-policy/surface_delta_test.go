package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// fixtureEntries evaluates a render and fails unless the fixture selected both bindings.
func fixtureEntries(t *testing.T, rendered string) []string {
	t.Helper()
	entries, _, _, err := evaluateRenderedSurface([]byte(rendered))
	if err != nil {
		t.Fatalf("evaluateRenderedSurface() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("fixture selected %d surface entries, want 2", len(entries))
	}
	return entries
}

// approvingSource is a validator source whose approved aggregate matches entries.
func approvingSource(t *testing.T, entries []string) []byte {
	t.Helper()
	canonical, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return []byte("package main\n\nconst expectedRenderedSurfaceSHA = \"" + fingerprint(canonical) + "\"\n")
}

// TestDescribeSurfaceDeltaNamesOnlyTheChangedEntry proves one changed entry is named alone.
func TestDescribeSurfaceDeltaNamesOnlyTheChangedEntry(t *testing.T) {
	base := []string{testSurfaceEntry(t, "a", "get"), testSurfaceEntry(t, "b", "get"), testSurfaceEntry(t, "c", "get")}
	head := []string{base[0], testSurfaceEntry(t, "b", "delete"), base[2]}

	want := []string{"changed rbac.authorization.k8s.io/v1|ClusterRole||b"}
	if got := describeSurfaceDelta(base, head); !reflect.DeepEqual(got, want) {
		t.Fatalf("describeSurfaceDelta() = %q, want %q", got, want)
	}
}

// TestDescribeSurfaceDeltaReportsAddedAndRemovedEntries names additions and removals separately.
func TestDescribeSurfaceDeltaReportsAddedAndRemovedEntries(t *testing.T) {
	base := []string{testSurfaceEntry(t, "a", "get"), testSurfaceEntry(t, "b", "get")}
	head := []string{base[1], testSurfaceEntry(t, "c", "get")}

	want := []string{
		"removed rbac.authorization.k8s.io/v1|ClusterRole||a",
		"added rbac.authorization.k8s.io/v1|ClusterRole||c",
	}
	if got := describeSurfaceDelta(base, head); !reflect.DeepEqual(got, want) {
		t.Fatalf("describeSurfaceDelta() = %q, want %q", got, want)
	}
}

// TestDescribeSurfaceDeltaTreatsADuplicateAsAChange compares duplicate identities as a multiset.
func TestDescribeSurfaceDeltaTreatsADuplicateAsAChange(t *testing.T) {
	entry := testSurfaceEntry(t, "a", "get")

	want := []string{"changed rbac.authorization.k8s.io/v1|ClusterRole||a"}
	if got := describeSurfaceDelta([]string{entry}, []string{entry, entry}); !reflect.DeepEqual(got, want) {
		t.Fatalf("describeSurfaceDelta() = %q, want %q", got, want)
	}
}

// TestDescribeSurfaceMismatchReportsAnUnavailableBaseAsUnknown never implies nothing moved without a base.
func TestDescribeSurfaceMismatchReportsAnUnavailableBaseAsUnknown(t *testing.T) {
	head := []string{testSurfaceEntry(t, "a", "get")}
	for name, lines := range map[string][]string{
		"render failure":    describeSurfaceMismatch(head, nil, errors.New("kustomize failed"), nil, nil),
		"no base root":      surfaceMismatchReport(context.Background(), "", head, nil),
		"unreadable render": describeSurfaceMismatch(head, []byte("{not yaml"), nil, nil, nil),
	} {
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "moved authorization surface entries: unknown (") {
			t.Fatalf("%s: lines = %q, want a single unknown report", name, lines)
		}
	}
}

// TestDescribeSurfaceMismatchSaysNoneOnlyAgainstAVerifiedBase limits "none" to a base that reproduces its approval.
func TestDescribeSurfaceMismatchSaysNoneOnlyAgainstAVerifiedBase(t *testing.T) {
	entries := fixtureEntries(t, deltaFixture)

	got := describeSurfaceMismatch(entries, []byte(deltaFixture), nil, approvingSource(t, entries), nil)
	want := []string{
		"moved authorization surface entries: none; the approval base renders this same surface, so only the approved aggregate differs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("describeSurfaceMismatch() = %q, want %q", got, want)
	}

	for name, lines := range map[string][]string{
		"unreadable base source":  describeSurfaceMismatch(entries, []byte(deltaFixture), nil, nil, errors.New("no such file")),
		"unparseable base source": describeSurfaceMismatch(entries, []byte(deltaFixture), nil, []byte("package main\n"), nil),
	} {
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "moved authorization surface entries: unknown (unverified: ") {
			t.Fatalf("%s: lines = %q, want an unverified unknown report", name, lines)
		}
	}
}

// TestSurfaceEntryKeyToleratesAMalformedEntry keeps a malformed entry from panicking the report.
func TestSurfaceEntryKeyToleratesAMalformedEntry(t *testing.T) {
	if got := surfaceEntryKey("no identity fields"); got != "malformed entry" {
		t.Fatalf("surfaceEntryKey() = %q, want malformed entry", got)
	}
}

// TestDescribeSurfaceMismatchNamesEntriesAgainstAVerifiedBase names entries under the verified header.
func TestDescribeSurfaceMismatchNamesEntriesAgainstAVerifiedBase(t *testing.T) {
	baseEntries := fixtureEntries(t, deltaFixture)
	head := fixtureEntries(t, strings.Replace(deltaFixture, "name: edit", "name: admin", 1))

	got := describeSurfaceMismatch(head, []byte(deltaFixture), nil, approvingSource(t, baseEntries), nil)
	want := []string{
		"moved authorization surface entries (against the approval base):",
		"  changed rbac.authorization.k8s.io/v1|ClusterRoleBinding||bravo",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("describeSurfaceMismatch() = %q, want %q", got, want)
	}
}

// TestDescribeSurfaceMismatchMarksAnUnreproducedBaseUnverified labels a base that does not reproduce its approval.
func TestDescribeSurfaceMismatchMarksAnUnreproducedBaseUnverified(t *testing.T) {
	baseEntries := fixtureEntries(t, deltaFixture)
	head := fixtureEntries(t, strings.Replace(deltaFixture, "name: bravo-editors", "name: bravo-owners", 1))
	otherApproval := approvingSource(t, head)

	got := describeSurfaceMismatch(head, []byte(deltaFixture), nil, otherApproval, nil)
	if len(got) != 2 || !strings.Contains(got[0], "unverified: the approval base renders") ||
		got[1] != "  changed rbac.authorization.k8s.io/v1|ClusterRoleBinding||bravo" {
		t.Fatalf("describeSurfaceMismatch() = %q, want an unverified header naming bravo", got)
	}

	if same := describeSurfaceMismatch(baseEntries, []byte(deltaFixture), nil, otherApproval, nil); len(same) != 1 ||
		!strings.Contains(same[0], ": unknown (unverified") {
		t.Fatalf("an empty delta against an unverified base = %q, want unknown", same)
	}
}

// TestValidateRenderedMismatchCarriesItsEntries keeps the mismatch message and exposes its entries.
func TestValidateRenderedMismatchCarriesItsEntries(t *testing.T) {
	err := validateRendered([]byte(deltaFixture))
	var mismatch *surfaceMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("validateRendered() error = %v, want a surface mismatch", err)
	}
	if !reflect.DeepEqual(mismatch.entries, fixtureEntries(t, deltaFixture)) {
		t.Fatal("surface mismatch does not carry the evaluated entries")
	}
	if !strings.Contains(err.Error(), "unapproved rendered authorization surface fingerprint: "+mismatch.actual) {
		t.Fatalf("validateRendered() error = %v, want the unchanged mismatch message", err)
	}
}

// TestSurfaceMismatchReportRendersTheBaseRoot renders and verifies a base checkout, and reports a failed render as unknown.
func TestSurfaceMismatchReportRendersTheBaseRoot(t *testing.T) {
	baseRoot := t.TempDir()
	source := filepath.Join(baseRoot, validatorSourcePath)
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, approvingSource(t, fixtureEntries(t, deltaFixture)), 0o600); err != nil {
		t.Fatal(err)
	}
	execute := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.HasSuffix(args[len(args)-1], authorizationOverlayPaths[0]) {
			return []byte(deltaFixture), nil
		}
		return []byte("{}\n"), nil
	}
	head := fixtureEntries(t, strings.Replace(deltaFixture, "name: alpha", "name: charlie", 1))

	got := surfaceMismatchReport(context.Background(), baseRoot, head, execute)
	want := []string{
		"moved authorization surface entries (against the approval base):",
		"  removed rbac.authorization.k8s.io/v1|ClusterRoleBinding||alpha",
		"  added rbac.authorization.k8s.io/v1|ClusterRoleBinding||charlie",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("surfaceMismatchReport() = %q, want %q", got, want)
	}

	failing := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("render failed") }
	if lines := surfaceMismatchReport(context.Background(), baseRoot, head, failing); len(lines) != 1 ||
		!strings.Contains(lines[0], ": unknown (approval base unavailable: render") {
		t.Fatalf("failing base render = %q, want unknown", lines)
	}
}
