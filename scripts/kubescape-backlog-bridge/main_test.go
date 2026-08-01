package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Fixtures are written as RAW JSON rather than marshalled from Go structs, so
// they stay byte-faithful to what `kubectl get -o json` actually emits. The
// stripped shapes below were measured against prod on 2026-08-01:
// 2215/2215 posture LIST objects carried `controls: null`, and 121/121 CVE LIST
// objects carried a blanked `vulnerabilitiesRef`.

func labels(ns, kind, name string) string {
	return fmt.Sprintf(`"labels":{"kubescape.io/workload-namespace":%q,`+
		`"kubescape.io/workload-kind":%q,"kubescape.io/workload-name":%q}`, ns, kind, name)
}

// postureDoc builds a hydrated workloadconfigurationscansummary object.
func postureDoc(ns, kind, name string, controls map[string]string) string {
	return postureDocSev(ns, kind, name, controls, "High")
}

func postureDocSev(ns, kind, name string, controls map[string]string, severity string) string {
	ids := make([]string, 0, len(controls))
	for id := range controls {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(
			`%q:{"controlID":%q,"severity":{"severity":%q},"status":{"status":%q}}`,
			id, id, severity, controls[id]))
	}

	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,%s},`+
		`"spec":{"controls":{%s},"severities":{"critical":0,"high":0}}}`,
		name, ns, labels(ns, kind, name), strings.Join(parts, ","))
}

// strippedPostureDoc is the spec-stripped shape a posture LIST returns.
func strippedPostureDoc(ns, kind, name string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,%s},`+
		`"spec":{"controls":null,"severities":{"critical":0,"high":0,"low":0,"medium":0,"unknown":0}}}`,
		name, ns, labels(ns, kind, name))
}

// cveDoc builds a hydrated vulnerabilitymanifestsummary object, using the
// {"all": N} spelling the real objects use and naming a real manifest.
func cveDoc(ns, name string, counts map[string]int) string {
	classes := make([]string, 0, len(counts))
	for class := range counts {
		classes = append(classes, class)
	}

	sort.Strings(classes)

	parts := make([]string, 0, len(classes))
	for _, class := range classes {
		parts = append(parts, fmt.Sprintf(`%q:{"all":%d,"relevant":%d}`, class, counts[class], counts[class]))
	}

	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,%s},`+
		`"spec":{"severities":{%s},`+
		`"vulnerabilitiesRef":{"all":{"kind":"vulnerabilitymanifests","name":"registry-image-%s","namespace":%q},`+
		`"relevant":{"kind":"vulnerabilitymanifests","name":"replicaset-%s","namespace":%q}}}}`,
		name, ns, labels(ns, "Deployment", name), strings.Join(parts, ","), name, ns, name, ns)
}

// strippedCVEDoc is the spec-stripped shape a CVE LIST returns: severities
// present but zero, and the manifest reference blanked.
func strippedCVEDoc(ns, name string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,%s},`+
		`"spec":{"severities":{"critical":{"all":0},"high":{"all":0},"low":{"all":0},`+
		`"medium":{"all":0},"negligible":{"all":0},"unknown":{"all":0}},`+
		`"vulnerabilitiesRef":{"all":{"kind":"","name":"","namespace":""},`+
		`"relevant":{"kind":"","name":"","namespace":""}}}}`,
		name, ns, labels(ns, "Deployment", name))
}

func itemOf(t *testing.T, doc string) item {
	t.Helper()

	var it item
	if err := json.Unmarshal([]byte(doc), &it); err != nil {
		t.Fatalf("fixture does not decode: %v\n%s", err, doc)
	}

	return it
}

func itemsOf(t *testing.T, docs ...string) []item {
	t.Helper()

	out := make([]item, 0, len(docs))
	for _, d := range docs {
		out = append(out, itemOf(t, d))
	}

	return out
}

// writeDocs writes a `kubectl get -A -o json` style list.
func writeDocs(t *testing.T, path string, docs ...string) {
	t.Helper()
	writeRaw(t, path, fmt.Sprintf(`{"items":[%s]}`, strings.Join(docs, ",")))
}

// writeBare writes a single-object `kubectl get <name> -o json` document.
func writeBare(t *testing.T, path, doc string) {
	t.Helper()
	writeRaw(t, path, doc)
}

