package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
)

// Provenance survives recovery. A never-bound identity can appear after the
// closing admission CAS, but only its exact zero declaration can be enrolled.
// A previously recorded UID is never replaced, even after its object is gone.
type sourceIdentity struct {
	SHA    string `json:"sha"`
	Digest string `json:"digest"`
}
type baseline struct {
	Version    int             `json:"version"`
	SourceSHA  string          `json:"sourceSHA"`
	Digest     string          `json:"digest"`
	Maximum    int             `json:"maximum"`
	HRSpec0    json.RawMessage `json:"hrSpec0"`
	ARSSpec0   json.RawMessage `json:"arsSpec0"`
	ESOSpec    json.RawMessage `json:"esoSpec"`
	HRUID      string          `json:"hrUID"`
	ARSUID     string          `json:"arsUID"`
	ESOUID     string          `json:"esoUID"`
	ZeroSource *sourceIdentity `json:"zeroSource,omitempty"`
}

func validSource(sha, digest string) bool {
	return regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) && regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest)
}
func canonicalFields(raw []byte, required []string, optional string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if unambiguous(raw) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errors.New("invalid baseline object")
	}
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
		if _, ok := fields[key]; !ok || bytes.Equal(fields[key], []byte("null")) {
			return nil, errors.New("incomplete baseline object")
		}
	}
	if optional != "" {
		allowed[optional] = true
	}
	for key, raw := range fields {
		if !allowed[key] || bytes.Equal(raw, []byte("null")) {
			return nil, errors.New("noncanonical baseline field")
		}
	}
	return fields, nil
}
func decodeSpec(raw json.RawMessage) (object, error) {
	o, err := evidence(raw)
	if err != nil || len(o) == 0 {
		return nil, errors.New("invalid expected resource specification")
	}
	return o, nil
}
func validateBaseline(raw []byte, j journal) error {
	b := j.Baseline
	fields, err := canonicalFields(raw, []string{"version", "sourceSHA", "digest", "maximum", "hrSpec0", "arsSpec0", "esoSpec", "hrUID", "arsUID", "esoUID"}, "zeroSource")
	if err != nil || b == nil {
		return errors.New("invalid baseline provenance")
	}
	for _, key := range []string{"sourceSHA", "digest", "hrUID", "arsUID", "esoUID"} {
		if _, err := exactString(fields, key); err != nil {
			return err
		}
	}
	if b.Version != 1 || (b.Maximum != 0 && b.Maximum != 1) || !validSource(b.SourceSHA, b.Digest) || b.HRUID != j.HRUID || b.ESOUID != j.ESOUID {
		return errors.New("inconsistent baseline identities")
	}
	if j.Phase != "baseline" && b.Maximum != 0 {
		return errors.New("closed recovery retains active admission authority")
	}
	if b.ZeroSource != nil {
		if _, err := canonicalFields(fields["zeroSource"], []string{"sha", "digest"}, ""); err != nil || !validSource(b.ZeroSource.SHA, b.ZeroSource.Digest) {
			return errors.New("invalid zero-source receipt")
		}
	}
	if b.Maximum == 1 && (b.ZeroSource == nil || b.HRUID == "" || b.ARSUID == "" || b.ESOUID == "") {
		return errors.New("active capacity lacks a bound zero baseline")
	}
	hr, e1 := decodeSpec(b.HRSpec0)
	ars, e2 := decodeSpec(b.ARSSpec0)
	eso, e3 := decodeSpec(b.ESOSpec)
	if e1 != nil || e2 != nil || e3 != nil || at(hr, "suspend") != false || !zeroAt(hr, "values", "minRunners") || !zeroAt(hr, "values", "maxRunners") || !zeroAt(ars, "minRunners") || !zeroAt(ars, "maxRunners") ||
		textAt(hr, "chartRef", "kind") != "OCIRepository" || textAt(hr, "chartRef", "name") != "platform-runners" ||
		textAt(hr, "values", "runnerScaleSetName") != "platform-linux" || textAt(ars, "runnerScaleSetName") != "platform-linux" ||
		textAt(hr, "values", "githubConfigUrl") != "https://github.com/devantler-tech" || textAt(ars, "githubConfigUrl") != "https://github.com/devantler-tech" ||
		textAt(hr, "values", "githubConfigSecret") != "arc-github-app" || textAt(ars, "githubConfigSecret") != "arc-github-app" ||
		textAt(hr, "values", "runnerGroup") != "platform" || textAt(ars, "runnerGroup") != "platform" ||
		textAt(eso, "target", "name") != "arc-github-app" || textAt(eso, "target", "creationPolicy") != "Owner" || textAt(eso, "secretStoreRef", "name") != "openbao" || textAt(eso, "secretStoreRef", "kind") != "SecretStore" {
		return errors.New("baseline is not the exact explicit-zero pool")
	}
	return nil
}
func matchingSpec(actual, expected json.RawMessage, kind string, bound bool) bool {
	a, e1 := decodeSpec(actual)
	e, e2 := decodeSpec(expected)
	if e1 != nil || e2 != nil {
		return false
	}
	if reflect.DeepEqual(a, e) {
		return true
	}
	if !bound || kind == "eso" {
		return false
	}
	// Retirement may encounter the already authorized one variant or a suspended
	// zero/one release. No other spec change belongs to this source authority.
	if kind == "hr" {
		if at(a, "suspend") != false && at(a, "suspend") != true {
			return false
		}
		a["suspend"] = false
		values, ok := a["values"].(map[string]any)
		if !ok {
			return false
		}
		n, ok := numberAt(values, "maxRunners")
		if !ok || (n != 0 && n != 1) || !zeroAt(values, "minRunners") {
			return false
		}
		values["maxRunners"] = json.Number("0")
	} else {
		n, ok := numberAt(a, "maxRunners")
		if !ok || (n != 0 && n != 1) || !zeroAt(a, "minRunners") {
			return false
		}
		a["maxRunners"] = json.Number("0")
	}
	return reflect.DeepEqual(a, e)
}
func baselineResources(s state, j journal, permitFirst bool) error {
	b := j.Baseline
	if b == nil {
		return errors.New("baseline provenance is missing")
	}
	for _, item := range []struct {
		r    *resource
		uid  string
		spec json.RawMessage
		kind string
	}{{s.Release, j.HRUID, b.HRSpec0, "hr"}, {s.Credential, j.ESOUID, b.ESOSpec, "eso"}, {s.ScaleSet, b.ARSUID, b.ARSSpec0, "ars"}} {
		if item.r == nil {
			continue
		}
		if (item.uid != "" && item.r.UID != item.uid) || (item.uid == "" && !permitFirst) || !matchingSpec(item.r.Spec, item.spec, item.kind, item.uid != "") {
			return errors.New("baseline resource is unbound, replaced or outside its exact specification")
		}
		if item.kind == "ars" && (item.r.Annotations["meta.helm.sh/release-name"] != "platform-runners" || item.r.Annotations["meta.helm.sh/release-namespace"] != "arc-runners") {
			return errors.New("scale set does not belong to the bound chart")
		}
	}
	return nil
}
func bindFirst(j *journal, s state) bool {
	changed := false
	if j.HRUID == "" && s.Release != nil {
		j.HRUID = s.Release.UID
		j.Baseline.HRUID = j.HRUID
		changed = true
	}
	if j.ESOUID == "" && s.Credential != nil {
		j.ESOUID = s.Credential.UID
		j.Baseline.ESOUID = j.ESOUID
		changed = true
	}
	if j.Baseline.ARSUID == "" && s.ScaleSet != nil {
		j.Baseline.ARSUID = s.ScaleSet.UID
		changed = true
	}
	return changed
}
func closeBaseline(j *journal) {
	j.Phase = "fenced"
	j.Baseline.Maximum = 0
	j.ControllerUID = ""
	j.ControllerPodUIDs = []string{}
	j.ControllerTicket = ""
}
func bindBaseline(s state) ([]operation, error) {
	if err := validateState(s); err != nil {
		return nil, err
	}
	j, err := readJournal(s)
	if err != nil {
		return nil, err
	}
	if j.Owner != s.Owner || j.Phase == "baseline" || j.Baseline == nil {
		return nil, errors.New("first binding requires this attempt's closed baseline")
	}
	if err := baselineResources(s, j, true); err != nil {
		return nil, err
	}
	if bindFirst(&j, s) {
		closeBaseline(&j)
	}
	return record(s, j)
}
