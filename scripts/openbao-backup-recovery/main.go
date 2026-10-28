// openbao-backup-recovery restores only inside the fixed isolated sandbox.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This is the actual server release and digest observed on all three servers,
// rather than the older CLI-only image used by the snapshot client. A changed
// running release must be explicitly rebound before a production proof runs.
const serverVersion = "2.6.3"
const serverImage = "quay.io/openbao/openbao:2.6.3@sha256:a60afafda36337abe833c4a63894bf1095098f29abea4091e7e555a33dd52889"
const localAPI = "http://127.0.0.1:8200"

type stageError string

func (e stageError) Error() string { return string(e) }

type baoAPI struct {
	base   string
	client *http.Client
}

// newAPI admits loopback only and refuses redirects and ambient proxy settings.
func newAPI(base string) (*baoAPI, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errEvidence
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, ResponseHeaderTimeout: 10 * time.Second, DisableKeepAlives: true}
	return &baoAPI{base: base, client: &http.Client{Transport: transport, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// request never returns a remote error body as a diagnostic.
func (a *baoAPI) request(ctx context.Context, method, path, token string, body io.Reader, limit int64) (int, []byte, error) {
	if !strings.HasPrefix(path, "/v1/") || strings.ContainsAny(path, "?#\r\n") {
		return 0, nil, errEvidence
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return 0, nil, errEvidence
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	if path == "/v1/sys/storage/raft/snapshot-force" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	response, err := a.client.Do(req)
	if err != nil {
		return 0, nil, errEvidence
	}
	b, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || int64(len(b)) > limit {
		return 0, nil, errEvidence
	}
	return response.StatusCode, b, nil
}

// writePrivate creates an owned handoff once; an existing path is never replaced.
func writePrivate(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errEvidence
	}
	_, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errEvidence
	}
	return nil
}

type healthState struct {
	Initialized *bool  `json:"initialized"`
	Sealed      *bool  `json:"sealed"`
	Version     string `json:"version"`
}

// health requires explicit initialization and seal fields from the pinned server.
func (a *baoAPI) health(ctx context.Context) (healthState, error) {
	var h healthState
	status, b, err := a.request(ctx, http.MethodGet, "/v1/sys/health", "", nil, 8192)
	if err != nil || (status != 200 && status != 501 && status != 503) || json.Unmarshal(b, &h) != nil || h.Initialized == nil || h.Sealed == nil || h.Version != serverVersion {
		return h, errEvidence
	}
	return h, nil
}

// initialize creates only the fresh temporary sandbox's own seal and root token.
func (a *baoAPI) initialize(ctx context.Context) (string, string, error) {
	deadline := time.Now().Add(20 * time.Second)
	for {
		h, err := a.health(ctx)
		if err == nil {
			if *h.Initialized {
				return "", "", errEvidence
			}
			break
		}
		if time.Now().After(deadline) {
			return "", "", errEvidence
		}
		time.Sleep(100 * time.Millisecond)
	}
	status, b, err := a.request(ctx, http.MethodPut, "/v1/sys/init", "", strings.NewReader(`{"secret_shares":1,"secret_threshold":1}`), 16384)
	var initialized struct {
		Root string   `json:"root_token"`
		Keys []string `json:"keys_base64"`
	}
	if err != nil || status != 200 || json.Unmarshal(b, &initialized) != nil || len(initialized.Keys) != 1 || initialized.Root == "" || initialized.Keys[0] == "" {
		return "", "", errEvidence
	}
	if a.unseal(ctx, initialized.Keys[0]) != nil {
		return "", "", errEvidence
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		h, err := a.health(ctx)
		if err == nil && *h.Initialized && !*h.Sealed {
			return initialized.Root, initialized.Keys[0], nil
		}
		if time.Now().After(deadline) {
			return "", "", errEvidence
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// unseal sends a key only over the fixed private loopback API, never in argv.
func (a *baoAPI) unseal(ctx context.Context, key string) error {
	body, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return errEvidence
	}
	status, _, err := a.request(ctx, http.MethodPut, "/v1/sys/unseal", "", bytes.NewReader(body), 16384)
	if err != nil || status != 200 {
		return errEvidence
	}
	return nil
}

// credentialRecords reads every fixed KV-v2 record and rejects deleted values.
func (a *baoAPI) credentialRecords(ctx context.Context, token string) (map[string]json.RawMessage, error) {
	records := map[string]json.RawMessage{}
	for _, path := range credentialPaths {
		status, b, err := a.request(ctx, http.MethodGet, "/v1/secret/data/"+path, token, nil, 1<<20)
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
		if err != nil || status != 200 || json.Unmarshal(b, &response) != nil || response.Data.Metadata.Version < 1 || response.Data.Metadata.Deleted != "" || response.Data.Metadata.Destroyed {
			return nil, errEvidence
		}
		if _, ok := recordValue(response.Data.Data); !ok {
			return nil, errEvidence
		}
		records[path] = response.Data.Data
	}
	return records, nil
}

// isolatedMembership refuses a restored configuration that still needs remote peers.
func (a *baoAPI) isolatedMembership(ctx context.Context, token string) bool {
	status, b, err := a.request(ctx, http.MethodGet, "/v1/sys/storage/raft/configuration", token, nil, 16384)
	var response struct {
		Data struct {
			Config struct {
				Servers []struct {
					ID            string `json:"node_id"`
					Address       string `json:"address"`
					Leader, Voter bool
				} `json:"servers"`
			} `json:"config"`
		} `json:"data"`
	}
	if err != nil || status != 200 || json.Unmarshal(b, &response) != nil || len(response.Data.Config.Servers) != 1 {
		return false
	}
	s := response.Data.Config.Servers[0]
	return s.ID == "isolated-restore-proof" && s.Address == "127.0.0.1:8201" && s.Leader && s.Voter
}

// restore verifies the old recovery keys and every record after one isolated restore.
func (a *baoAPI) restore(ctx context.Context, temporaryRoot, dir string) error {
	root, err := privateInput(filepath.Join(dir, "root-token"), 512)
	if err != nil {
		return stageError("inputs")
	}
	key, err := privateInput(filepath.Join(dir, "unseal-key"), 512)
	if err != nil {
		return stageError("inputs")
	}
	validToken := regexp.MustCompile(`^[A-Za-z0-9._-]{20,256}$`)
	validKey := regexp.MustCompile(`^[A-Za-z0-9+/=]{32,256}$`)
	recoveryRoot := strings.TrimSpace(string(root))
	recoveryKey := strings.TrimSpace(string(key))
	if !validToken.MatchString(recoveryRoot) || !validKey.MatchString(recoveryKey) || recoveryRoot == temporaryRoot {
		return stageError("inputs")
	}
	b, err := privateInput(filepath.Join(dir, "expected.json"), 1<<20)
	if err != nil {
		return stageError("inputs")
	}
	var expected map[string]json.RawMessage
	if json.Unmarshal(b, &expected) != nil || !recordsEqual(expected, expected) {
		return stageError("inputs")
	}
	snapshot, err := privateInput(filepath.Join(dir, "snapshot.snap"), maxSnapshotBytes)
	if err != nil {
		return stageError("snapshot")
	}
	status, _, err := a.request(ctx, http.MethodPost, "/v1/sys/storage/raft/snapshot-force", temporaryRoot, bytes.NewReader(snapshot), 16384)
	if err != nil || status != 204 {
		return stageError("restore")
	}
	// The restore endpoint returns before its background restore finishes. The
	// production root token must replace the fresh temporary root before success.
	deadline := time.Now().Add(90 * time.Second)
	for {
		h, err := a.health(ctx)
		if err == nil && *h.Initialized {
			if *h.Sealed {
				if a.unseal(ctx, recoveryKey) != nil {
					return stageError("unseal")
				}
			} else {
				actual, err := a.credentialRecords(ctx, recoveryRoot)
				if err == nil {
					if !recordsEqual(expected, actual) {
						return stageError("records")
					}
					if !a.isolatedMembership(ctx, recoveryRoot) {
						return stageError("membership")
					}
					return a.confirmRecoveryKey(ctx, recoveryRoot, recoveryKey, expected)
				}
			}
		}
		if time.Now().After(deadline) {
			return stageError("restored_state")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// confirmRecoveryKey forces a fresh isolated unseal even if restore auto-unsealed.
func (a *baoAPI) confirmRecoveryKey(ctx context.Context, root, key string, expected map[string]json.RawMessage) error {
	status, _, err := a.request(ctx, http.MethodPut, "/v1/sys/seal", root, nil, 16384)
	if err != nil || status != 204 {
		return stageError("seal")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		h, err := a.health(ctx)
		if err == nil && *h.Initialized && *h.Sealed {
			break
		}
		if time.Now().After(deadline) {
			return stageError("seal")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if a.unseal(ctx, key) != nil {
		return stageError("unseal")
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		h, err := a.health(ctx)
		if err == nil && *h.Initialized && !*h.Sealed {
			actual, err := a.credentialRecords(ctx, root)
			if err == nil && recordsEqual(expected, actual) && a.isolatedMembership(ctx, root) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return stageError("unseal")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// createFixture emits only synthetic records and random sandbox-owned recovery keys.
func (a *baoAPI) createFixture(ctx context.Context, root, key, dir string) error {
	status, _, err := a.request(ctx, http.MethodPost, "/v1/sys/mounts/secret", root, strings.NewReader(`{"type":"kv","options":{"version":"2"}}`), 16384)
	if err != nil || status != 204 {
		return stageError("fixture")
	}
	expected := map[string]json.RawMessage{}
	for i, path := range credentialPaths {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return stageError("fixture")
		}
		data := map[string]any{"username": fmt.Sprintf("synthetic-%d", i), "password": hex.EncodeToString(random), "retainedField": "synthetic-fixture"}
		raw, err := json.Marshal(data)
		if err != nil {
			return stageError("fixture")
		}
		expected[path] = raw
		body, err := json.Marshal(map[string]any{"data": data})
		if err != nil {
			return stageError("fixture")
		}
		status, _, err := a.request(ctx, http.MethodPost, "/v1/secret/data/"+path, root, bytes.NewReader(body), 16384)
		if err != nil || status != 200 {
			return stageError("fixture")
		}
	}
	b, err := json.Marshal(expected)
	if err != nil {
		return stageError("fixture")
	}
	for name, data := range map[string][]byte{"root-token": []byte(root), "unseal-key": []byte(key), "expected.json": b} {
		if writePrivate(filepath.Join(dir, name), data) != nil {
			return stageError("fixture")
		}
	}
	status, snapshot, err := a.request(ctx, http.MethodGet, "/v1/sys/storage/raft/snapshot", root, nil, maxSnapshotBytes)
	if err != nil || status != 200 || len(snapshot) == 0 || writePrivate(filepath.Join(dir, "snapshot.snap"), snapshot) != nil {
		return stageError("fixture")
	}
	return nil
}

// sandbox owns one child process; no caller can select a server or API target.
func sandbox(mode string) error {
	if mode != "verify" && (mode != "fixture" || os.Getenv("OPENBAO_RECOVERY_SYNTHETIC") != "1") {
		return stageError("mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	entries, err := os.ReadDir("/work")
	if err != nil || len(entries) != 0 {
		return stageError("sandbox")
	}
	if os.Mkdir("/work/data", 0700) != nil {
		return stageError("sandbox")
	}
	configuration := `disable_mlock = true
api_addr = "http://127.0.0.1:8200"
cluster_addr = "http://127.0.0.1:8201"
listener "tcp" {
  address = "127.0.0.1:8200"
  tls_disable = true
  max_request_size = 300000000
}
storage "raft" {
  path = "/work/data"
  node_id = "isolated-restore-proof"
  performance_multiplier = 1
}
audit "file" {
  type = "file"
  path = "file/"
  options = {
    file_path = "/openbao/audit/audit.log"
  }
}
`
	if writePrivate("/work/server.hcl", []byte(configuration)) != nil {
		return stageError("sandbox")
	}
	child := exec.CommandContext(ctx, "/usr/bin/bao", "server", "-config=/work/server.hcl", "-log-level=error")
	child.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/work", "TMPDIR=/work"}
	child.Stdout = io.Discard
	child.Stderr = io.Discard
	if child.Start() != nil {
		return stageError("server")
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer func() { _ = child.Process.Kill(); <-done }()
	a, err := newAPI(localAPI)
	if err != nil {
		return stageError("sandbox")
	}
	root, key, err := a.initialize(ctx)
	if err != nil {
		return stageError("initialization")
	}
	if mode == "fixture" {
		return a.createFixture(ctx, root, key, "/export")
	}
	return a.restore(ctx, root, "/proof")
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "image" {
		fmt.Println(serverImage)
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "OPENBAO_BACKUP_RESTORE=FAIL stage=mode")
		os.Exit(2)
	}
	err := sandbox(os.Args[1])
	if err != nil {
		stage := "verification"
		var known stageError
		if errors.As(err, &known) {
			stage = string(known)
		}
		fmt.Printf("OPENBAO_BACKUP_RESTORE=FAIL stage=%s\n", stage)
		os.Exit(2)
	}
	if os.Args[1] == "fixture" {
		fmt.Println("OPENBAO_BACKUP_FIXTURE=PASS")
	} else {
		fmt.Printf("OPENBAO_BACKUP_RESTORE=PASS records=3 version=%s\n", serverVersion)
	}
}