func writeRaw(t *testing.T, path, raw string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func mustDeriveCVE(t *testing.T, items []item) []theme {
	t.Helper()

	got, err := deriveCVE(items)
	if err != nil {
		t.Fatalf("deriveCVE: %v", err)
	}

	return got
}

func mustDerivePosture(t *testing.T, items []item, exceptions []exception) []theme {
	t.Helper()

	got, err := derivePosture(items, exceptions)
	if err != nil {
		t.Fatalf("derivePosture: %v", err)
	}

	return got
}

func TestOnlyFailedControlsBecomeThemes(t *testing.T) {
	items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{
		"C-0016": "failed",
		"C-0266": "passed",
		"C-0034": "skipped",
	}))

	got := mustDerivePosture(t, items, nil)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 theme (the failed control), got %d: %+v", len(got), got)
	}

	if got[0].Key != "C-0016" {
		t.Errorf("want the failed control C-0016, got %q", got[0].Key)
	}
}

func TestThemesGroupAcrossResourcesNotPerResource(t *testing.T) {
	items := itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureDoc("app", "Deployment", "web", map[string]string{"C-0016": "failed"}),
		postureDoc("data", "StatefulSet", "db", map[string]string{"C-0016": "failed"}),
	)

	got := mustDerivePosture(t, items, nil)
	if len(got) != 1 {
		t.Fatalf("3 resources failing ONE control must yield 1 theme, got %d", len(got))
	}

	if got[0].Count != 3 {
		t.Errorf("want 3 affected components, got %d (%v)", got[0].Count, got[0].Components)
	}
}

// The anti-churn guarantee: identical state must re-derive an identical
// fingerprint. Input order is shuffled to prove the result does not depend on
// map iteration or slice order.
func TestFingerprintIsStableAcrossRunsAndInputOrder(t *testing.T) {
	base := itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureDoc("app", "Deployment", "web", map[string]string{"C-0016": "failed"}),
		postureDoc("data", "StatefulSet", "db", map[string]string{"C-0016": "failed"}),
	)
	want := mustDerivePosture(t, base, nil)[0].Fingerprint()

	rng := rand.New(rand.NewSource(1))

	for i := range 50 {
		shuffled := append([]item(nil), base...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })

		got := mustDerivePosture(t, shuffled, nil)[0].Fingerprint()
		if got != want {
			t.Fatalf("shuffle %d changed the fingerprint: want %s got %s", i, want, got)
		}
	}
}

// A changing severity COUNT must update the existing theme, not mint a new
// identity — otherwise every scan re-files.
func TestFingerprintIgnoresCounts(t *testing.T) {
	few := mustDeriveCVE(t, itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 1})))
	many := mustDeriveCVE(t, itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 999})))

	if len(few) != 1 || len(many) != 1 {
		t.Fatalf("want one theme each, got %d and %d", len(few), len(many))
	}

	if few[0].Fingerprint() != many[0].Fingerprint() {
		t.Errorf("count change must not alter the fingerprint: %s vs %s",
			few[0].Fingerprint(), many[0].Fingerprint())
	}
}

// The counterpart to the rule above: excluding the count from the IDENTITY is
// correct, but the count must still be carried, or an update to an existing
// entry cannot show 1 critical becoming 999 and the update is pointless.
func TestTotalIsAccumulatedEvenThoughItIsNotFingerprinted(t *testing.T) {
	few := mustDeriveCVE(t, itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 1})))
	many := mustDeriveCVE(t, itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 999})))

	if few[0].Total != 1 || many[0].Total != 999 {
		t.Fatalf("totals must reflect the real counts, got %d and %d", few[0].Total, many[0].Total)
	}

	var a, b bytes.Buffer
	if err := report(few, []surface{surfaceCVE}, true, &a); err != nil {
		t.Fatalf("report: %v", err)
	}

	if err := report(many, []surface{surfaceCVE}, true, &b); err != nil {
		t.Fatalf("report: %v", err)
	}

	if a.String() == b.String() {
		t.Errorf("1 and 999 criticals must not render identically: %q", a.String())
	}
}

// Totals are summed across workloads, not overwritten by the last one seen.
func TestTotalSumsAcrossWorkloads(t *testing.T) {
	got := mustDeriveCVE(t, itemsOf(t,
		cveDoc("app", "api", map[string]int{"critical": 2}),
		cveDoc("app", "web", map[string]int{"critical": 5}),
	))
	if len(got) != 1 {
		t.Fatalf("want 1 theme, got %d", len(got))
	}

	if got[0].Total != 7 {
		t.Errorf("want the summed total 7, got %d", got[0].Total)
	}
}

