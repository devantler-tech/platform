package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// postureItem builds a hydrated workloadconfigurationscansummary-shaped object.
func postureItem(ns, kind, name string, controls map[string]string) item {
	var it item
	it.Metadata.Name = name
	it.Metadata.Namespace = ns
	it.Metadata.Labels = map[string]string{
		"kubescape.io/workload-namespace": ns,
		"kubescape.io/workload-kind":      kind,
		"kubescape.io/workload-name":      name,
	}
	it.Spec.Controls = map[string]struct {
		ControlID string `json:"controlID"`
		Severity  struct {
			Severity string `json:"severity"`
		} `json:"severity"`
		Status struct {
			Status string `json:"status"`
		} `json:"status"`
	}{}
	for id, status := range controls {
		c := it.Spec.Controls[id]
		c.ControlID = id
		c.Severity.Severity = "High"
		c.Status.Status = status
		it.Spec.Controls[id] = c
	}
	return it
}

// cveItem builds a hydrated vulnerabilitymanifestsummary-shaped object, using
// the {"all": N} spelling the real objects use.
func cveItem(ns, name string, counts map[string]int) item {
	var it item
	it.Metadata.Name = name
	it.Metadata.Namespace = ns
	it.Spec.Severities = map[string]json.RawMessage{}
	for class, n := range counts {
		it.Spec.Severities[class] = json.RawMessage(fmt.Sprintf(`{"all":%d}`, n))
	}
	return it
}

func TestOnlyFailedControlsBecomeThemes(t *testing.T) {
	items := []item{
		postureItem("app", "Deployment", "api", map[string]string{
			"C-0016": "failed",
			"C-0266": "passed",
			"C-0034": "skipped",
		}),
	}
	got := derivePosture(items)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 theme (the failed control), got %d: %+v", len(got), got)
	}
	if got[0].Key != "C-0016" {
		t.Errorf("want the failed control C-0016, got %q", got[0].Key)
	}
}

