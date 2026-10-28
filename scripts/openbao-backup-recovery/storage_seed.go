package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
)

func (a *baoAPI) storageRecord(ctx context.Context, token string) (json.RawMessage, int64, int, error) {
	status, raw, err := a.request(ctx, http.MethodGet, "/v1/secret/data/"+storageCredentialPath, token, nil, 16384)
	defer clear(raw)
	if err != nil || status != http.StatusOK {
		return nil, 0, status, errEvidence
	}
	var response struct {
		Data struct {
			Data     json.RawMessage `json:"data"`
			Metadata struct {
				Version   int64  `json:"version"`
				Deleted   string `json:"deletion_time"`
				Destroyed bool   `json:"destroyed"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Data.Metadata.Version < 1 || response.Data.Metadata.Deleted != "" || response.Data.Metadata.Destroyed {
		return nil, 0, status, errEvidence
	}
	if _, ok := recordValueAtPath(storageCredentialPath, response.Data.Data); !ok {
		return nil, 0, status, errEvidence
	}
	return response.Data.Data, response.Data.Metadata.Version, status, nil
}

// prepareStorage never rotates a key, overwrites history or retries an uncertain write.
// Its expected account records must already have survived independent recovery.
func (a *baoAPI) prepareStorage(ctx context.Context, token string, expected map[string]json.RawMessage, entropy io.Reader) (string, error) {
	canonical := []string{"infrastructure/email/admin", "infrastructure/email/maintainer", "infrastructure/email/agent"}
	if !slices.Equal(credentialPaths, canonical) || !recordsEqual(expected, expected) || entropy == nil || !regexp.MustCompile(`^[A-Za-z0-9._-]{20,256}$`).MatchString(token) {
		return "", errEvidence
	}
	health, err := a.health(ctx)
	if err != nil || !*health.Initialized || *health.Sealed {
		return "", errEvidence
	}
	status, raw, err := a.request(ctx, http.MethodGet, "/v1/sys/mounts", token, nil, 1<<20)
	var mounts struct {
		Data map[string]struct {
			Type    string            `json:"type"`
			Options map[string]string `json:"options"`
		} `json:"data"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(raw, &mounts) != nil || mounts.Data["secret/"].Type != "kv" || mounts.Data["secret/"].Options["version"] != "2" {
		return "", errEvidence
	}
	before, err := a.credentialRecords(ctx, token)
	if err != nil || !recordsEqual(expected, before) {
		return "", errEvidence
	}
	value, version, status, err := a.storageRecord(ctx, token)
	defer func() { clear(value) }()
	state := "EXISTING"
	if err != nil {
		if status != http.StatusNotFound {
			return "", errEvidence
		}
		// A deleted or destroyed value is not a fresh key. Never recreate history.
		status, _, err = a.request(ctx, http.MethodGet, "/v1/secret/metadata/"+storageCredentialPath, token, nil, 16384)
		if err != nil || status != http.StatusNotFound {
			return "", errEvidence
		}
		random := make([]byte, 32)
		if _, err := io.ReadFull(entropy, random); err != nil {
			clear(random)
			return "", errEvidence
		}
		value, err = json.Marshal(map[string]string{"passphrase": hex.EncodeToString(random)})
		clear(random)
		if err != nil {
			return "", errEvidence
		}
		body, err := json.Marshal(map[string]any{"data": json.RawMessage(value), "options": map[string]int{"cas": 0}})
		if err != nil {
			return "", errEvidence
		}
		status, raw, err = a.request(ctx, http.MethodPost, "/v1/secret/data/"+storageCredentialPath, token, bytes.NewReader(body), 16384)
		clear(body)
		var created struct {
			Data struct{ Version int64 } `json:"data"`
		}
		if err != nil || status != http.StatusOK || json.Unmarshal(raw, &created) != nil || created.Data.Version != 1 {
			return "", errEvidence
		}
		version, state = 1, "CREATED"
	}
	readback, afterVersion, _, err := a.storageRecord(ctx, token)
	defer clear(readback)
	left, ok1 := recordValueAtPath(storageCredentialPath, value)
	right, ok2 := recordValueAtPath(storageCredentialPath, readback)
	if err != nil || !ok1 || !ok2 || afterVersion != version || !reflect.DeepEqual(left, right) {
		return "", errEvidence
	}
	after, err := a.credentialRecords(ctx, token)
	if err != nil || !recordsEqual(expected, after) {
		return "", errEvidence
	}
	return state, nil
}