// A changing component SET must NOT change the identity. Which workloads
// exhibit a theme is mutable state about it, not part of what it IS — so a
// workload joining or leaving updates the existing entry instead of minting a
// new one and stranding the old.
func TestFingerprintIsStableWhenTheComponentSetChanges(t *testing.T) {
	one := mustDerivePosture(t, itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
	), nil)
	two := mustDerivePosture(t, itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureDoc("app", "Deployment", "web", map[string]string{"C-0016": "failed"}),
	), nil)

	if one[0].Fingerprint() != two[0].Fingerprint() {
		t.Errorf("a workload joining a theme must not change its identity: %s vs %s",
			one[0].Fingerprint(), two[0].Fingerprint())
	}

	if one[0].Count == two[0].Count {
		t.Error("guard: the component sets must actually differ, or the test is vacuous")
	}
}

// The converse control: a DIFFERENT theme must have a different identity, without
// which the test above would pass for a constant fingerprint.
func TestFingerprintDiffersByKeyAndSurface(t *testing.T) {
	a := mustDerivePosture(t, itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
	), nil)
	b := mustDerivePosture(t, itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0087": "failed"}),
	), nil)

	if a[0].Fingerprint() == b[0].Fingerprint() {
		t.Error("different controls must have different identities")
	}

	// Assert the theme EXISTS rather than guarding on it: `len(cve) > 0 &&`
	// makes the comparison skip silently if derivation ever returns nothing,
	// which is the vacuity this control exists to rule out.
	cve := mustDeriveCVE(t, itemsOf(t, cveDoc("app", "api", map[string]int{"C-0016": 1})))
	if len(cve) != 1 {
		t.Fatalf("want exactly 1 cve theme to compare against, got %d", len(cve))
	}

	if cve[0].Fingerprint() == a[0].Fingerprint() {
		t.Error("the same key on a different surface must have a different identity")
	}
}

func TestZeroCountSeveritiesRaiseNoTheme(t *testing.T) {
	got := mustDeriveCVE(t, itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 0, "high": 0})))
	if len(got) != 0 {
		t.Errorf("all-zero severities must raise no theme, got %+v", got)
	}
}

func TestSeverityCountAcceptsBothSpellings(t *testing.T) {
	if got, ok := severityCount(json.RawMessage(`7`)); !ok || got != 7 {
		t.Errorf("bare integer: want 7, got %d", got)
	}

	if got, ok := severityCount(json.RawMessage(`{"all":7}`)); !ok || got != 7 {
		t.Errorf(`{"all":N} form: want 7, got %d`, got)
	}
}

// The stripped-skeleton guard, at the size where the previous SIZE heuristic
// silently gave up: a single stripped object. `critical: 0` here is not a clean
// cluster, it is an unread one, and the two must not be conflated.
func TestSingleStrippedCVEObjectFailsClosed(t *testing.T) {
	err := checkExamined(itemsOf(t, strippedCVEDoc("app", "api")))
	if err == nil {
		t.Fatal("a stripped object must fail closed regardless of input size")
	}

	if !errors.Is(err, errStrippedList) {
		t.Errorf("want errStrippedList, got %v", err)
	}
}

func TestSingleStrippedPostureObjectFailsClosed(t *testing.T) {
	err := checkExamined(itemsOf(t, strippedPostureDoc("app", "Deployment", "api")))
	if err == nil {
		t.Fatal("a posture object with `controls: null` must fail closed")
	}

	if !errors.Is(err, errStrippedList) {
		t.Errorf("want errStrippedList, got %v", err)
	}
}

// The control that makes the two tests above meaningful: an EXAMINED object
// with nothing to report must pass. Measured on prod, a genuinely clean image
// still names its manifest and a genuinely compliant workload still carries its
// control map — so "examined and empty" is distinguishable from "never read".
func TestExaminedButEmptyInputPasses(t *testing.T) {
	if err := checkExamined(itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 0}))); err != nil {
		t.Errorf("a clean-but-examined CVE object must pass, got %v", err)
	}

	allPassed := postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "passed"})
	if err := checkExamined(itemsOf(t, allPassed)); err != nil {
		t.Errorf("an all-passed posture object must pass, got %v", err)
	}
}

// A posture object carrying an empty-but-present control map is an answer, not
// a silence.
func TestEmptyControlMapIsExaminedNotStripped(t *testing.T) {
	doc := `{"metadata":{"name":"api","namespace":"app"},"spec":{"controls":{},"severities":{}}}`
	if err := checkExamined(itemsOf(t, doc)); err != nil {
		t.Errorf("`controls: {}` is examined-and-empty, must pass, got %v", err)
	}
}

