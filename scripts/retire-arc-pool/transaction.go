package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const journalKey = "platform.devantler.tech/arc-retirement"
const reconcileKey = "kustomize.toolkit.fluxcd.io/reconcile"

type identity struct {
	Run     string `json:"run"`
	Attempt string `json:"attempt"`
	SHA     string `json:"sha"`
}
type resource struct {
	Name        string
	Namespace   string
	UID         string
	RV          string
	Annotations map[string]string
	Spec        json.RawMessage
}
type state struct {
	Namespace          resource
	Release            *resource
	Credential         *resource
	ScaleSet           *resource
	Owner              identity
	DrainProven        bool
	ChildrenAbsent     bool
	NodesAbsent        bool
	SecretAbsent       bool
	SourceProven       bool
	WriterCurrent      bool
	Controller         *resource
	ControllerPodUIDs  []string
	ControllerReplaced bool
}
type journal struct {
	Version           int       `json:"version"`
	Owner             identity  `json:"owner"`
	NamespaceUID      string    `json:"namespaceUID"`
	HRUID             string    `json:"hrUID"`
	ESOUID            string    `json:"esoUID"`
	Phase             string    `json:"phase"`
	ControllerUID     string    `json:"controllerUID"`
	ControllerPodUIDs []string  `json:"controllerPodUIDs"`
	ControllerTicket  string    `json:"controllerTicket"`
	Baseline          *baseline `json:"baseline,omitempty"`
}
type operation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func validIdentity(o identity) bool {
	positive := regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	_, err := strconv.ParseUint(o.Run, 10, 64)
	attempt, attemptErr := strconv.ParseUint(o.Attempt, 10, 16)
	return err == nil && attemptErr == nil && positive.MatchString(o.Run) && positive.MatchString(o.Attempt) && attempt > 0 && regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(o.SHA)
}
func validResource(r resource, name, namespace string) bool {
	return r.Name == name && r.Namespace == namespace && r.UID != "" && r.RV != ""
}
func validateState(s state) error {
	if !validIdentity(s.Owner) || !validResource(s.Namespace, "arc-runners", "") ||
		(s.Release != nil && !validResource(*s.Release, "platform-runners", "arc-runners")) ||
		(s.Credential != nil && !validResource(*s.Credential, "arc-github-app", "arc-runners")) ||
		(s.ScaleSet != nil && !validResource(*s.ScaleSet, "platform-linux", "arc-runners")) ||
		(s.Controller != nil && !validResource(*s.Controller, "arc-controller", "arc-systems")) {
		return errors.New("retirement identity is incomplete")
	}
	return nil
}
func header(r resource) []operation {
	return []operation{{"test", "/metadata/uid", r.UID}, {"test", "/metadata/resourceVersion", r.RV}}
}
func annotations(r resource) map[string]string {
	a := make(map[string]string, len(r.Annotations)+1)
	for k, v := range r.Annotations {
		a[k] = v
	}
	return a
}
func record(s state, j journal) ([]operation, error) {
	body, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	a := annotations(s.Namespace)
	a[journalKey] = string(body)
	return append(header(s.Namespace), operation{"add", "/metadata/annotations", a}), nil
}

