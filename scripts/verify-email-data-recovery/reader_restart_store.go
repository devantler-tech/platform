package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const readerRestartDataPath = "/v1/secret/data/infrastructure/email/recovery/retained-reader-restart-witness"
const readerRestartMetadataPath = "/v1/secret/metadata/infrastructure/email/recovery/retained-reader-restart-witness"

// restartRecordMetadata requires explicit original version-one creation metadata.
func restartRecordMetadata(metadata object) (string, bool) {
	version, vok := metadata["version"].(float64)
	created, cok := metadata["created_time"].(string)
	deleted, dok := metadata["deletion_time"].(string)
	destroyed, bok := metadata["destroyed"].(bool)
	when, err := time.Parse(time.RFC3339Nano, created)
	if !vok || version != 1 || !cok || !dok || deleted != "" || !bok || destroyed || err != nil || when.IsZero() || when.After(time.Now().Add(time.Minute)) {
		return "", false
	}
	return created, true
}

// requestRestartWitness grants only fixed metadata reads and a CAS-zero write to the separate witness record.
func (s *readerCustodyStore) requestRestartWitness(ctx context.Context, method, path string, payload []byte) (object, int, error) {
	fail := func() (object, int, error) { return nil, 0, errors.New("reader-restart-request") }
	read := method == http.MethodGet && len(payload) == 0 && (path == "/v1/sys/mounts" || path == readerRestartDataPath || path == readerRestartMetadataPath)
	write := method == http.MethodPost && path == readerRestartDataPath && len(payload) > 0 && len(payload) <= 1<<20
	if write {
		var body object
		if strictjson.Validate(payload, nil) != nil || json.Unmarshal(payload, &body) != nil || len(body) != 2 || len(child(body, "options")) != 1 || child(body, "options")["cas"] != float64(0) || text(child(body, "data"), "schema") != restartWitnessSchema {
			return fail()
		}
	}
	if s == nil || s.client == nil || (!read && !write) {
		return fail()
	}
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return fail()
	}
	req.Header.Set("X-Vault-Token", s.token)
	if write {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(req)
	if err != nil {
		return fail()
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return fail()
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return object{}, response.StatusCode, nil
	}
	var value object
	if strictjson.Validate(raw, nil) != nil || json.Unmarshal(raw, &value) != nil || value == nil {
		return fail()
	}
	return value, response.StatusCode, nil
}

// createRestartWitness refuses existing custody and uncertain creates; native/source proof follows the last preflight and readback.
func (s *readerCustodyStore) createRestartWitness(ctx context.Context, witness object, verify func() error) (string, error) {
	fail := func() (string, error) { return "", errors.New("reader-restart-custody") }
	if verify == nil || text(witness, "schema") != restartWitnessSchema {
		return fail()
	}
	mounts, status, err := s.requestRestartWitness(ctx, http.MethodGet, "/v1/sys/mounts", nil)
	mount := child(child(mounts, "data"), "secret/")
	if err != nil || status != http.StatusOK || text(mount, "type") != "kv" || text(child(mount, "options"), "version") != "2" {
		return fail()
	}
	for _, path := range []string{readerRestartMetadataPath, readerRestartDataPath} {
		_, status, err := s.requestRestartWitness(ctx, http.MethodGet, path, nil)
		if err != nil || status != http.StatusNotFound {
			return fail()
		}
	}
	payload, err := json.Marshal(object{"options": object{"cas": 0}, "data": witness})
	if err != nil || verify() != nil {
		return fail()
	}
	// Never retry this POST, including an unreadable or lost response. The
	// record is retained for a separately reviewed observation on refusal.
	ack, status, err := s.requestRestartWitness(ctx, http.MethodPost, readerRestartDataPath, payload)
	created, valid := restartRecordMetadata(child(ack, "data"))
	if err != nil || status != http.StatusOK || !valid {
		return fail()
	}
	readback, status, err := s.requestRestartWitness(ctx, http.MethodGet, readerRestartDataPath, nil)
	data := child(readback, "data")
	readCreated, valid := restartRecordMetadata(child(data, "metadata"))
	if err != nil || status != http.StatusOK || !valid || created != readCreated || !reflect.DeepEqual(child(data, "data"), witness) || verify() != nil {
		return fail()
	}
	canonical, err := json.Marshal(object{"data": witness, "metadata": object{"version": 1, "created_time": created, "deletion_time": "", "destroyed": false}})
	if err != nil {
		return fail()
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
