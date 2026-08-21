package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func writeOracleFixture(t *testing.T, exceptionJSON, postureJSON string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	exceptionsPath := filepath.Join(dir, "exceptions.json")
	posturePath := filepath.Join(dir, "posture.json")
	writeRaw(t, exceptionsPath, exceptionJSON)
	writeBare(t, posturePath, postureJSON)

	return exceptionsPath, posturePath
}

// The production break this catches is reporting a finding as excepted while
// leaving policies= blank. An unnamed policy cannot provide the audit identity
// oracle mode exists to expose, so the artifact must be rejected instead.
func TestOracleRejectsAnUnnamedMatchingPolicy(t *testing.T) {
	exceptionsPath, posturePath := writeOracleFixture(t,
		exceptionsDoc(policyDoc("", []string{"C-0016"}, `{"kind":"^Job$"}`)),
		postureDoc("app", "Job", "nightly", map[string]string{"C-0016": "failed"}),
	)

	var out bytes.Buffer
	err := run([]string{
		"-mode=oracle", "-exceptions", exceptionsPath, "-posture", posturePath,
	}, &out)
	if !errors.Is(err, errBadExceptions) {
		t.Fatalf("want errBadExceptions, got %v", err)
	}

	if out.Len() != 0 {
		t.Fatalf("an unnamed policy must not produce an oracle verdict, got %q", out.String())
	}
}

// The production break this catches is an oracle that filters a finding but
// never identifies which declared policy made that decision. An aggregate
// suppression count cannot verify one exception or support a local ablation.
func TestOracleNamesTheMatchingException(t *testing.T) {
	exceptionsPath, posturePath := writeOracleFixture(t,
		exceptionsDoc(policyDoc("batch-workloads", []string{"C-0016"}, `{"kind":"^Job$"}`)),
		postureDoc("app", "Job", "nightly", map[string]string{"C-0016": "failed"}),
	)

	var out bytes.Buffer
	if err := run([]string{
		"-mode=oracle",
		"-exceptions", exceptionsPath,
		"-posture", posturePath,
	}, &out); err != nil {
		t.Fatalf("run oracle: %v", err)
	}

	want := "excepted\tcontrol=C-0016\tcomponent=app/Job/nightly\tpolicies=batch-workloads\n"
	if got := out.String(); got != want {
		t.Fatalf("oracle output mismatch\nwant: %q\n got: %q", want, got)
	}
}

// The production break this catches is a non-discriminating oracle: removing
// the policy locally must change the answer for the exact same raw finding.
func TestOracleAblationChangesTheAnswer(t *testing.T) {
	posture := postureDoc("app", "Job", "nightly", map[string]string{"C-0016": "failed"})
	matchingPath, posturePath := writeOracleFixture(t,
		exceptionsDoc(policyDoc("batch-workloads", []string{"C-0016"}, `{"kind":"^Job$"}`)),
		posture,
	)
	ablatedPath, _ := writeOracleFixture(t,
		exceptionsDoc(policyDoc("unrelated", []string{"C-0999"}, `{"kind":"^Job$"}`)),
		posture,
	)

	var withPolicy bytes.Buffer
	if err := run([]string{
		"-mode=oracle", "-exceptions", matchingPath, "-posture", posturePath,
	}, &withPolicy); err != nil {
		t.Fatalf("run with matching policy: %v", err)
	}

	var withoutPolicy bytes.Buffer
	if err := run([]string{
		"-mode=oracle", "-exceptions", ablatedPath, "-posture", posturePath,
	}, &withoutPolicy); err != nil {
		t.Fatalf("run with policy ablated: %v", err)
	}

	if !strings.HasPrefix(withPolicy.String(), "excepted\t") {
		t.Fatalf("matching policy must report excepted, got %q", withPolicy.String())
	}

	wantAblated := "unexcepted\tcontrol=C-0016\tcomponent=app/Job/nightly\tpolicies=-\n"
	if got := withoutPolicy.String(); got != wantAblated {
		t.Fatalf("ablated output mismatch\nwant: %q\n got: %q", wantAblated, got)
	}

	if withPolicy.String() == withoutPolicy.String() {
		t.Fatal("ablating the matching policy must change the oracle's answer")
	}
}

// The production break this catches is matching the kind parsed out of wlid
// instead of the workload identity labels the generated exception artifact
// names. The two intentionally disagree in this fixture.
func TestOracleMatchesWorkloadKindLabelInsteadOfWLID(t *testing.T) {
	posture := strings.Replace(
		postureDoc("app", "Job", "nightly", map[string]string{"C-0016": "failed"}),
		`"spec":{`,
		`"spec":{"wlid":"wlid://cluster-app/namespace-app/deployment-nightly",`,
		1,
	)
	exceptionsPath, posturePath := writeOracleFixture(t,
		exceptionsDoc(policyDoc("batch-workloads", []string{"C-0016"}, `{"kind":"^Job$"}`)),
		posture,
	)

	var out bytes.Buffer
	if err := run([]string{
		"-mode=oracle", "-exceptions", exceptionsPath, "-posture", posturePath,
	}, &out); err != nil {
		t.Fatalf("run oracle: %v", err)
	}

	if !strings.HasPrefix(out.String(), "excepted\t") {
		t.Fatalf("the Job label must decide the match despite the Deployment wlid, got %q", out.String())
	}
}

// Oracle mode is opt-in. The default report remains the backlog view rather
// than exposing per-finding policy decisions in every ordinary run.
func TestDefaultReportDoesNotEmitOracleRows(t *testing.T) {
	exceptionsPath, posturePath := writeOracleFixture(t,
		exceptionsDoc(policyDoc("batch-workloads", []string{"C-0016"}, `{"kind":"^Job$"}`)),
		postureDoc("app", "Job", "nightly", map[string]string{"C-0016": "failed"}),
	)

	var out bytes.Buffer
	if err := run([]string{
		"-exceptions", exceptionsPath, "-posture", posturePath,
	}, &out); err != nil {
		t.Fatalf("run default report: %v", err)
	}

	if strings.Contains(out.String(), "policies=") {
		t.Fatalf("default report must not emit oracle rows, got %q", out.String())
	}
}
