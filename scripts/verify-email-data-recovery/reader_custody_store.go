package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/devantler-tech/platform/internal/emailreaderinputs"
	"github.com/devantler-tech/platform/internal/strictjson"
)

const retainedReaderDataPath = "/v1/secret/data/" + emailreaderinputs.Path
const retainedReaderMetadataPath = "/v1/secret/metadata/" + emailreaderinputs.Path

type readerCustodyStore struct {
	client   *http.Client
	endpoint string
	token    string
}

// Only the protected workflow's loopback tunnel is accepted. No environment
// proxy, redirect, URL credentials or arbitrary remote destination receives
// the OpenBao token. The caller pins the production tunnel's exact port.
func newReaderCustodyStore(endpoint, token string) (*readerCustodyStore, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || !regexp.MustCompile(`^[A-Za-z0-9._-]{16,256}$`).MatchString(token) {
		return nil, errors.New("custody-endpoint")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("custody-endpoint")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, MaxResponseHeaderBytes: 64 << 10}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &readerCustodyStore{client: client, endpoint: endpoint, token: token}, nil
}

// readerCustodyDocument binds the complete copy-specific input set to its content digests and fixed record schema.
func readerCustodyDocument(bundle retainedReaderInputs) (object, error) {
	fail := func() (object, error) { return nil, errors.New("custody-document") }
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !uuid.MatchString(bundle.ReaderUID) || !uuid.MatchString(bundle.ClaimUID) || bundle.ReaderUID == bundle.ClaimUID || len(bundle.Inputs) != 8 {
		return fail()
	}
	allowed := map[string]bool{}
	for _, want := range append(recoveryResources(retainedRecovery, "", ""), recoveryReader(retainedRecovery, "")) {
		m := child(want, "metadata")
		allowed[(ownedReference{kind: text(want, "kind"), name: text(m, "name"), namespace: text(m, "namespace")}).key()] = true
	}
	seen := map[string]bool{}
	for _, input := range bundle.Inputs {
		ref := ownedReference{kind: input.Kind, name: input.Name, namespace: input.Namespace}
		if !allowed[ref.key()] || !uuid.MatchString(input.UID) || seen[input.UID] || input.Values == nil || (input.Kind == "Pod" && input.UID != bundle.ReaderUID) || (input.Kind == "PersistentVolumeClaim" && input.UID != bundle.ClaimUID) {
			return fail()
		}
		delete(allowed, ref.key())
		seen[input.UID] = true
		raw, err := json.Marshal(input.Values)
		if err != nil {
			return fail()
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != input.Digest {
			return fail()
		}
	}
	raw, err := json.Marshal(bundle)
	if err != nil || len(raw) > 512<<10 {
		return fail()
	}
	var value object
	if json.Unmarshal(raw, &value) != nil {
		return fail()
	}
	document := object{"schema": "email-reader-inputs/v1", "trial_run": retainedRecovery.run, "trial_revision": retainedRecovery.revision, "bundle": value}
	raw, err = json.Marshal(document)
	if err != nil || emailreaderinputs.ValidateDocument(raw) != nil {
		return fail()
	}
	return document, nil
}

// ensure creates the one fixed record with CAS=0, or verifies an identical
// original version. It has no update, delete, metadata-write or retry-write
// path. verify runs after the final API preflight, immediately before POST,
// then again after readback. A stored record alone authorizes no lifecycle.
func (s *readerCustodyStore) ensure(ctx context.Context, bundle retainedReaderInputs, verify func(context.Context) error) error {
	fail := func() error { return errors.New("reader-custody") }
	expected, err := readerCustodyDocument(bundle)
	if err != nil || verify == nil || s == nil || s.client == nil || ctx.Err() != nil {
		return fail()
	}
	mounts, status, err := s.request(ctx, http.MethodGet, "/v1/sys/mounts", nil)
	mount := child(child(mounts, "data"), "secret/")
	if err != nil || status != http.StatusOK || text(mount, "type") != "kv" || text(child(mount, "options"), "version") != "2" {
		return fail()
	}
	found, err := s.matches(ctx, expected)
	if err != nil {
		return fail()
	}
	if !found {
		// A soft-deleted value is not a new key. Preserve its metadata and
		// refuse before any write, rather than trying to replace its history.
		_, status, err = s.request(ctx, http.MethodGet, retainedReaderMetadataPath, nil)
		if err != nil || status != http.StatusNotFound {
			return fail()
		}
	}
	if verify(ctx) != nil {
		return fail()
	}
	if !found {
		payload, err := json.Marshal(object{"options": object{"cas": 0}, "data": expected})
		if err != nil {
			return fail()
		}
		_, status, err = s.request(ctx, http.MethodPost, retainedReaderDataPath, payload)
		// A lost or failed response is uncertain and is never retried. A CAS
		// conflict can succeed only by reading the identical original record.
		if err != nil || (status != http.StatusOK && status != http.StatusBadRequest) {
			return fail()
		}
	}
	found, err = s.matches(ctx, expected)
	if err != nil || !found || verify(ctx) != nil {
		return fail()
	}
	return nil
}

// matches accepts only an identical, undeleted original KV2 version; missing data is not recovery proof.
func (s *readerCustodyStore) matches(ctx context.Context, expected object) (bool, error) {
	o, status, err := s.request(ctx, http.MethodGet, retainedReaderDataPath, nil)
	if err != nil {
		return false, errors.New("custody-readback")
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	data := child(o, "data")
	metadata := child(data, "metadata")
	deleted, hasDeleted := metadata["deletion_time"].(string)
	destroyed, hasDestroyed := metadata["destroyed"].(bool)
	_, timeErr := time.Parse(time.RFC3339Nano, text(metadata, "created_time"))
	actualBytes, actualErr := json.Marshal(data["data"])
	expectedBytes, expectedErr := json.Marshal(expected)
	if status != http.StatusOK || metadata["version"] != float64(1) || !hasDeleted || deleted != "" || !hasDestroyed || destroyed || timeErr != nil || actualErr != nil || expectedErr != nil || !bytes.Equal(actualBytes, expectedBytes) {
		return false, errors.New("custody-readback")
	}
	return true, nil
}

// request restricts custody HTTP methods and destinations and rejects ambiguous or oversized responses.
func (s *readerCustodyStore) request(ctx context.Context, method, path string, payload []byte) (object, int, error) {
	fail := func() (object, int, error) { return nil, 0, errors.New("custody-request") }
	if (method != http.MethodGet || (path != "/v1/sys/mounts" && path != retainedReaderDataPath && path != retainedReaderMetadataPath)) && (method != http.MethodPost || path != retainedReaderDataPath) {
		return fail()
	}
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return fail()
	}
	req.Header.Set("X-Vault-Token", s.token)
	if method == http.MethodPost {
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
	if strictjson.Validate(raw, nil) != nil {
		return fail()
	}
	var value object
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return fail()
	}
	return value, response.StatusCode, nil
}
