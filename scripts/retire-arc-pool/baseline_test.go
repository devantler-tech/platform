package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func baselineFixture(t *testing.T) state {
	t.Helper()
	s := fixture()
	s.Release.Spec = json.RawMessage(`{"suspend":false,"chartRef":{"kind":"OCIRepository","name":"platform-runners"},"values":{"runnerScaleSetName":"platform-linux","githubConfigUrl":"https://github.com/devantler-tech","githubConfigSecret":"arc-github-app","runnerGroup":"platform","minRunners":0,"maxRunners":0}}`)
	s.Credential.Spec = json.RawMessage(`{"target":{"name":"arc-github-app","creationPolicy":"Owner"},"secretStoreRef":{"name":"openbao","kind":"SecretStore"}}`)
	s.ScaleSet = &resource{Name: "platform-linux", Namespace: "arc-runners", UID: "ars-1", RV: "40", Annotations: map[string]string{"meta.helm.sh/release-name": "platform-runners", "meta.helm.sh/release-namespace": "arc-runners"}, Spec: json.RawMessage(`{"runnerScaleSetName":"platform-linux","githubConfigUrl":"https://github.com/devantler-tech","githubConfigSecret":"arc-github-app","runnerGroup":"platform","minRunners":0,"maxRunners":0}`)}
	b := &baseline{Version: 1, SourceSHA: strings.Repeat("b", 40), Digest: "sha256:" + strings.Repeat("c", 64), HRSpec0: s.Release.Spec, ARSSpec0: s.ScaleSet.Spec, ESOSpec: s.Credential.Spec}
	j := journal{Version: 1, Owner: s.Owner, NamespaceUID: s.Namespace.UID, Phase: "baseline", ControllerPodUIDs: []string{}, Baseline: b}
	setJournal(t, &s, j)
	return s
}
func setJournal(t *testing.T, s *state, j journal) {
	t.Helper()
	body, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	s.Namespace.Annotations[journalKey] = string(body)
}
func applyRecord(t *testing.T, s *state, p []operation) {
	t.Helper()
	s.Namespace.Annotations[journalKey] = p[len(p)-1].Value.(map[string]string)[journalKey]
}

