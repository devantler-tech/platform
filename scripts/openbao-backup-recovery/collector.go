package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const snapshotPrefix = "openbao-snapshots/"

// recoveryHTTP refuses ambient proxies, redirects and transparent decompression.
func recoveryHTTP() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, ResponseHeaderTimeout: 20 * time.Second, DisableCompression: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// signedRead uses the official AWS signer and never puts credentials in a URL or process.
func signedRead(ctx context.Context, client *http.Client, t target, path string, query url.Values, etag string, limit int64) (result []byte, headers http.Header, resultErr error) {
	u, err := url.Parse(t.Endpoint)
	if err != nil || !regexp.MustCompile(`^https://[a-f0-9]{32}(?:\.eu)?\.r2\.cloudflarestorage\.com$`).MatchString(t.Endpoint) || !strings.HasPrefix(path, "/"+t.Bucket) || limit <= 0 {
		return nil, nil, errEvidence
	}
	u.Path = path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, errEvidence
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	sum := sha256.Sum256(nil)
	if v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: t.AccessKey, SecretAccessKey: t.SecretKey}, req, hex.EncodeToString(sum[:]), "s3", "auto", time.Now().UTC()) != nil {
		return nil, nil, errEvidence
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, errEvidence
	}
	defer func() {
		if response.Body.Close() != nil {
			result, headers, resultErr = nil, nil, errEvidence
		}
	}()
	if response.StatusCode != http.StatusOK || response.ContentLength > limit {
		return nil, nil, errEvidence
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(b)) > limit || (response.ContentLength >= 0 && int64(len(b)) != response.ContentLength) {
		return nil, nil, errEvidence
	}
	return b, response.Header, nil
}

// downloadSnapshot admits a complete bounded listing and one immutable fresh object.
func downloadSnapshot(ctx context.Context, client *http.Client, t target, start, end time.Time) ([]byte, error) {
	b, _, err := signedRead(ctx, client, t, "/"+t.Bucket, url.Values{"list-type": {"2"}, "prefix": {snapshotPrefix}, "max-keys": {"256"}}, "", 1<<20)
	if err != nil {
		return nil, errEvidence
	}
	var listing struct {
		XMLName                             xml.Name
		Name, Prefix, NextContinuationToken string
		KeyCount, MaxKeys                   *int
		IsTruncated                         *bool
		Contents                            []struct {
			Key, ETag    string
			Size         int64
			LastModified time.Time
		}
	}
	decoder := xml.NewDecoder(bytes.NewReader(b))
	if decoder.Decode(&listing) != nil || decoder.Decode(new(any)) != io.EOF || listing.XMLName.Local != "ListBucketResult" || (listing.XMLName.Space != "" && listing.XMLName.Space != "http://s3.amazonaws.com/doc/2006-03-01/") || listing.Name != t.Bucket || listing.Prefix != snapshotPrefix || listing.IsTruncated == nil || *listing.IsTruncated || listing.NextContinuationToken != "" || listing.KeyCount == nil || *listing.KeyCount != len(listing.Contents) || listing.MaxKeys == nil || *listing.MaxKeys != 256 || len(listing.Contents) == 0 || len(listing.Contents) > 256 {
		return nil, errEvidence
	}
	etags := map[string]string{}
	var objects bytes.Buffer
	encoder := json.NewEncoder(&objects)
	for _, item := range listing.Contents {
		if !strings.HasPrefix(item.Key, snapshotPrefix) || !regexp.MustCompile(`^"[a-fA-F0-9]{32}(?:-[1-9][0-9]{0,5})?"$`).MatchString(item.ETag) {
			return nil, errEvidence
		}
		key := strings.TrimPrefix(item.Key, snapshotPrefix)
		if encoder.Encode(snapshotObject{Status: "success", Type: "file", Key: key, Size: item.Size, LastModified: item.LastModified}) != nil {
			return nil, errEvidence
		}
		etags[key] = item.ETag
	}
	selected, err := selectSnapshot(&objects, true, start, end)
	if err != nil {
		return nil, errEvidence
	}
	b, headers, err := signedRead(ctx, client, t, "/"+t.Bucket+"/"+snapshotPrefix+selected.Key, nil, etags[selected.Key], selected.Size)
	modified, stampErr := http.ParseTime(headers.Get("Last-Modified"))
	if err != nil || stampErr != nil || int64(len(b)) != selected.Size || headers.Get("ETag") != etags[selected.Key] || !modified.Equal(selected.LastModified.Truncate(time.Second)) {
		return nil, errEvidence
	}
	return b, nil
}