func TestThemesGroupAcrossResourcesNotPerResource(t *testing.T) {
	items := []item{
		postureItem("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureItem("app", "Deployment", "web", map[string]string{"C-0016": "failed"}),
		postureItem("data", "StatefulSet", "db", map[string]string{"C-0016": "failed"}),
	}
	got := derivePosture(items)
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
	base := []item{
		postureItem("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureItem("app", "Deployment", "web", map[string]string{"C-0016": "failed"}),
		postureItem("data", "StatefulSet", "db", map[string]string{"C-0016": "failed"}),
	}
	want := derivePosture(base)[0].Fingerprint()

	rng := rand.New(rand.NewSource(1))
	for i := range 50 {
		shuffled := append([]item(nil), base...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got := derivePosture(shuffled)[0].Fingerprint()
		if got != want {
			t.Fatalf("shuffle %d changed the fingerprint: want %s got %s", i, want, got)
		}
	}
}

// A changing severity COUNT must update the existing theme, not mint a new
// identity — otherwise every scan re-files.
func TestFingerprintIgnoresCounts(t *testing.T) {
	few := []item{cveItem("app", "api", map[string]int{"critical": 1})}
	many := []item{cveItem("app", "api", map[string]int{"critical": 999})}

	a, b := deriveCVE(few), deriveCVE(many)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want one theme each, got %d and %d", len(a), len(b))
	}
	if a[0].Fingerprint() != b[0].Fingerprint() {
		t.Errorf("count change must not alter the fingerprint: %s vs %s",
			a[0].Fingerprint(), b[0].Fingerprint())
	}
}

// A changing component SET must change the identity — the converse control,
// without which the test above would pass for a constant fingerprint.
func TestFingerprintChangesWhenComponentsChange(t *testing.T) {
	one := derivePosture([]item{
		postureItem("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
	})
	two := derivePosture([]item{
		postureItem("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureItem("app", "Deployment", "web", map[string]string{"C-0016": "failed"}),
	})
	if one[0].Fingerprint() == two[0].Fingerprint() {
		t.Error("a different affected-component set must yield a different fingerprint")
	}
}

func TestZeroCountSeveritiesRaiseNoTheme(t *testing.T) {
	got := deriveCVE([]item{cveItem("app", "api", map[string]int{"critical": 0, "high": 0})})
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

// The stripped-LIST guard: the shape measured on prod, where every object
// reads empty. It must be an error, never an all-clear.
func TestStrippedListInputFailsClosed(t *testing.T) {
	var items []item
	for i := range defaultMinHydrationSample {
		items = append(items, cveItem("app", fmt.Sprintf("w%d", i), map[string]int{"critical": 0}))
	}
	err := checkHydration(items, defaultMinHydrationSample)
	if err == nil {
		t.Fatal("an entirely-empty large input must fail closed, not report a clean cluster")
	}
	if !errors.Is(err, errStrippedList) {
		t.Errorf("want errStrippedList, got %v", err)
	}
}

// The guard must not fire when any object is hydrated, or the bridge would
// refuse real input.
func TestHydratedInputPassesTheGuard(t *testing.T) {
	var items []item
	for i := range defaultMinHydrationSample {
		items = append(items, cveItem("app", fmt.Sprintf("w%d", i), map[string]int{"critical": 0}))
	}
	items = append(items, cveItem("app", "real", map[string]int{"critical": 3}))
	if err := checkHydration(items, defaultMinHydrationSample); err != nil {
		t.Errorf("input containing a hydrated object must pass, got %v", err)
	}
}

// A genuinely small, genuinely clean input is not the stripped shape.
func TestSmallEmptyInputIsNotTreatedAsStripped(t *testing.T) {
	items := []item{cveItem("app", "only", map[string]int{"critical": 0})}
	if err := checkHydration(items, defaultMinHydrationSample); err != nil {
		t.Errorf("a sub-threshold empty input must pass, got %v", err)
	}
}

// Sanitization: the rendered component carries namespace/kind/name and none of
// the reachability evidence that must stay out of a public issue.
func TestComponentCarriesOnlyTheSanitizedMinimum(t *testing.T) {
	var it item
	it.Metadata.Name = "secret-thing"
	it.Metadata.Namespace = "app"
	it.Metadata.Labels = map[string]string{
		"kubescape.io/workload-namespace": "app",
		"kubescape.io/workload-kind":      "Deployment",
		"kubescape.io/workload-name":      "api",
	}
	got := it.component()
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
	dir := t.TempDir()
	path := filepath.Join(dir, "posture.json")
	writeList(t, path, []item{
		postureItem("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
	})

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
	dir := t.TempDir()
	path := filepath.Join(dir, "posture.json")
	writeList(t, path, []item{
		postureItem("app", "Deployment", "api", map[string]string{"C-0016": "failed"}),
		postureItem("data", "StatefulSet", "db", map[string]string{"C-0087": "failed"}),
	})

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
// The theme must keep the HIGHEST, independently of input order — anything
// order-dependent here would reintroduce the nondeterminism every other field
// is engineered to avoid.
func TestThemeSeverityIsTheHighestAndOrderIndependent(t *testing.T) {
	low := postureItem("app", "Deployment", "a", map[string]string{"C-0016": "failed"})
	high := postureItem("app", "Deployment", "b", map[string]string{"C-0016": "failed"})
	for id, c := range low.Spec.Controls {
		c.Severity.Severity = "Low"
		low.Spec.Controls[id] = c
	}
	for id, c := range high.Spec.Controls {
		c.Severity.Severity = "Critical"
		high.Spec.Controls[id] = c
	}

	for _, order := range [][]item{{low, high}, {high, low}} {
		got := derivePosture(order)
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

// An invocation with no input must not answer "nothing to file" — that is the
// same false all-clear the stripped-LIST guard exists to prevent.
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

func writeList(t *testing.T, path string, items []item) {
	t.Helper()
	raw, err := json.Marshal(list{Items: items})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