// One stripped object among hydrated ones still fails closed: it contributes
// silent zeros to the derived themes.
func TestOneStrippedObjectAmongHydratedOnesFailsClosed(t *testing.T) {
	items := itemsOf(t,
		cveDoc("app", "api", map[string]int{"critical": 3}),
		strippedCVEDoc("app", "web"),
	)
	if err := checkExamined(items); err == nil {
		t.Error("a stripped object must fail closed even alongside hydrated ones")
	}
}

// Sanitization: the rendered component carries namespace/kind/name and none of
// the reachability evidence that must stay out of a public issue.
func TestComponentCarriesOnlyTheSanitizedMinimum(t *testing.T) {
	it := itemOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}))

	got := it.component().String()
	if got != "app/Deployment/api" {
		t.Fatalf("want app/Deployment/api, got %q", got)
	}

	for _, leak := range []string{"10.244.", "wlid://", "sha256:", "prod-worker"} {
		if strings.Contains(got, leak) {
			t.Errorf("component leaked %q: %s", leak, got)
		}
	}
}

// Feature-flag-first: BOTH states are exercised. Report is the default-on
// half; write is refused while the flag is off.
func TestModeReportIsTheDefaultAndProducesOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posture.json")
	writeDocs(t, path, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}))

	var out bytes.Buffer
	if err := run([]string{"-posture", path}, &out); err != nil {
		t.Fatalf("report mode must succeed, got %v", err)
	}

	if !strings.Contains(out.String(), "C-0016") {
		t.Errorf("report must name the control, got %q", out.String())
	}
}

func TestModeWriteIsRefusedWhileTheFlagIsOff(t *testing.T) {
	var out bytes.Buffer

	err := run([]string{"-mode", "write"}, &out)
	if err == nil {
		t.Fatal("write mode must be refused in this slice")
	}

	if !errors.Is(err, errWritesNotEnabled) {
		t.Errorf("want errWritesNotEnabled, got %v", err)
	}
}

// Two consecutive runs on unchanged state must be byte-identical. This is the
// issue's stated acceptance check, asserted directly.
func TestConsecutiveRunsOnUnchangedStateAreByteIdentical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posture.json")
	writeDocs(t, path,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureDoc("data", "StatefulSet", "db", map[string]string{"C-0087": "failed"}),
	)

	var first, second bytes.Buffer
	if err := run([]string{"-posture", path}, &first); err != nil {
		t.Fatalf("first run: %v", err)
	}

	if err := run([]string{"-posture", path}, &second); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if first.String() != second.String() {
		t.Errorf("consecutive runs differ:\n1: %q\n2: %q", first.String(), second.String())
	}

	if first.Len() == 0 {
		t.Error("guard: the comparison above is vacuous if both runs are empty")
	}
}

// One control can be reported at different severities by different workloads.
// The theme must keep the HIGHEST, independently of input order.
func TestThemeSeverityIsTheHighestAndOrderIndependent(t *testing.T) {
	low := postureDocSev("app", "Deployment", "a", map[string]string{"C-0016": "failed"}, "Low")
	high := postureDocSev("app", "Deployment", "b", map[string]string{"C-0016": "failed"}, "Critical")

	for _, order := range [][]string{{low, high}, {high, low}} {
		got := mustDerivePosture(t, itemsOf(t, order...), nil)
		if len(got) != 1 {
			t.Fatalf("want 1 theme, got %d", len(got))
		}

		if got[0].Severity != "Critical" {
			t.Errorf("want the highest severity (Critical), got %q", got[0].Severity)
		}
	}
}

func TestSeverityRankPutsUnknownLowest(t *testing.T) {
	if severityRank("some-future-severity") >= severityRank("Negligible") {
		t.Error("an unrecognised severity must not outrank a known one")
	}

	if severityRank("Critical") <= severityRank("High") {
		t.Error("Critical must outrank High")
	}
}

// An invocation with no input must not answer "nothing to file".
func TestNoInputIsAnErrorNotACleanBillOfHealth(t *testing.T) {
	var out bytes.Buffer

	err := run(nil, &out)
	if err == nil {
		t.Fatal("an empty invocation must be an error, not a clean report")
	}

	if strings.Contains(out.String(), "nothing to file") {
		t.Errorf("must not print an all-clear for missing input, got %q", out.String())
	}
}