func allowedRecoveryObject(kind, namespace, name string) bool {
	switch namespace + "/" + kind + "/" + name {
	case "openbao/job/vault-snapshot-init", "flux-system/configmap/variables-base", "flux-system/configmap/variables-cluster", "flux-system/secret/variables-cluster", "openbao/secret/vault-snapshot-r2", "openbao/secret/openbao-unseal":
		return true
	default:
		return false
	}
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, errEvidence
	}
	return b.Buffer.Write(p)
}

// recoveryObject executes a fixed GET only, with bounded output and a fixed context.
func recoveryObject(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if !allowedRecoveryObject(kind, namespace, name) {
		return nil, errEvidence
	}
	command := exec.CommandContext(ctx, "kubectl", "--context=admin@prod", "--request-timeout=20s", "--namespace="+namespace, "get", kind, name, "--output=json")
	var output boundedOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	if command.Run() != nil || output.Len() == 0 {
		return nil, errEvidence
	}
	return output.Bytes(), nil
}

func recoveryDirectory(dir string) bool {
	if runtime.GOOS != "linux" || os.Getuid() == 0 || !strings.HasPrefix(dir, "/dev/shm/") || filepath.Clean(dir) != dir {
		return false
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil || real != dir {
		return false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) {
		return false
	}
	var fs syscall.Statfs_t
	return syscall.Statfs(dir, &fs) == nil && fs.Type == 0x01021994
}

// collectRecoveryInputs reads production only; every restore mutation stays in the sandbox.
func collectRecoveryInputs(dir string) error {
	if !recoveryDirectory(dir) {
		return errEvidence
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	job, err := recoveryObject(ctx, "job", "openbao", "vault-snapshot-init")
	if err != nil {
		return errEvidence
	}
	start, end, err := freshJobWindow(job, time.Now().UTC())
	if err != nil {
		return errEvidence
	}
	api, err := newAPI("http://127.0.0.1:38200")
	if err != nil {
		return errEvidence
	}
	health, err := api.health(ctx)
	if err != nil || !*health.Initialized || *health.Sealed {
		return errEvidence
	}
	base, err := recoveryObject(ctx, "configmap", "flux-system", "variables-base")
	if err != nil {
		return errEvidence
	}
	cluster, err := recoveryObject(ctx, "configmap", "flux-system", "variables-cluster")
	if err != nil {
		return errEvidence
	}
	variables, err := recoveryObject(ctx, "secret", "flux-system", "variables-cluster")
	if err != nil {
		return errEvidence
	}
	keys, err := recoveryObject(ctx, "secret", "openbao", "vault-snapshot-r2")
	if err != nil {
		return errEvidence
	}
	target, err := backupTarget(base, cluster, variables, keys)
	if err != nil {
		return errEvidence
	}
	recovery, err := recoveryObject(ctx, "secret", "openbao", "openbao-unseal")
	if err != nil {
		return errEvidence
	}
	values, err := fixedObject(recovery, "Secret", "openbao", "openbao-unseal")
	if err != nil {
		return errEvidence
	}
	root, key := values["root-token"], values["unseal-key"]
	if !regexp.MustCompile(`^[A-Za-z0-9._-]{20,256}$`).MatchString(root) || !regexp.MustCompile(`^[A-Za-z0-9+/=]{32,256}$`).MatchString(key) {
		return errEvidence
	}
	records, err := api.credentialRecords(ctx, root)
	if err != nil || !recordsEqual(records, records) {
		return errEvidence
	}
	expected, err := json.Marshal(records)
	if err != nil {
		return errEvidence
	}
	snapshot, err := downloadSnapshot(ctx, recoveryHTTP(), target, start, end)
	if err != nil {
		return errEvidence
	}
	for name, data := range map[string][]byte{"root-token": []byte(root), "unseal-key": []byte(key), "expected.json": expected, "snapshot.snap": snapshot} {
		if writePrivate(filepath.Join(dir, name), data) != nil {
			return errEvidence
		}
	}
	return nil
}
