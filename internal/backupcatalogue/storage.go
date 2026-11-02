package backupcatalogue

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/readonly"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type readStore struct {
	blob.DefaultProviderImplementation
	guard                          *ReadTransport
	client                         *minio.Client
	bucket, prefix                 string
	endpoint, accessKey, secretKey string
	httpClient                     *http.Client
}

func newStorage(ctx context.Context, p Policy, accessKey, secretKey string, base http.RoundTripper) (*readStore, error) {
	if accessKey == "" || secretKey == "" {
		return nil, errReadHeld
	}
	guard, err := NewReadTransport(ctx, p, base)
	if err != nil {
		return nil, err
	}
	client, err := minio.New(strings.TrimPrefix(p.Endpoint, "https://"), &minio.Options{
		Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: true, Transport: guard, Region: "auto", BucketLookup: minio.BucketLookupPath, MaxRetries: 1,
	})
	if err != nil {
		guard.Close()
		return nil, errReadHeld
	}
	return &readStore{guard: guard, client: client, bucket: p.Bucket, prefix: p.Prefix,
		endpoint: p.Endpoint, accessKey: accessKey, secretKey: secretKey, httpClient: &http.Client{Transport: guard, Timeout: 35 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (*readStore) IsReadOnly() bool { return true }
func (s *readStore) PutBlob(context.Context, blob.ID, blob.Bytes, blob.PutOptions) error {
	return errors.Join(readonly.ErrReadonly, s.guard.hold())
}
func (s *readStore) DeleteBlob(context.Context, blob.ID) error {
	return errors.Join(readonly.ErrReadonly, s.guard.hold())
}
func (s *readStore) ExtendBlobRetention(context.Context, blob.ID, blob.ExtendOptions) error {
	return errors.Join(readonly.ErrReadonly, s.guard.hold())
}

func (s *readStore) objectError(id blob.ID, err error) error {
	e := minio.ToErrorResponse(err)
	if id == "kopia.blobcfg" && e.StatusCode == 404 && e.Code == "NoSuchKey" && s.guard.acceptMissingRetentionConfig() == nil {
		return blob.ErrBlobNotFound
	}
	return s.guard.hold()
}

func (s *readStore) GetBlob(ctx context.Context, id blob.ID, offset, length int64, out blob.OutputBuffer) error {
	if err := s.guard.beginOperation(ctx); err != nil {
		return err
	}
	defer s.guard.endOperation()
	out.Reset()
	if !contextActive(ctx) || !cleanKey(string(id)) || len(id) == 0 || len(s.prefix)+len(id) > 1024 {
		return s.guard.hold()
	}
	if offset < 0 || length < -1 || length == -1 && offset != 0 || length > 0 && offset > math.MaxInt64-length {
		return errors.Join(blob.ErrInvalidRange, s.guard.hold())
	}
	if length == 0 {
		return nil
	}
	options := minio.GetObjectOptions{}
	if length > 0 {
		if err := options.SetRange(offset, offset+length-1); err != nil {
			return errors.Join(blob.ErrInvalidRange, s.guard.hold())
		}
	}
	// Core returns the actual response body. The convenience Object closes its
	// HTTP body asynchronously after Close returns, which cannot prove cleanup.
	r, _, _, err := (minio.Core{Client: s.client}).GetObject(ctx, s.bucket, s.prefix+string(id), options)
	if err != nil {
		return s.objectError(id, err)
	}
	_, readErr := io.Copy(out, r)
	closeErr := r.Close()
	if readErr != nil {
		return s.objectError(id, readErr)
	}
	if closeErr != nil || !contextActive(ctx) || blob.EnsureLengthExactly(out.Length(), length) != nil {
		return s.guard.hold()
	}
	return nil
}

func (s *readStore) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: "catalogue-ro", Config: &struct{}{}}
}
func (*readStore) DisplayName() string { return "catalogue-ro" }

func (s *readStore) GetMetadata(ctx context.Context, id blob.ID) (blob.Metadata, error) {
	if err := s.guard.beginOperation(ctx); err != nil {
		return blob.Metadata{}, err
	}
	defer s.guard.endOperation()
	if !contextActive(ctx) || !cleanKey(string(id)) || len(id) == 0 || len(s.prefix)+len(id) > 1024 {
		return blob.Metadata{}, s.guard.hold()
	}
	o, err := s.client.StatObject(ctx, s.bucket, s.prefix+string(id), minio.StatObjectOptions{})
	if err != nil {
		return blob.Metadata{}, s.objectError(id, err)
	}
	if !contextActive(ctx) || o.Size < 0 || o.LastModified.IsZero() {
		return blob.Metadata{}, s.guard.hold()
	}
	return blob.Metadata{BlobID: id, Length: o.Size, Timestamp: o.LastModified}, nil
}

var _ blob.Storage = (*readStore)(nil)