// A per-object `kubectl get <crd> <name> -n <ns> -o json` emits a BARE object
// with no "items" key — exactly what this command's own documentation tells the
// operator to produce.
func TestBareSingleObjectInputIsParsedNotSilentlyEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bare.json")
	writeBare(t, path, cveDoc("app", "api", map[string]int{"critical": 18}))

	var out bytes.Buffer
	if err := run([]string{"-cve", path}, &out); err != nil {
		t.Fatalf("a bare per-object document must be accepted, got %v", err)
	}

	if strings.Contains(out.String(), "nothing to file") {
		t.Fatalf("bare object with critical=18 reported an all-clear: %q", out.String())
	}

	if !strings.Contains(out.String(), "critical") {
		t.Errorf("want the critical theme, got %q", out.String())
	}
}

func TestUnrecognisedDocumentShapeIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.json")
	writeRaw(t, path, `{"unrelated":true}`)

	var out bytes.Buffer

	err := run([]string{"-cve", path}, &out)
	if err == nil {
		t.Fatal("an unrecognised document shape must be rejected, not reported clean")
	}

	if strings.Contains(out.String(), "nothing to file") {
		t.Errorf("must not print an all-clear for junk input, got %q", out.String())
	}
}

func TestExplicitlyEmptyListIsAcceptedAsGenuinelyEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	writeRaw(t, path, `{"items":[]}`)

	var out bytes.Buffer
	if err := run([]string{"-cve", path}, &out); err != nil {
		t.Errorf("an explicitly empty list must be accepted, got %v", err)
	}
}

// A document handed to the wrong surface must be an error. Before this, a CVE
// file passed to -posture satisfied every guard, derived no controls, and
// exited 0 with "nothing to file" — a false all-clear reached by a typo.
func TestCVEDocumentPassedToPostureIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cve.json")
	writeBare(t, path, cveDoc("app", "api", map[string]int{"critical": 18}))

	var out bytes.Buffer

	err := run([]string{"-posture", path}, &out)
	if err == nil {
		t.Fatal("a CVE document passed to -posture must be rejected")
	}

	if !errors.Is(err, errSurfaceMismatch) {
		t.Errorf("want errSurfaceMismatch, got %v", err)
	}

	if strings.Contains(out.String(), "nothing to file") {
		t.Errorf("must not print an all-clear for a swapped file, got %q", out.String())
	}
}

func TestPostureDocumentPassedToCVEIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posture.json")
	writeBare(t, path, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}))

	var out bytes.Buffer

	err := run([]string{"-cve", path}, &out)
	if err == nil {
		t.Fatal("a posture document passed to -cve must be rejected")
	}

	if !errors.Is(err, errSurfaceMismatch) {
		t.Errorf("want errSurfaceMismatch, got %v", err)
	}
}

// A cluster-wide run produces one per-object GET per workload, so the surface
// flags must be repeatable. A single-valued flag made the promised cluster-wide
// grouping unreachable: the second -posture silently replaced the first.
func TestSurfaceFlagsAreRepeatableAndAllInputsAreRead(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	writeBare(t, a, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}))
	writeBare(t, b, postureDoc("data", "StatefulSet", "db", map[string]string{"C-0016": "failed"}))

	var out bytes.Buffer
	if err := run([]string{"-posture", a, "-posture", b}, &out); err != nil {
		t.Fatalf("repeated -posture must be accepted, got %v", err)
	}

	for _, want := range []string{"app/Deployment/api", "data/StatefulSet/db"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("every repeated input must be read; %q missing from %q", want, out.String())
		}
	}

	if themeLines(out.String()) != 1 {
		t.Errorf("both workloads failing one control must group into ONE theme, got %q", out.String())
	}
}

func TestRepeatedCVEFlagsAreAllRead(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	writeBare(t, a, cveDoc("app", "api", map[string]int{"critical": 2}))
	writeBare(t, b, cveDoc("data", "db", map[string]int{"critical": 5}))

	var out bytes.Buffer
	if err := run([]string{"-cve", a, "-cve", b}, &out); err != nil {
		t.Fatalf("repeated -cve must be accepted, got %v", err)
	}

	if !strings.Contains(out.String(), "total=7") {
		t.Errorf("want the summed total across both inputs, got %q", out.String())
	}
}

// `items: null` decodes into a nil slice WITHOUT error, so every guard passes
// vacuously and the command reports a clean cluster at exit 0 — the same false
// all-clear as the stripped skeleton, reached by a different shape. An
// explicitly empty list is `items: []`, and only that stays a legitimate pass.
func TestItemsNullIsRejectedNotTreatedAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "null.json")
	writeRaw(t, path, `{"items":null}`)

	for _, flag := range []string{"-cve", "-posture"} {
		var out bytes.Buffer

		err := run([]string{flag, path}, &out)
		if err == nil {
			t.Fatalf("%s: `items: null` must be rejected, not reported clean", flag)
		}

		if !errors.Is(err, errUnrecognisedDocument) {
			t.Errorf("%s: want errUnrecognisedDocument, got %v", flag, err)
		}

		if strings.Contains(out.String(), "nothing to file") {
			t.Errorf("%s: must not print an all-clear, got %q", flag, out.String())
		}
	}
}

