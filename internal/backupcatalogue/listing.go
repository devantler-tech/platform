package backupcatalogue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/minio/minio-go/v7/pkg/signer"
)

type listPage struct {
	Name, Prefix, EncodingType *string
	KeyCount, MaxKeys          *int
	IsTruncated                *bool
	ContinuationToken          *string
	NextContinuationToken      string
	Delimiter, StartAfter      string
	CommonPrefixes             []struct{ Prefix string }
	Contents                   []struct {
		Key          string
		Size         *int64
		LastModified *time.Time
		ETag         string
	}
}

func decodeListPage(data []byte, bucket, prefix, token string) (listPage, error) {
	var p listPage
	if !singularXML(data, "ListBucketResult", true) || xml.Unmarshal(data, &p) != nil || p.Name == nil || *p.Name != bucket || p.Prefix == nil ||
		p.EncodingType == nil || *p.EncodingType != "url" || p.KeyCount == nil || *p.KeyCount != len(p.Contents) || p.MaxKeys == nil || *p.MaxKeys != 1000 ||
		p.IsTruncated == nil || len(p.Contents) > 1000 || len(p.CommonPrefixes) != 0 || p.Delimiter != "" || p.StartAfter != "" {
		return listPage{}, errReadHeld
	}
	decoded, err := url.QueryUnescape(*p.Prefix)
	if err != nil || decoded != prefix || token != "" && (p.ContinuationToken == nil || *p.ContinuationToken != token) ||
		token == "" && p.ContinuationToken != nil && *p.ContinuationToken != "" || len(p.NextContinuationToken) > 8192 ||
		*p.IsTruncated && (p.NextContinuationToken == "" || len(p.Contents) == 0) || !*p.IsTruncated && p.NextContinuationToken != "" {
		return listPage{}, errReadHeld
	}
	return p, nil
}

// ListBlobs uses the pinned SDK signer with explicit page decoding: the SDK's
// iterator does not require every binding/final marker and discards the page
// envelope needed to prove complete, non-repeating pagination.
func (s *readStore) ListBlobs(ctx context.Context, prefix blob.ID, callback func(blob.Metadata) error) error {
	if err := s.guard.beginOperation(ctx); err != nil {
		return err
	}
	defer s.guard.endOperation()
	fullPrefix := s.prefix + string(prefix)
	if callback == nil || !cleanKey(fullPrefix) || len(fullPrefix) > 1024 {
		return s.guard.hold()
	}
	seenTokens := map[string]bool{"": true}
	token, lastKey := "", ""
	count := 0
	for pageNumber := 0; pageNumber < 1024; pageNumber++ {
		if !contextActive(ctx) {
			return s.guard.hold()
		}
		q := url.Values{"list-type": {"2"}, "encoding-type": {"url"}, "prefix": {fullPrefix}, "max-keys": {"1000"}, "delimiter": {""}, "fetch-owner": {"true"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		r, err := http.NewRequestWithContext(ctx, "GET", s.endpoint+"/"+s.bucket+"/?"+q.Encode(), nil)
		if err != nil {
			return s.guard.hold()
		}
		hash := sha256.Sum256(nil)
		r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(hash[:]))
		r = signer.SignV4(*r, s.accessKey, s.secretKey, "", "auto")
		response, err := s.httpClient.Do(r)
		if err != nil {
			return s.guard.hold()
		}
		data, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || !contextActive(ctx) {
			return s.guard.hold()
		}
		page, err := decodeListPage(data, s.bucket, fullPrefix, token)
		if err != nil {
			return s.guard.hold()
		}
		for _, object := range page.Contents {
			key, decodeErr := url.QueryUnescape(object.Key)
			count++
			if decodeErr != nil || !strings.HasPrefix(key, fullPrefix) || !cleanKey(key) || key <= lastKey || len(key) > 1024 || count > 16384 ||
				object.Size == nil || *object.Size < 0 || object.LastModified == nil || object.LastModified.IsZero() || object.ETag == "" {
				return s.guard.hold()
			}
			lastKey = key
			id := blob.ID(strings.TrimPrefix(key, s.prefix))
			if id == ".storageconfig" {
				continue
			}
			if len(id) == 0 || callback(blob.Metadata{BlobID: id, Length: *object.Size, Timestamp: *object.LastModified}) != nil {
				return s.guard.hold()
			}
		}
		if !contextActive(ctx) {
			return s.guard.hold()
		}
		if !*page.IsTruncated {
			return nil
		}
		if seenTokens[page.NextContinuationToken] {
			return s.guard.hold()
		}
		token = page.NextContinuationToken
		seenTokens[token] = true
	}
	return s.guard.hold()
}
