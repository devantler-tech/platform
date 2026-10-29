package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const maxSnapshotBytes int64 = 256 << 20

var credentialPaths = []string{"apps/private-mail/admin", "apps/private-mail/maintainer", "apps/private-mail/agent"}
var snapshotName = regexp.MustCompile(`^openbao-[0-9]{8}-[0-9]{6}\.snap$`)
var errEvidence = errors.New("recovery evidence is incomplete or invalid")

type snapshotObject struct {
	Status       string    `json:"status"`
	Type         string    `json:"type"`
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

// selectSnapshot examines the entire bounded listing, rather than trusting its last line.
func selectSnapshot(input io.Reader, fresh bool, start, end time.Time) (snapshotObject, error) {
	var selected snapshotObject
	b, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil || len(b) > 1<<20 || (fresh && (start.IsZero() || end.Before(start))) {
		return selected, errEvidence
	}
	d := json.NewDecoder(bytes.NewReader(b))
	seen := map[string]bool{}
	for count := 0; ; count++ {
		var object snapshotObject
		err := d.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil || count >= 256 || object.Status != "success" || object.Type != "file" || !snapshotName.MatchString(object.Key) || seen[object.Key] || object.Size <= 0 || object.Size > maxSnapshotBytes || object.LastModified.IsZero() {
			return selected, errEvidence
		}
		stamp, err := time.Parse("20060102-150405", strings.TrimSuffix(strings.TrimPrefix(object.Key, "openbao-"), ".snap"))
		if err != nil || stamp.After(time.Now().UTC().Add(5*time.Minute)) || object.LastModified.After(time.Now().UTC().Add(5*time.Minute)) {
			return selected, errEvidence
		}
		seen[object.Key] = true
		if selected.Key == "" || object.Key > selected.Key {
			selected = object
		}
	}
	if selected.Key == "" {
		return selected, errEvidence
	}
	if fresh {
		stamp, err := time.Parse("20060102-150405", strings.TrimSuffix(strings.TrimPrefix(selected.Key, "openbao-"), ".snap"))
		if err != nil || stamp.Before(start.Add(-5*time.Second)) || stamp.After(end.Add(5*time.Second)) || selected.LastModified.Before(start.Add(-5*time.Second)) || selected.LastModified.After(end.Add(5*time.Second)) {
			return selected, errEvidence
		}
	}
	return selected, nil
}

// recordValue retains number spelling and requires a complete credential object.
func recordValue(raw json.RawMessage) (map[string]any, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v map[string]any
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || len(v) == 0 {
		return nil, false
	}
	// The declared PushSecrets seed a password-only record. Account identity is
	// bound by the fixed credential path; an optional username is metadata.
	password, ok := v["password"].(string)
	if !ok || !regexp.MustCompile(`^[!-~]{40,256}$`).MatchString(password) || strings.HasPrefix(password, "REPLACE_") || strings.HasPrefix(password, "CHANGE_ME") {
		return nil, false
	}
	if value, present := v["username"]; present {
		username, ok := value.(string)
		if !ok || !regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,254}$`).MatchString(username) {
			return nil, false
		}
	}
	return v, true
}

// recordsEqual requires every original field in all three fixed recovery records.
func recordsEqual(expected, actual map[string]json.RawMessage) bool {
	if len(expected) != len(credentialPaths) || len(actual) != len(credentialPaths) {
		return false
	}
	for _, path := range credentialPaths {
		e, ok1 := recordValue(expected[path])
		a, ok2 := recordValue(actual[path])
		if !ok1 || !ok2 || !reflect.DeepEqual(e, a) {
			return false
		}
	}
	return true
}

// privateInput refuses links, special files, shared permissions and truncated reads.
func privateInput(path string, limit int64) (result []byte, resultErr error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errEvidence
	}
	defer func() {
		if f.Close() != nil {
			result, resultErr = nil, errEvidence
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > limit {
		return nil, errEvidence
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) {
		return nil, errEvidence
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) != info.Size() || int64(len(b)) > limit {
		return nil, errEvidence
	}
	return b, nil
}