// A bare object whose spec is null carries no payload structure and must not be
// credited either.
func TestSpecNullIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "specnull.json")
	writeRaw(t, path, `{"metadata":{"name":"api","namespace":"app"},"spec":null}`)

	var out bytes.Buffer

	err := run([]string{"-cve", path}, &out)
	if err == nil {
		t.Fatal("`spec: null` must be rejected, not reported clean")
	}

	if strings.Contains(out.String(), "nothing to file") {
		t.Errorf("must not print an all-clear, got %q", out.String())
	}
}

// clusterScopedDoc is the shape a cluster-scoped object really has: the
// workload-namespace label is ABSENT (not empty), while the summary CR itself is
// stored in the scanner's namespace. Measured on the live cluster — all 407
// posture summaries lacking that label were cluster-scoped kinds.
func clusterScopedDoc(kind, name, storedIn string, controls map[string]string) string {
	ids := make([]string, 0, len(controls))
	for id := range controls {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(
			`%q:{"controlID":%q,"severity":{"severity":"High"},"status":{"status":%q}}`,
			id, id, controls[id]))
	}

	return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,`+
		`"labels":{"kubescape.io/workload-kind":%q,"kubescape.io/workload-name":%q}},`+
		`"spec":{"controls":{%s},"severities":{"critical":0}}}`,
		name, storedIn, kind, name, strings.Join(parts, ","))
}

// A cluster-scoped object has no namespace. Taking the summary CR's own
// metadata.namespace invents one — the scanner's — and a namespace-scoped
// exception then matches it.
func TestClusterScopedComponentHasNoFabricatedNamespace(t *testing.T) {
	it := itemOf(t, clusterScopedDoc("ClusterRoleBinding", "some-binding", "kubescape",
		map[string]string{"C-0016": "failed"}))

	got := it.component()
	if got.Namespace != "" {
		t.Errorf("a cluster-scoped object must have no namespace, got %q", got.Namespace)
	}

	if strings.Contains(got.String(), "kubescape") {
		t.Errorf("the scanner's storage namespace must not appear in the component: %s", got)
	}
}

// The consequence, end to end: a namespace-only ClusterSecurityException must
// NOT suppress a cluster-scoped finding. The live `controller-rbac` policy is
// namespace-only and lists `kubescape`, so fabricating that namespace hid real
// cluster-scoped RBAC findings.
func TestNamespaceOnlyExceptionDoesNotSuppressClusterScopedFindings(t *testing.T) {
	exceptions := loadFixture(t, exceptionsDoc(
		policyDoc("controller-rbac", []string{"C-0016"}, `{"namespace":"^(flux-system|kubescape)$"}`)))

	items := itemsOf(t, clusterScopedDoc("ClusterRoleBinding", "some-binding", "kubescape",
		map[string]string{"C-0016": "failed"}))

	got := mustDerivePosture(t, items, exceptions)
	if len(got) != 1 {
		t.Fatalf("a cluster-scoped finding must survive a namespace-only exception, got %+v", got)
	}

	// Control: the same exception MUST still suppress a genuinely namespaced
	// workload in one of the listed namespaces, or this test would pass simply
	// because exceptions stopped working.
	nsScoped := mustDerivePosture(t, itemsOf(t,
		postureDoc("flux-system", "Deployment", "controller", map[string]string{"C-0016": "failed"}),
	), exceptions)
	if len(nsScoped) != 0 {
		t.Errorf("control: the namespaced workload must still be excepted, got %+v", nsScoped)
	}
}

// A severity count in an unrecognised shape must be an error, not zero: zero
// makes corrupt scanner output indistinguishable from a clean result.
func TestSeverityCountRejectsUnrecognisedShapes(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"foo":1}`, `"7"`, `{"all":null}`, `[]`} {
		if got, ok := severityCount(json.RawMessage(raw)); ok {
			t.Errorf("%s must be rejected, got %d ok=true", raw, got)
		}
	}
}

