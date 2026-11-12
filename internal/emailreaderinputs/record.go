// Package emailreaderinputs validates the one fixed retained-copy input record.
// Values contain credentials: callers must keep them in private memory and never log them.
package emailreaderinputs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// Path is deliberately not a caller-selected credential location.
const Path = "infrastructure/email/recovery/retained-reader-inputs"

const namespace = "email-data-recovery-41561215415"

var errInvalid = errors.New("retained input evidence is incomplete or invalid")
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type input struct {
	Kind, Name, Namespace, UID, Digest string
	Values                             map[string]any
}

type document struct {
	Schema        string `json:"schema"`
	TrialRun      string `json:"trial_run"`
	TrialRevision string `json:"trial_revision"`
	Bundle        struct {
		ReaderUID, ClaimUID string
		Inputs              []input
	} `json:"bundle"`
}

// decode refuses duplicate, aliased, trailing, unknown and excessively nested fields.
func decode(raw []byte, value any, fields map[string][]string) error {
	if strictjson.Validate(raw, fields) != nil {
		return errInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		return errInvalid
	}
	return nil
}

// ValidateDocument binds all eight complete inputs, their independent identities and content hashes.
// It proves record grammar only; fresh native parity and independent restore remain caller obligations.
func ValidateDocument(raw []byte) error {
	var d document
	fields := map[string][]string{
		"":                {"schema", "trial_run", "trial_revision", "bundle"},
		"bundle":          {"ReaderUID", "ClaimUID", "Inputs"},
		"bundle.Inputs[]": {"Kind", "Name", "Namespace", "UID", "Digest", "Values"},
	}
	if len(raw) == 0 || len(raw) > 512<<10 || decode(raw, &d, fields) != nil || d.Schema != "email-reader-inputs/v1" || d.TrialRun != "41561215415" || d.TrialRevision != "7e4486cad02c326004ac9a1a637bb5205dbfe76a" || !uuid.MatchString(d.Bundle.ReaderUID) || !uuid.MatchString(d.Bundle.ClaimUID) || d.Bundle.ReaderUID == d.Bundle.ClaimUID || len(d.Bundle.Inputs) != 8 {
		return errInvalid
	}
	allowed := map[[3]string]bool{
		{"Pod", "reader", namespace}:                    true,
		{"PersistentVolumeClaim", "data", namespace}:    true,
		{"Namespace", namespace, ""}:                    true,
		{"StorageClass", namespace, ""}:                 true,
		{"CiliumNetworkPolicy", "isolation", namespace}: true,
		{"ConfigMap", "datastore", namespace}:           true,
		{"Secret", "storage-key", namespace}:            true,
		{"Secret", "test-authentication", namespace}:    true,
	}
	seen := map[string]bool{}
	for _, i := range d.Bundle.Inputs {
		ref := [3]string{i.Kind, i.Name, i.Namespace}
		if !allowed[ref] || !uuid.MatchString(i.UID) || seen[i.UID] || len(i.Values) == 0 || (i.Kind == "Pod" && i.UID != d.Bundle.ReaderUID) || (i.Kind == "PersistentVolumeClaim" && i.UID != d.Bundle.ClaimUID) {
			return errInvalid
		}
		delete(allowed, ref)
		seen[i.UID] = true
		canonical, err := json.Marshal(i.Values)
		if err != nil {
			return errInvalid
		}
		digest := sha256.Sum256(canonical)
		if hex.EncodeToString(digest[:]) != i.Digest {
			return errInvalid
		}
	}
	return nil
}

// RecordValue admits only a complete, undeleted original KV2 version and preserves its creation time.
// Input is the KV2 response's data object, never a passphrase-only or account projection.
func RecordValue(raw []byte) (map[string]any, error) {
	var record struct {
		Data     json.RawMessage `json:"data"`
		Metadata struct {
			Version        json.RawMessage `json:"version"`
			Created        *string         `json:"created_time"`
			Deleted        *string         `json:"deletion_time"`
			Destroyed      *bool           `json:"destroyed"`
			CustomMetadata json.RawMessage `json:"custom_metadata"`
		} `json:"metadata"`
	}
	fields := map[string][]string{"": {"data", "metadata"}, "metadata": {"version", "created_time", "deletion_time", "destroyed", "custom_metadata"}}
	if len(raw) == 0 || len(raw) > 1<<20 || decode(raw, &record, fields) != nil || !bytes.Equal(bytes.TrimSpace(record.Metadata.Version), []byte("1")) || record.Metadata.Created == nil || record.Metadata.Deleted == nil || *record.Metadata.Deleted != "" || record.Metadata.Destroyed == nil || *record.Metadata.Destroyed || ValidateDocument(record.Data) != nil {
		return nil, errInvalid
	}
	created, err := time.Parse(time.RFC3339Nano, *record.Metadata.Created)
	if err != nil || created.IsZero() || created.After(time.Now().Add(time.Minute)) {
		return nil, errInvalid
	}
	// Request IDs and optional mount metadata are not custody values. Preserve
	// the entire document plus every required original-version metadata field.
	var data map[string]any
	d := json.NewDecoder(bytes.NewReader(record.Data))
	d.UseNumber()
	if d.Decode(&data) != nil {
		return nil, errInvalid
	}
	return map[string]any{"data": data, "metadata": map[string]any{"version": json.Number("1"), "created_time": *record.Metadata.Created, "deletion_time": "", "destroyed": false}}, nil
}

// Expectation reads exactly the one fixed original record from the collector's private handoff.
func Expectation(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > 1<<20 || strictjson.Validate(raw, nil) != nil {
		return nil, errInvalid
	}
	var records map[string]json.RawMessage
	if json.Unmarshal(raw, &records) != nil || len(records) != 1 || records[Path] == nil {
		return nil, errInvalid
	}
	return RecordValue(records[Path])
}
