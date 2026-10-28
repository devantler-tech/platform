package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCredentialDownloadBindsTheCompletedArchive exercises signing, immutable reads and complete archive admission.
func TestCredentialDownloadBindsTheCompletedArchive(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	root, unseal := strings.Repeat("r", 40), strings.Repeat("u", 44)
	archive := credentialArchive(t, credentialArchiveEntry{name: legacyKeyPath, body: credentialSecret(t, root, unseal)})
	key := "velero/prod/backups/fixture-complete/fixture-complete.tar.gz"
	etag := `"` + strings.Repeat("a", 32) + `"`
	for _, scenario := range []string{"matching", "changed-object", "wrong-object", "truncated-list", "wrong-window", "short-read", "failed-close", "corrupt-archive"} {
		t.Run(scenario, func(t *testing.T) {
			listingKey, stamp, gotETag, truncated := key, now, etag, false
			body := append([]byte(nil), archive...)
			switch scenario {
			case "changed-object":
				gotETag = `"` + strings.Repeat("b", 32) + `"`
			case "wrong-object":
				listingKey = "unrelated/fixture-complete.tar.gz"
			case "truncated-list":
				truncated = true
			case "wrong-window":
				stamp = now.Add(-48 * time.Hour)
			case "corrupt-archive":
				body[len(body)-8] ^= 1
			}
			listing := fmt.Sprintf(`<ListBucketResult><Name>fixture-backups</Name><Prefix>%s</Prefix><KeyCount>1</KeyCount><MaxKeys>2</MaxKeys><IsTruncated>%t</IsTruncated><Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>%s</ETag><Size>%d</Size></Contents></ListBucketResult>`, key, truncated, listingKey, stamp.Format(time.RFC3339), etag, len(body))
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Host != strings.Repeat("c", 32)+".r2.cloudflarestorage.com" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Fatal("archive request escaped its signed read-only origin")
				}
				h := make(http.Header)
				content := []byte(listing)
				if r.URL.Query().Get("list-type") == "2" {
					if r.URL.Query().Get("prefix") != key || r.URL.Query().Get("max-keys") != "2" {
						t.Fatal("archive listing was not bounded to its exact object")
					}
				} else {
					if r.URL.Path != "/fixture-backups/"+key || r.Header.Get("If-Match") != etag {
						t.Fatal("archive GET was not bound to the listing")
					}
					content = body
					h.Set("ETag", gotETag)
					h.Set("Last-Modified", stamp.Format(http.TimeFormat))
				}
				size := int64(len(content))
				if scenario == "short-read" && r.URL.Query().Get("list-type") == "" {
					size++
				}
				responseBody := io.NopCloser(bytes.NewReader(content))
				if scenario == "failed-close" {
					responseBody = failedClose{bytes.NewReader(content)}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: h, Body: responseBody, ContentLength: size, Request: r}, nil
			})}
			target := target{Endpoint: "https://" + strings.Repeat("c", 32) + ".r2.cloudflarestorage.com", Bucket: "fixture-backups", AccessKey: strings.Repeat("d", 32), SecretKey: strings.Repeat("e", 64)}
			got, err := downloadRecoveryCredentials(context.Background(), client, target, "velero/prod", credentialBackup{Name: "fixture-complete", Start: now.Add(-time.Hour), End: now.Add(time.Minute)})
			if scenario == "matching" {
				if err != nil || got.Root != root || got.Unseal != unseal {
					t.Fatal("independent archive credentials were not recovered")
				}
			} else if err == nil || got.Root != "" || got.Unseal != "" {
				t.Fatal("unverified remote archive released credentials")
			}
		})
	}
}
