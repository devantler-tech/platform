package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type failedClose struct{ io.Reader }

func (failedClose) Close() error { return errors.New("synthetic incomplete response") }

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIndependentSnapshotReadRequiresSignedFreshBoundedBytes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	name := "openbao-" + now.Format("20060102-150405") + ".snap"
	etag := `"` + strings.Repeat("a", 32) + `"`
	base := fmt.Sprintf(`<ListBucketResult><Name>fixture-backups</Name><Prefix>openbao-snapshots/</Prefix><KeyCount>1</KeyCount><MaxKeys>256</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>openbao-snapshots/%s</Key><LastModified>%s</LastModified><ETag>%s</ETag><Size>8</Size></Contents></ListBucketResult>`, name, now.Format(time.RFC3339), etag)
	for _, tc := range []string{"matching", "truncated", "wrong-prefix", "duplicate", "old", "wrong-size", "changed-object", "redirect", "oversized", "trailing-xml", "close-failure"} {
		t.Run(tc, func(t *testing.T) {
			listing := base
			size := int64(8)
			gotETag := etag
			status := http.StatusOK
			body := "snapshot"
			gets := 0
			switch tc {
			case "truncated":
				listing = strings.Replace(listing, "<IsTruncated>false", "<IsTruncated>true", 1)
			case "wrong-prefix":
				listing = strings.ReplaceAll(listing, "openbao-snapshots/", "other/")
			case "duplicate":
				listing = strings.Replace(listing, "</ListBucketResult>", listing[strings.Index(listing, "<Contents>"):strings.Index(listing, "</Contents>")+len("</Contents>")]+"</ListBucketResult>", 1)
			case "old":
				listing = strings.ReplaceAll(listing, now.Format(time.RFC3339), now.Add(-48*time.Hour).Format(time.RFC3339))
			case "wrong-size":
				size = 9
			case "changed-object":
				gotETag = `"` + strings.Repeat("b", 32) + `"`
			case "redirect":
				status = http.StatusTemporaryRedirect
			case "oversized":
				listing = strings.Repeat("x", (1<<20)+1)
			case "trailing-xml":
				listing += "<unexpected/>"
			}
			client := &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("X-Amz-Content-Sha256") != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
					t.Fatal("S3 read is missing the required empty-payload hash header")
				}
				if r.Method != http.MethodGet || r.URL.Host != strings.Repeat("c", 32)+".r2.cloudflarestorage.com" || !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential="+strings.Repeat("d", 32)+"/") {
					t.Fatal("request escaped its signed read-only origin")
				}
				h := make(http.Header)
				content := body
				n := size
				if r.URL.Query().Get("list-type") == "2" {
					if r.URL.Query().Get("prefix") != "openbao-snapshots/" || r.URL.Query().Get("max-keys") != "256" || r.URL.Path != "/fixture-backups" {
						t.Fatal("unexpected list scope")
					}
					content = listing
					n = int64(len(content))
				} else {
					gets++
					if r.URL.Path != "/fixture-backups/openbao-snapshots/"+name || r.Header.Get("If-Match") != etag {
						t.Fatal("object is not bound to listing")
					}
					h.Set("ETag", gotETag)
					h.Set("Last-Modified", now.Format(http.TimeFormat))
				}
				body := io.NopCloser(strings.NewReader(content))
				if tc == "close-failure" {
					body = failedClose{strings.NewReader(content)}
				}
				return &http.Response{StatusCode: status, Header: h, Body: body, ContentLength: n, Request: r}, nil
			})}
			target := target{Endpoint: "https://" + strings.Repeat("c", 32) + ".r2.cloudflarestorage.com", Bucket: "fixture-backups", AccessKey: strings.Repeat("d", 32), SecretKey: strings.Repeat("e", 64)}
			b, err := downloadSnapshot(context.Background(), client, target, now.Add(-time.Minute), now.Add(time.Minute))
			if (err == nil) != (tc == "matching") || (err == nil && !bytes.Equal(b, []byte("snapshot"))) {
				t.Fatal("incorrect independent snapshot verdict")
			}
			if tc == "matching" && gets != 1 {
				t.Fatal("did not download the actual object")
			}
			if tc != "matching" && len(b) != 0 {
				t.Fatal("refused input exposed partial content")
			}
		})
	}
}

func TestIndependentProductionReaderLimitsKubernetesObjects(t *testing.T) {
	for _, item := range []struct {
		k, ns, n string
		ok       bool
	}{
		{"job", "openbao", "vault-snapshot-init", true},
		{"configmap", "flux-system", "variables-base", true},
		{"configmap", "flux-system", "variables-cluster", true},
		{"secret", "flux-system", "variables-cluster", true},
		{"secret", "openbao", "vault-snapshot-r2", true},
		{"secret", "openbao", "openbao-unseal", true},
		{"secret", "openbao", "other", false},
		{"pods", "openbao", "openbao-0", false},
		{"secret", "other", "openbao-unseal", false},
	} {
		if allowedRecoveryObject(item.k, item.ns, item.n) != item.ok {
			t.Fatal("incorrect fixed Kubernetes read boundary")
		}
	}
}

// TestSnapshotTransportNeverForwardsSignedReads exercises real local HTTP connections.
func TestSnapshotTransportNeverForwardsSignedReads(t *testing.T) {
	var proxied, followed bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied = true; w.WriteHeader(http.StatusNoContent) }))
	defer proxy.Close()
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true; w.WriteHeader(http.StatusOK) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	sourceURL, err := url.Parse(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := recoveryHTTP()
	transport := client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "recovery-fixture.invalid:80" {
			address = sourceURL.Host
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://recovery-fixture.invalid/", nil)
	if err != nil {
		t.Fatal("could not prepare local transport request")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("could not exercise local transport")
	}
	if response.Body.Close() != nil {
		t.Fatal("could not finish local response")
	}
	if response.StatusCode != http.StatusTemporaryRedirect || proxied || followed {
		t.Fatal("transport forwarded a request through an ambient proxy or redirect")
	}
}