// Neither a duplicate field nor a differently cased alias may bless a receipt.
func unambiguous(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("receipt nesting exceeds bound")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delim != '{' && delim != '[' {
			return errors.New("invalid receipt delimiter")
		}
		seen := []string{}
		for d.More() {
			if delim == '{' {
				token, err := d.Token()
				key, ok := token.(string)
				if err != nil || !ok {
					return errors.New("ambiguous receipt fields")
				}
				for _, prior := range seen {
					if strings.EqualFold(prior, key) {
						return errors.New("ambiguous receipt fields")
					}
				}
				seen = append(seen, key)
			}
			if err := walk(depth + 1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
			return errors.New("incomplete receipt")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing receipt")
	}
	return nil
}
func readJournal(s state) (journal, error) {
	var j journal
	body := []byte(s.Namespace.Annotations[journalKey])
	if len(body) > 128<<10 || unambiguous(body) != nil {
		return j, errors.New("invalid retirement journal")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&j) != nil || d.Decode(new(any)) != io.EOF || j.Version != 1 || !validIdentity(j.Owner) || j.NamespaceUID != s.Namespace.UID {
		return j, errors.New("retirement journal identity changed")
	}
	// Struct field aliases are otherwise accepted by encoding/json.
	var canonical map[string]json.RawMessage
	if json.Unmarshal(body, &canonical) != nil {
		return j, errors.New("invalid retirement journal")
	}
	if len(canonical) != 9 && !(len(canonical) == 10 && j.Baseline != nil) {
		return j, errors.New("incomplete retirement journal")
	}
	for _, key := range []string{"namespaceUID", "hrUID", "esoUID", "phase", "controllerUID", "controllerTicket"} {
		if _, err := exactString(canonical, key); err != nil {
			return j, err
		}
	}
	for k := range canonical {
		switch k {
		case "version", "owner", "namespaceUID", "hrUID", "esoUID", "phase", "controllerUID", "controllerPodUIDs", "controllerTicket":
		case "baseline":
			if err := validateBaseline(canonical[k], j); err != nil {
				return j, err
			}
		default:
			return j, errors.New("noncanonical journal field")
		}
	}
	var ownerFields map[string]json.RawMessage
	if json.Unmarshal(canonical["owner"], &ownerFields) != nil {
		return j, errors.New("invalid journal owner")
	}
	if len(ownerFields) != 3 {
		return j, errors.New("incomplete journal owner")
	}
	for _, key := range []string{"run", "attempt", "sha"} {
		if _, err := exactString(ownerFields, key); err != nil {
			return j, err
		}
	}
	for k := range ownerFields {
		if k != "run" && k != "attempt" && k != "sha" {
			return j, errors.New("noncanonical owner field")
		}
	}
	switch j.Phase {
	case "baseline", "fenced", "drained":
		if j.Phase == "baseline" && j.Baseline == nil {
			return j, errors.New("baseline provenance is missing")
		}
		if j.ControllerUID != "" || len(j.ControllerPodUIDs) != 0 || j.ControllerTicket != "" {
			return j, errors.New("premature controller receipt")
		}
	case "quiescing", "quiesced", "uninstalling", "uninstalled", "credential-removing", "absent", "restored":
		if j.ControllerUID == "" || !validPodUIDs(j.ControllerPodUIDs) || !regexp.MustCompile(`^native-[1-9][0-9]{0,19}-[1-9][0-9]{0,4}(-[0-9a-f]{32})?$`).MatchString(j.ControllerTicket) {
			return j, errors.New("incomplete controller retirement receipt")
		}
	default:
		return j, errors.New("invalid retirement phase")
	}
	return j, nil
}
func verify(s state) (journal, error) {
	if err := validateState(s); err != nil {
		return journal{}, err
	}
	j, err := readJournal(s)
	if err != nil {
		return j, err
	}
	if j.Owner != s.Owner {
		return j, errors.New("retirement is owned by another native attempt")
	}
	if j.Baseline != nil {
		if err := baselineResources(s, j, false); err != nil {
			return j, err
		}
	}
	hrMayBeGone := j.Phase == "uninstalling" || j.Phase == "uninstalled" || j.Phase == "credential-removing" || j.Phase == "absent" || j.Phase == "restored"
	esoMayBeGone := j.Phase == "credential-removing" || j.Phase == "absent" || j.Phase == "restored"
	if (s.Release != nil && s.Release.UID != j.HRUID) || (s.Release == nil && j.HRUID != "" && !hrMayBeGone && j.Baseline == nil) ||
		(s.Credential != nil && s.Credential.UID != j.ESOUID) || (s.Credential == nil && j.ESOUID != "" && !esoMayBeGone && j.Baseline == nil) {
		return j, errors.New("retirement resource disappeared or was replaced outside its recorded transition")
	}
	if (j.Phase == "drained" || j.Phase == "quiescing" || j.Phase == "quiesced") && !s.DrainProven {
		return j, errors.New("drain proof is no longer complete")
	}
	if j.Phase == "uninstalling" && s.Release != nil && !s.DrainProven {
		return j, errors.New("release resumed before uninstall")
	}
	if j.Phase == "quiescing" || j.Phase == "quiesced" || j.Phase == "uninstalling" {
		if s.Controller == nil || s.Controller.UID != j.ControllerUID {
			return j, errors.New("controller identity changed")
		}
		if j.Phase != "quiescing" && !s.ControllerReplaced {
			return j, errors.New("old controller process remains")
		}
	}
	if (j.Phase == "uninstalled" || j.Phase == "credential-removing" || j.Phase == "absent" || j.Phase == "restored") && (s.Release != nil || !s.ChildrenAbsent || !s.NodesAbsent) {
		return j, errors.New("uninstall proof is no longer complete")
	}
	if (j.Phase == "absent" || j.Phase == "restored") && (s.Credential != nil || !s.SecretAbsent) {
		return j, errors.New("credential retirement is no longer complete")
	}
	return j, nil
}
func validPodUIDs(ids []string) bool {
	if len(ids) < 1 || len(ids) > 32 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || len(id) > 128 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func exactString(fields map[string]json.RawMessage, key string) (string, error) {
	var value any
	if json.Unmarshal(fields[key], &value) != nil {
		return "", errors.New("missing receipt string")
	}
	s, ok := value.(string)
	if !ok {
		return "", errors.New("receipt string has wrong type")
	}
	return s, nil
}
func exactNumber(fields map[string]json.RawMessage, key string) (string, error) {
	raw := fields[key]
	if !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).Match(raw) {
		return "", errors.New("receipt number has wrong type")
	}
	if _, err := strconv.ParseUint(string(raw), 10, 64); err != nil {
		return "", err
	}
	return string(raw), nil
}
func terminal(body []byte, old identity) error {
	if !validIdentity(old) || len(body) > 1<<20 || unambiguous(body) != nil {
		return errors.New("invalid previous-attempt receipt")
	}
	var fields, repository map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["repository"], &repository) != nil {
		return errors.New("invalid attempt object")
	}
	run, e1 := exactNumber(fields, "id")
	attempt, e2 := exactNumber(fields, "run_attempt")
	status, e3 := exactString(fields, "status")
	name, e4 := exactString(repository, "full_name")
	sha, e5 := exactString(fields, "head_sha")
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || run != old.Run || attempt != old.Attempt || sha != old.SHA || status != "completed" || name != "devantler-tech/platform" {
		return errors.New("previous exact attempt is not terminal")
	}
	return nil
}
func claim(s state, receipt []byte) ([]operation, error) {
	if err := validateState(s); err != nil {
		return nil, err
	}
	if _, present := s.Namespace.Annotations[journalKey]; present {
		j, err := readJournal(s)
		if err != nil {
			return nil, err
		}
		original := s.Owner
		s.Owner = j.Owner
		if original != j.Owner {
			if original.Run == j.Owner.Run && original.Attempt == j.Owner.Attempt {
				return nil, errors.New("same-attempt source identity changed")
			}
			if err := terminal(receipt, j.Owner); err != nil {
				return nil, err
			}
		}
		if j.Baseline != nil {
			if err := baselineResources(s, j, true); err != nil {
				return nil, err
			}
			changed := bindFirst(&j, s)
			if j.Phase == "baseline" || changed {
				closeBaseline(&j)
			}
			// Check the freshly bound journal against current receipts, without
			// writing it until the outer Namespace UID/RV CAS succeeds.
			body, _ := json.Marshal(j)
			s.Namespace.Annotations = annotations(s.Namespace)
			s.Namespace.Annotations[journalKey] = string(body)
		}
		if _, err := verify(s); err != nil {
			return nil, err
		}
		s.Owner = original
		j.Owner = original
		return record(s, j)
	}
	j := journal{Version: 1, Owner: s.Owner, NamespaceUID: s.Namespace.UID, Phase: "fenced", ControllerPodUIDs: []string{}}
	if s.Release != nil {
		j.HRUID = s.Release.UID
	}
	if s.Credential != nil {
		j.ESOUID = s.Credential.UID
	}
	return record(s, j)
}
func advance(s state, next string) ([]operation, error) {
	j, err := verify(s)
	if err != nil {
		return nil, err
	}
	if next == "restored" && !s.SourceProven {
		return nil, errors.New("inactive source has not joined")
	}
	if next == j.Phase {
		return record(s, j)
	}
	valid := false
	switch next {
	case "drained":
		valid = j.Phase == "fenced" && s.DrainProven && (j.Baseline == nil || j.Baseline.Writer != nil)
	case "quiescing":
		valid = j.Phase == "drained" && s.DrainProven && s.Controller != nil && validPodUIDs(s.ControllerPodUIDs)
		if valid {
			j.ControllerUID = s.Controller.UID
			j.ControllerPodUIDs = append([]string{}, s.ControllerPodUIDs...)
			sort.Strings(j.ControllerPodUIDs)
			body, _ := json.Marshal(j.ControllerPodUIDs)
			hash := sha256.Sum256(body)
			j.ControllerTicket = "native-" + s.Owner.Run + "-" + s.Owner.Attempt + "-" + fmt.Sprintf("%x", hash[:16])
		}
	case "quiesced":
		valid = j.Phase == "quiescing" && s.ControllerReplaced
	case "uninstalling":
		valid = j.Phase == "quiesced"
	case "uninstalled":
		valid = j.Phase == "uninstalling" && s.Release == nil && s.ChildrenAbsent && s.NodesAbsent
	case "credential-removing":
		valid = j.Phase == "uninstalled" && s.Release == nil && s.ChildrenAbsent && s.NodesAbsent
	case "absent":
		valid = j.Phase == "credential-removing" && s.Release == nil && s.Credential == nil && s.ChildrenAbsent && s.NodesAbsent && s.SecretAbsent
	case "restored":
		valid = j.Phase == "absent" && s.SourceProven
	}
	if !valid {
		return nil, errors.New("retirement phase lacks its complete proof")
	}
	j.Phase = next
	return record(s, j)
}
