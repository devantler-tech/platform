package backupcatalogue

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
)

func repositoryHTTPFixture(t *testing.T, r repo.Repository) http.RoundTripper {
	t.Helper()
	backend := r.(repo.DirectRepository).BlobReader()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		ctx := request.Context()
		prefix := "/backup-fixture/velero/kopia/fixture/"
		if request.URL.Query().Get("list-type") == "2" {
			fullPrefix := request.URL.Query().Get("prefix")
			var records []blob.Metadata
			if err := backend.ListBlobs(ctx, blob.ID(strings.TrimPrefix(fullPrefix, "velero/kopia/fixture/")), func(m blob.Metadata) error { records = append(records, m); return nil }); err != nil {
				return nil, err
			}
			slices.SortFunc(records, func(a, b blob.Metadata) int { return strings.Compare(string(a.BlobID), string(b.BlobID)) })
			var data bytes.Buffer
			data.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>backup-fixture</Name><Prefix>`)
			_ = xml.EscapeText(&data, []byte(url.QueryEscape(fullPrefix)))
			fmt.Fprintf(&data, "</Prefix><KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><EncodingType>url</EncodingType><IsTruncated>false</IsTruncated>", len(records))
			for _, m := range records {
				data.WriteString("<Contents><Key>")
				_ = xml.EscapeText(&data, []byte(url.QueryEscape("velero/kopia/fixture/"+string(m.BlobID))))
				fmt.Fprintf(&data, "</Key><LastModified>%s</LastModified><ETag>fixture</ETag><Size>%d</Size></Contents>", m.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), m.Length)
			}
			data.WriteString("</ListBucketResult>")
			return storageResponse(request, 200, data.String()), nil
		}
		if !strings.HasPrefix(request.URL.Path, prefix) {
			t.Fatal("unexpected fixture path")
		}
		id := blob.ID(strings.TrimPrefix(request.URL.Path, prefix))
		m, err := backend.GetMetadata(ctx, id)
		if errors.Is(err, blob.ErrBlobNotFound) {
			return storageResponse(request, 404, "<Error><Code>NoSuchKey</Code></Error>"), nil
		}
		if err != nil {
			return nil, err
		}
		if request.Method == "HEAD" {
			response := storageResponse(request, 200, "")
			response.Body = http.NoBody
			response.ContentLength = m.Length
			response.Header.Set("Content-Length", strconv.FormatInt(m.Length, 10))
			return response, nil
		}
		if request.Method != "GET" {
			t.Fatal("fixture received mutation")
		}
		offset, length := int64(0), int64(-1)
		status := 200
		if rangeValue := request.Header.Get("Range"); rangeValue != "" {
			parts := requestedRange.FindStringSubmatch(rangeValue)
			if len(parts) != 3 {
				t.Fatal("unexpected fixture range")
			}
			offset, _ = strconv.ParseInt(parts[1], 10, 64)
			end, _ := strconv.ParseInt(parts[2], 10, 64)
			length = end - offset + 1
			status = 206
		}
		var out outputBuffer
		if err = backend.GetBlob(ctx, id, offset, length, &out); err != nil {
			return nil, err
		}
		response := storageResponse(request, status, out.String())
		response.Header.Set("Content-Length", strconv.Itoa(out.Len()))
		if status == 206 {
			response.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, m.Length))
		}
		return response, nil
	})
}

func TestReaderOpensRealEncryptedRepositoryWithoutAmbientCache(t *testing.T) {
	ctx, r, _ := repositoryFixture(t, false)
	cache := filepath.Join(t.TempDir(), "ambient-cache")
	t.Setenv("KOPIA_CACHE_DIRECTORY", cache)
	p := listPolicy()
	p.MaxRequests = 512
	p.MaxResponseBytes = 4 << 20
	p.MaxBytes = 64 << 20
	parent := t.TempDir()
	c, err := observeRepository(ctx, p, "fixture-access", "fixture-secret", "fixture-password", parent, repositoryHTTPFixture(t, r))
	if err != nil || c.Snapshots != 1 || !c.MetadataOnly {
		t.Fatalf("real guarded repository not observed: %v", err)
	}
	if _, err = os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambient cache was created: %v", err)
	}
	remaining, err := os.ReadDir(parent)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("private config was not removed: count=%d err=%v", len(remaining), err)
	}
}

func TestReaderDoesNotPublishPartialCatalogueFromCorruptRepository(t *testing.T) {
	ctx, r, _ := repositoryFixture(t, true)
	p := listPolicy()
	p.MaxRequests = 512
	p.MaxResponseBytes = 4 << 20
	p.MaxBytes = 64 << 20
	c, err := observeRepository(ctx, p, "fixture-access", "fixture-secret", "fixture-password", t.TempDir(), repositoryHTTPFixture(t, r))
	if err == nil || c.Digest != "" || len(c.entries) != 0 {
		t.Fatal("corrupt snapshot was silently omitted")
	}
}

func TestReaderRejectsUnsafeEnvironmentBeforeTransport(t *testing.T) {
	t.Setenv("KOPIA_IGNORE_MALFORMED_MANIFEST_CONTENTS", "1")
	if _, err := observeRepository(context.Background(), listPolicy(), "access", "secret", "password", t.TempDir(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe environment reached backend")
		return &http.Response{Body: io.NopCloser(strings.NewReader(""))}, nil
	})); err == nil {
		t.Fatal("skip option accepted")
	}
}