func TestBaselineClaimClosesAndBindsOnlyExactZero(t *testing.T) {
	s := baselineFixture(t)
	p, err := claim(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	applyRecord(t, &s, p)
	j, err := verify(s)
	if err != nil {
		t.Fatal(err)
	}
	if j.Phase != "fenced" || j.HRUID != "hr-1" || j.ESOUID != "eso-1" || j.Baseline.ARSUID != "ars-1" || j.Baseline.Maximum != 0 || j.Baseline.SourceSHA == s.Owner.SHA {
		t.Fatal("missing closed source-bound identities")
	}
	for _, kind := range []string{"active-first", "changed-spec", "foreign-chart"} {
		s := baselineFixture(t)
		switch kind {
		case "active-first":
			s.Release.Spec = json.RawMessage(strings.Replace(string(s.Release.Spec), `"maxRunners":0`, `"maxRunners":1`, 1))
		case "changed-spec":
			s.Credential.Spec = json.RawMessage(strings.Replace(string(s.Credential.Spec), `"openbao"`, `"other"`, 1))
		case "foreign-chart":
			s.ScaleSet.Annotations["meta.helm.sh/release-name"] = "other"
		}
		if _, err := claim(s, nil); err == nil {
			t.Fatal("accepted", kind)
		}
	}
}
func TestBaselineLateFirstBindingInvalidatesAllCompletionReceipts(t *testing.T) {
	for _, kind := range []string{"hr", "eso", "ars"} {
		s := baselineFixture(t)
		lateHR, lateESO, lateARS := s.Release, s.Credential, s.ScaleSet
		s.Release, s.Credential, s.ScaleSet = nil, nil, nil
		p, err := claim(s, nil)
		if err != nil {
			t.Fatal(err)
		}
		applyRecord(t, &s, p)
		j, _ := readJournal(s)
		j.Phase = "restored"
		j.Baseline.Writer = writerFixture(t)
		j.ControllerUID = "controller"
		j.ControllerPodUIDs = []string{"old"}
		j.ControllerTicket = "native-123-1"
		setJournal(t, &s, j)
		switch kind {
		case "hr":
			s.Release = lateHR
		case "eso":
			s.Credential = lateESO
		case "ars":
			s.ScaleSet = lateARS
		}
		if _, err := verify(s); err == nil {
			t.Fatal("unbound late object verified", kind)
		}
		p, err = bindBaseline(s)
		if err != nil {
			t.Fatal(kind, err)
		}
		applyRecord(t, &s, p)
		j, err = verify(s)
		if err != nil {
			t.Fatal(kind, err)
		}
		if j.Phase != "fenced" || j.ControllerUID != "" || len(j.ControllerPodUIDs) != 0 || j.ControllerTicket != "" {
			t.Fatal("completion survived late creation")
		}
		p, err = bindBaseline(s)
		if err != nil {
			t.Fatal(err)
		}
		applyRecord(t, &s, p)
		switch kind {
		case "hr":
			s.Release.UID = "replacement"
		case "eso":
			s.Credential.UID = "replacement"
		case "ars":
			s.ScaleSet.UID = "replacement"
		}
		if _, err := bindBaseline(s); err == nil {
			t.Fatal("bound UID replaced", kind)
		}
	}
}
func TestBaselineForeignOpeningRequiresTerminalOwnerBeforeAnyBinding(t *testing.T) {
	s := baselineFixture(t)
	s.Owner.Run = "456"
	if _, err := claim(s, nil); err == nil {
		t.Fatal("adopted live or unknown producer")
	}
	if _, err := claim(s, []byte(`{"id":123,"run_attempt":1,"status":"completed","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repository":{"full_name":"devantler-tech/platform"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := bindBaseline(s); err == nil {
		t.Fatal("bound foreign owner without claim")
	}
}

func TestRepeatedRetirementRestartsEachObservedControllerProcess(t *testing.T) {
	s := owned(t)
	s.DrainProven = true
	s.Controller = &resource{Name: "arc-controller", Namespace: "arc-systems", UID: "controller", RV: "40"}
	p, err := advance(s, "drained")
	if err != nil {
		t.Fatal(err)
	}
	applyRecord(t, &s, p)
	s.ControllerPodUIDs = []string{"first-pod"}
	p, err = advance(s, "quiescing")
	if err != nil {
		t.Fatal(err)
	}
	applyRecord(t, &s, p)
	j, _ := readJournal(s)
	first := j.ControllerTicket
	j.Phase = "drained"
	j.ControllerUID = ""
	j.ControllerPodUIDs = []string{}
	j.ControllerTicket = ""
	setJournal(t, &s, j)
	s.ControllerPodUIDs = []string{"second-pod"}
	p, err = advance(s, "quiescing")
	if err != nil {
		t.Fatal(err)
	}
	applyRecord(t, &s, p)
	j, err = readJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	if j.ControllerTicket == first {
		t.Fatal("second process received the old restart annotation")
	}
}
func TestBaselineSchemaAndActiveAuthorityFailClosed(t *testing.T) {
	for _, kind := range []string{"null", "partial", "alias", "oversized", "active-unproved", "active-unbound", "wrong-zero-digest", "nonzero-spec"} {
		s := baselineFixture(t)
		j, _ := readJournal(s)
		switch kind {
		case "active-unproved", "active-unbound", "wrong-zero-digest":
			j.Baseline.Maximum = 1
			if kind != "active-unproved" {
				j.Baseline.ZeroSource = &sourceIdentity{SHA: strings.Repeat("d", 40), Digest: "sha256:" + strings.Repeat("e", 64)}
			}
			if kind == "wrong-zero-digest" {
				j.Baseline.ZeroSource.Digest = "unbound"
			}
			if kind != "active-unbound" {
				j.HRUID = "hr-1"
				j.ESOUID = "eso-1"
				j.Baseline.HRUID = "hr-1"
				j.Baseline.ESOUID = "eso-1"
				j.Baseline.ARSUID = "ars-1"
			}
		case "nonzero-spec":
			j.Baseline.ARSSpec0 = json.RawMessage(strings.Replace(string(j.Baseline.ARSSpec0), `"maxRunners":0`, `"maxRunners":1`, 1))
		}
		setJournal(t, &s, j)
		body := s.Namespace.Annotations[journalKey]
		switch kind {
		case "null":
			body = strings.Replace(body, `"baseline":{`, `"baseline":null,"unrecognized":{`, 1)
		case "partial":
			body = strings.Replace(body, `"sourceSHA":"`+strings.Repeat("b", 40)+`",`, "", 1)
		case "alias":
			body = strings.Replace(body, `"sourceSHA"`, `"SourceSHA"`, 1)
		case "oversized":
			body += strings.Repeat(" ", 128<<10)
		}
		s.Namespace.Annotations[journalKey] = body
		if _, err := readJournal(s); err == nil {
			t.Fatal("accepted", kind)
		}
	}
}
