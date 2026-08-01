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
	few := deriveCVE(itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 1})))
	many := deriveCVE(itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 999})))

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
	few := deriveCVE(itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 1})))
	many := deriveCVE(itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 999})))

	if few[0].Total != 1 || many[0].Total != 999 {
		t.Fatalf("totals must reflect the real counts, got %d and %d", few[0].Total, many[0].Total)
	}

	var a, b bytes.Buffer
	if err := report(few, &a); err != nil {
		t.Fatalf("report: %v", err)
	}

	if err := report(many, &b); err != nil {
		t.Fatalf("report: %v", err)
	}

	if a.String() == b.String() {
		t.Errorf("1 and 999 criticals must not render identically: %q", a.String())
	}
}

// Totals are summed across workloads, not overwritten by the last one seen.
func TestTotalSumsAcrossWorkloads(t *testing.T) {
	got := deriveCVE(itemsOf(t,
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

// A changing component SET must change the identity — the converse control,
// without which the test above would pass for a constant fingerprint.
func TestFingerprintChangesWhenComponentsChange(t *testing.T) {
	one := mustDerivePosture(t, itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
	), nil)
	two := mustDerivePosture(t, itemsOf(t,
		postureDoc("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureDoc("app", "Deployment", "web", map[string]string{"C-0016": "failed"}),
	), nil)

	if one[0].Fingerprint() == two[0].Fingerprint() {
		t.Error("a different affected-component set must yield a different fingerprint")
	}
}

func TestZeroCountSeveritiesRaiseNoTheme(t *testing.T) {
	got := deriveCVE(itemsOf(t, cveDoc("app", "api", map[string]int{"critical": 0, "high": 0})))
	if len(got) != 0 {
		t.Errorf("all-zero severities must raise no theme, got %+v", got)
	}
}

func TestSeverityCountAcceptsBothSpellings(t *testing.T) {
	if got := severityCount(json.RawMessage(`7`)); got != 7 {
		t.Errorf("bare integer: want 7, got %d", got)
	}

	if got := severityCount(json.RawMessage(`{"all":7}`)); got != 7 {
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

	if strings.Count(out.String(), "\n") != 1 {
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