func TestUnrecognisedSeverityShapeFailsTheRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	writeRaw(t, path, `{"metadata":{"name":"api","namespace":"app"},`+
		`"spec":{"severities":{"critical":{"foo":1}},`+
		`"vulnerabilitiesRef":{"all":{"kind":"vulnerabilitymanifests","name":"img","namespace":"app"}}}}`)

	var out bytes.Buffer

	err := run([]string{"-cve", path}, &out)
	if err == nil {
		t.Fatal("an unrecognised severity shape must fail the run, not report clean")
	}

	if strings.Contains(out.String(), "nothing to file") {
		t.Errorf("must not print an all-clear, got %q", out.String())
	}
}

// A clean report must say WHICH surfaces it examined. Each surface flag is
// optional, so a bare "no live-only findings" claims a clean bill of health for
// a surface this invocation never looked at.
func TestAllClearNamesOnlyTheExaminedSurfaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posture.json")
	writeBare(t, path, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "passed"}))

	var out bytes.Buffer
	if err := run([]string{"-posture", path}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out.String(), "posture") {
		t.Errorf("the all-clear must name the examined surface, got %q", out.String())
	}

	if strings.Contains(out.String(), "cve") {
		t.Errorf("must not claim anything about the unexamined cve surface, got %q", out.String())
	}
}

// themeLines counts rendered theme rows, ignoring advisory lines such as the
// unfiltered-report note. The point of the assertion is how many THEMES were
// emitted, which a raw newline count conflates with commentary.
func themeLines(out string) int {
	n := 0

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "\t"+string(surfacePosture)+"\t") ||
			strings.Contains(line, "\t"+string(surfaceCVE)+"\t") {
			n++
		}
	}

	return n
}

// A posture report derived WITHOUT the declared exceptions includes controls the
// platform has already accepted. That is a legitimate view, but it must announce
// itself so it is not mistaken for a drainable backlog.
func TestUnfilteredPostureReportSaysSo(t *testing.T) {
	dir := t.TempDir()
	posture := filepath.Join(dir, "posture.json")
	writeBare(t, posture, postureDoc("app", "Deployment", "api", map[string]string{"C-0036": "failed"}))

	var without bytes.Buffer
	if err := run([]string{"-posture", posture}, &without); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(without.String(), "-exceptions") {
		t.Errorf("an unfiltered posture report must say the exceptions were not applied, got %q", without.String())
	}

	// Control: supplying exceptions removes the note.
	exceptions := filepath.Join(dir, "exceptions.json")
	writeRaw(t, exceptions, exceptionsDoc(policyDoc("other", []string{"C-0999"}, `{"kind":".*"}`)))

	var with bytes.Buffer
	if err := run([]string{"-posture", posture, "-exceptions", exceptions}, &with); err != nil {
		t.Fatalf("run with exceptions: %v", err)
	}

	if strings.Contains(with.String(), "were NOT applied") {
		t.Errorf("a filtered report must not carry the note, got %q", with.String())
	}

	if themeLines(with.String()) != 1 {
		t.Errorf("control: the finding itself must still be reported, got %q", with.String())
	}
}

// A CVE-only run carries no posture note — the exceptions artifact is posture-only.
func TestCVEOnlyRunHasNoExceptionsNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cve.json")
	writeBare(t, path, cveDoc("app", "api", map[string]int{"critical": 3}))

	var out bytes.Buffer
	if err := run([]string{"-cve", path}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if strings.Contains(out.String(), "were NOT applied") {
		t.Errorf("a cve-only run must not carry the posture-exceptions note, got %q", out.String())
	}
}

// A control whose status is missing or unrecognised must fail closed. Treating
// "not failed" as "fine" silently absorbs malformed scanner output into a clean
// report — measured live, every real control carries a recognised status.
func TestUnrecognisedControlStatusFailsClosed(t *testing.T) {
	for _, status := range []string{"", "  ", "weird-new-status"} {
		items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": status}))

		_, err := derivePosture(items, nil)
		if err == nil {
			t.Errorf("status %q must fail closed, got no error", status)
		}
	}
}

