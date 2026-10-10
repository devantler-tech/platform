package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixture() state {
	return state{
		Namespace:  resource{Name: "arc-runners", UID: "ns-1", RV: "10", Annotations: map[string]string{"foreign": "keep"}},
		Release:    &resource{Name: "platform-runners", Namespace: "arc-runners", UID: "hr-1", RV: "20", Annotations: map[string]string{"foreign": "keep"}},
		Credential: &resource{Name: "arc-github-app", Namespace: "arc-runners", UID: "eso-1", RV: "30", Annotations: map[string]string{}},
		Owner:      identity{Run: "123", Attempt: "1", SHA: strings.Repeat("a", 40)},
	}
}
func owned(t *testing.T) state {
	t.Helper()
	s := fixture()
	p, err := claim(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
	return s
}
func TestClaimIsNamespaceBoundAndPreservesForeignAnnotations(t *testing.T) {
	s := fixture()
	p, err := claim(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p[0].Path != "/metadata/uid" || p[0].Value != "ns-1" || p[1].Path != "/metadata/resourceVersion" || p[1].Value != "10" {
		t.Fatal("missing compare-and-swap")
	}
	a := p[len(p)-1].Value.(map[string]string)
	if a["foreign"] != "keep" {
		t.Fatal("foreign annotation lost")
	}
	var j journal
	if json.Unmarshal([]byte(a[journalKey]), &j) != nil || j.NamespaceUID != "ns-1" || j.HRUID != "hr-1" || j.ESOUID != "eso-1" || j.Phase != "fenced" {
		t.Fatal("identity missing")
	}
}
func TestPartialInstallRecordsProvenAbsence(t *testing.T) {
	for _, missing := range []string{"release", "credential", "both"} {
		s := fixture()
		if missing != "credential" {
			s.Release = nil
		}
		if missing != "release" {
			s.Credential = nil
		}
		if _, err := claim(s, nil); err != nil {
			t.Fatal(missing, err)
		}
	}
}
func TestJournalRejectsReplayReplacementAndAmbiguousJSON(t *testing.T) {
	for _, kind := range []string{"namespace", "release", "credential", "owner", "duplicate", "alias", "trailing", "unknown"} {
		s := owned(t)
		switch kind {
		case "namespace":
			s.Namespace.UID = "replacement"
		case "release":
			s.Release.UID = "replacement"
		case "credential":
			s.Credential.UID = "replacement"
		case "owner":
			s.Owner.SHA = strings.Repeat("b", 40)
		case "duplicate":
			s.Namespace.Annotations[journalKey] = strings.Replace(s.Namespace.Annotations[journalKey], "\"version\":1", "\"version\":1,\"version\":1", 1)
		case "alias":
			s.Namespace.Annotations[journalKey] = strings.Replace(s.Namespace.Annotations[journalKey], "\"version\"", "\"Version\"", 1)
		case "trailing":
			s.Namespace.Annotations[journalKey] += " {}"
		case "unknown":
			s.Namespace.Annotations[journalKey] = strings.Replace(s.Namespace.Annotations[journalKey], "\"version\":1", "\"version\":1,\"unexpected\":true", 1)
		}
		if _, err := verify(s); err == nil {
			t.Fatal("accepted", kind)
		}
	}
}
func TestForeignAttemptRequiresExactTerminalReceipt(t *testing.T) {
	s := owned(t)
	old := s.Owner
	s.Owner.Run = "456"
	if _, err := claim(s, nil); err == nil {
		t.Fatal("missing receipt accepted")
	}
	for _, body := range []string{
		`{"id":123,"run_attempt":1,"status":"in_progress","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}`,
		`{"id":123,"run_attempt":2,"status":"completed","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}`,
		`{"id":124,"run_attempt":1,"status":"completed","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}`,
		`{"id":123,"run_attempt":1,"status":"completed","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"other/platform"}}`,
		`{"id":123,"id":123,"run_attempt":1,"status":"completed","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}`,
		`{"id":123,"run_attempt":1,"status":"in_progress","ſtatus":"completed","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}`,
		`{"ID":123,"RUN_ATTEMPT":1,"Status":"completed","Repository":{"FULL_NAME":"devantler-tech/platform"}}`,
		`{"id":"123","run_attempt":1,"status":"completed","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}`,
	} {
		if terminal([]byte(body), old) == nil {
			t.Fatal("accepted wrong receipt")
		}
	}
	receipt := []byte(`{"id":123,"run_attempt":1,"status":"completed","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}`)
	if _, err := claim(s, receipt); err != nil {
		t.Fatal(err)
	}
	// The same native attempt may execute always() recovery while still running.
	s.Owner = old
	if _, err := claim(s, nil); err != nil {
		t.Fatal(err)
	}
}
func TestRetirementOrderingCannotDropCredentialBeforeUninstall(t *testing.T) {
	s := owned(t)
	for _, phase := range []string{"quiescing", "quiesced", "uninstalling", "uninstalled", "credential-removing", "absent", "restored"} {
		if _, err := advance(s, phase); err == nil {
			t.Fatal("skipped drain", phase)
		}
	}
	s.DrainProven = true
	p, err := advance(s, "drained")
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
	s.Controller = &resource{Name: "arc-controller", Namespace: "arc-systems", UID: "controller-1", RV: "40"}
	s.ControllerPodUIDs = []string{"old-1", "old-2"}
	p, err = advance(s, "quiescing")
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
	if _, err = advance(s, "quiesced"); err == nil {
		t.Fatal("old controller processes accepted")
	}
	s.ControllerReplaced = true
	p, err = advance(s, "quiesced")
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
	p, err = advance(s, "uninstalling")
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
	if _, err = advance(s, "uninstalled"); err == nil {
		t.Fatal("live release accepted")
	}
	s.Release = nil
	s.ChildrenAbsent = true
	s.NodesAbsent = true
	p, err = advance(s, "uninstalled")
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
	p, err = advance(s, "credential-removing")
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
	if _, err = advance(s, "absent"); err == nil {
		t.Fatal("live credential accepted")
	}
	s.Credential = nil
	s.SecretAbsent = true
	p, err = advance(s, "absent")
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
	if _, err = advance(s, "restored"); err == nil {
		t.Fatal("restored before inactive source joined")
	}
	s.SourceProven = true
	p, err = advance(s, "restored")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := p[len(p)-1].Value.(map[string]string)[journalKey]; !present {
		t.Fatal("removed durable fence")
	}
}

func TestRestoredRemainsClosedAndCannotReturnToCredentialCreation(t *testing.T) {
	s := owned(t)
	j, _ := readJournal(s)
	j.Phase = "restored"
	j.ControllerUID, j.ControllerPodUIDs = "controller-1", []string{"old-1"}
	j.ControllerTicket = "native-123-1"
	s.Release, s.Credential = nil, nil
	s.ChildrenAbsent, s.NodesAbsent, s.SecretAbsent, s.SourceProven = true, true, true, true
	b, _ := json.Marshal(j)
	s.Namespace.Annotations[journalKey] = string(b)
	for _, phase := range []string{"fenced", "drained", "uninstalled", "credential-removing"} {
		if _, err := advance(s, phase); err == nil {
			t.Fatal("reopened", phase)
		}
	}
	if _, err := advance(s, "restored"); err != nil {
		t.Fatal(err)
	}
	s.SecretAbsent = false
	if _, err := advance(s, "restored"); err == nil {
		t.Fatal("accepted recreated credential")
	}
}
func TestInterruptedPhasesAreIdempotentButNeverAdoptNewResources(t *testing.T) {
	s := owned(t)
	if _, err := advance(s, "fenced"); err != nil {
		t.Fatal(err)
	}
	s.Release.UID = "replacement"
	if _, err := advance(s, "fenced"); err == nil {
		t.Fatal("replacement accepted")
	}
	s = owned(t)
	s.Release = nil
	if _, err := verify(s); err == nil {
		t.Fatal("unrecorded disappearance accepted")
	}
}

func TestPhaseRetriesRecheckCompleteEvidence(t *testing.T) {
	for _, kind := range []string{"release", "children", "nodes", "credential", "secret", "missing-hr-field", "null-eso-field"} {
		s := owned(t)
		var j journal
		if json.Unmarshal([]byte(s.Namespace.Annotations[journalKey]), &j) != nil {
			t.Fatal("fixture")
		}
		j.Phase = "credential-removing"
		s.Release = nil
		s.ChildrenAbsent = true
		s.NodesAbsent = true
		switch kind {
		case "release":
			s.Release = fixture().Release
		case "children":
			s.ChildrenAbsent = false
		case "nodes":
			s.NodesAbsent = false
		case "credential":
			j.Phase = "absent"
			s.SecretAbsent = true
		case "secret":
			j.Phase = "absent"
			s.Credential = nil
		case "missing-hr-field":
			j.Phase = "fenced"
			s = fixture()
			s.Release = nil
		case "null-eso-field":
			j.Phase = "fenced"
			s = fixture()
			s.Credential = nil
		}
		b, _ := json.Marshal(j)
		if kind == "missing-hr-field" {
			b = []byte(strings.Replace(string(b), "\"hrUID\":\"hr-1\",", "", 1))
		}
		if kind == "null-eso-field" {
			b = []byte(strings.Replace(string(b), "\"esoUID\":\"eso-1\"", "\"esoUID\":null", 1))
		}
		s.Namespace.Annotations[journalKey] = string(b)
		if _, err := advance(s, j.Phase); err == nil {
			t.Fatal("accepted", kind)
		}
		if _, err := claim(s, nil); err == nil {
			t.Fatal("adopted", kind)
		}
	}
}