// The control: every recognised non-failure status is still silently skipped,
// so the guard above does not simply reject everything.
func TestRecognisedNonFailureStatusesAreSkipped(t *testing.T) {
	for _, status := range []string{"passed", "skipped", "irrelevant", "excluded", "ignored", "PASSED"} {
		items := itemsOf(t, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": status}))

		got, err := derivePosture(items, nil)
		if err != nil {
			t.Errorf("status %q must be accepted, got %v", status, err)
		}

		if len(got) != 0 {
			t.Errorf("status %q must raise no theme, got %+v", status, got)
		}
	}
}

// A real per-object CVE response always carries its severity buckets, zero-valued
// when clean. An absent map is malformed input, and iterating it zero times would
// report exactly the clean result it is not.
func TestCVEDocumentWithoutSeverityBucketsIsRejected(t *testing.T) {
	for _, spec := range []string{
		`"spec":{"vulnerabilitiesRef":{"all":{"kind":"vulnerabilitymanifests","name":"img","namespace":"app"}}}`,
		`"spec":{"severities":{},"vulnerabilitiesRef":{"all":{"kind":"vulnerabilitymanifests","name":"img","namespace":"app"}}}`,
	} {
		path := filepath.Join(t.TempDir(), "nobuckets.json")
		writeRaw(t, path, `{"metadata":{"name":"api","namespace":"app"},`+spec+`}`)

		var out bytes.Buffer

		err := run([]string{"-cve", path}, &out)
		if err == nil {
			t.Errorf("a CVE document with no severity buckets must be rejected: %s", spec)
		}

		if strings.Contains(out.String(), "nothing to file") {
			t.Errorf("must not print an all-clear, got %q", out.String())
		}
	}
}

// A framework-scoped exception cannot be honoured: the scan summaries carry no
// framework. Applying it anyway would widen a deliberately narrow exception
// across every framework, so it is refused rather than silently widened.
func TestFrameworkScopedExceptionIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exceptions.json")
	writeRaw(t, path, `[{"name":"fw","policyType":"postureExceptionPolicy","actions":["alertOnly"],`+
		`"resources":[{"designatorType":"Attributes","attributes":{"kind":".*"}}],`+
		`"posturePolicies":[{"controlID":"^C-0016$","frameworkName":"^nsa$"}]}]`)

	_, err := loadExceptions(path)
	if err == nil {
		t.Fatal("a framework-scoped exception must be refused, not applied across every framework")
	}

	if !errors.Is(err, errBadExceptions) {
		t.Errorf("want errBadExceptions, got %v", err)
	}
}

// The control: an unscoped policy — the shape every current CSE uses — still loads.
func TestUnscopedExceptionStillLoads(t *testing.T) {
	got := loadFixture(t, exceptionsDoc(policyDoc("ok", []string{"C-0016"}, `{"kind":".*"}`)))
	if len(got) != 1 {
		t.Fatalf("an unscoped policy must still load, got %d", len(got))
	}
}

// The clean report must not claim anything about the cluster — this command has
// no inventory, so it can only speak for the objects it was handed.
func TestAllClearDoesNotClaimClusterWideCoverage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posture.json")
	writeBare(t, path, postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "passed"}))

	var out bytes.Buffer
	if err := run([]string{"-posture", path, "-exceptions", filepath.Join(t.TempDir(), "none")}, &out); err == nil {
		t.Fatal("guard: a missing exceptions file must error, keeping this test honest")
	}

	out.Reset()

	if err := run([]string{"-posture", path}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Key on text unique to the all-clear line. Asserting on "supplied" alone
	// was VACUOUS: the unfiltered-exceptions note also contains that word, so
	// the assertion passed through unrelated output (caught by ablation).
	if !strings.Contains(out.String(), "only the objects passed on the command line") {
		t.Errorf("the all-clear must scope itself to the supplied inputs, got %q", out.String())
	}

	if strings.Contains(out.String(), "surface(s) — nothing to file") {
		t.Errorf("the all-clear must not claim whole-surface coverage, got %q", out.String())
	}
}

// A real namespace may legitimately be called "cluster", so the cluster-scope
// marker must not be a bare word a namespace could equal — otherwise two
// distinct components render identically.
func TestNamespaceNamedClusterDoesNotCollideWithClusterScope(t *testing.T) {
	namespaced := component{Namespace: "cluster", Kind: "Deployment", Name: "api"}
	clusterScoped := component{Kind: "Deployment", Name: "api"}

	if namespaced.String() == clusterScoped.String() {
		t.Errorf("a namespace called %q must not render as cluster scope: both are %q",
			"cluster", namespaced.String())
	}
}

// A negative severity count is not a finding count, it is malformed input.
func TestNegativeSeverityCountIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "neg.json")
	writeRaw(t, path, `{"metadata":{"name":"api","namespace":"app"},`+
		`"spec":{"severities":{"critical":{"all":-3}},`+
		`"vulnerabilitiesRef":{"all":{"kind":"vulnerabilitymanifests","name":"img","namespace":"app"}}}}`)

	var out bytes.Buffer

	err := run([]string{"-cve", path}, &out)
	if err == nil {
		t.Fatal("a negative severity count must be rejected")
	}

	if strings.Contains(out.String(), "nothing to file") {
		t.Errorf("must not print an all-clear, got %q", out.String())
	}
}
