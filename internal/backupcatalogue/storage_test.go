package backupcatalogue

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/readonly"
)

type outputBuffer struct{ bytes.Buffer }

func (b *outputBuffer) Length() int { return b.Len() }

func storageResponse(r *http.Request, status int, data string) *http.Response {
	header := http.Header{}
	header.Set("Last-Modified", time.Unix(1700000000, 0).UTC().Format(http.TimeFormat))
	header.Set("ETag", `"fixture"`)
	return &http.Response{Request: r, StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(data)), ContentLength: int64(len(data))}
}

func TestStorageUsesGuardBeforeAnySDKRead(t *testing.T) {
	calls := 0
	s, err := newStorage(t.Context(), testPolicy(), "fixture-access", "fixture-secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/backup-fixture/velero/kopia/fixture/p123" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Fatalf("unbound SDK request: %s %s", r.Method, r.URL.Path)
		}
		return storageResponse(r, 200, "bytes"), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("constructor accessed backend")
	}
	var out outputBuffer
	if err = s.GetBlob(t.Context(), "p123", 0, -1, &out); err != nil || out.String() != "bytes" || calls != 1 || s.guard.Err() != nil {
		t.Fatalf("guarded SDK read failed: %q calls=%d err=%v / %v", out.String(), calls, err, s.guard.Err())
	}
}

func TestStorageDeniesMutationsAndAnonymousFallback(t *testing.T) {
	for _, keys := range [][2]string{{"", "secret"}, {"access", ""}} {
		if s, err := newStorage(t.Context(), testPolicy(), keys[0], keys[1], http.DefaultTransport); err == nil || s != nil {
			t.Fatal("anonymous credential fallback accepted")
		}
	}
	for _, operation := range []string{"put", "delete", "retention"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			s, err := newStorage(t.Context(), testPolicy(), "access", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return storageResponse(r, 200, ""), nil }))
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "put":
				err = s.PutBlob(t.Context(), "p123", nil, blob.PutOptions{SetModTime: time.Now()})
			case "delete":
				err = s.DeleteBlob(t.Context(), "p123")
			case "retention":
				err = s.ExtendBlobRetention(t.Context(), "p123", blob.ExtendOptions{})
			}
			if !errors.Is(err, readonly.ErrReadonly) || !s.IsReadOnly() || calls != 0 || s.guard.Err() == nil {
				t.Fatalf("mutation was not a sticky zero-network refusal: %v calls=%d", err, calls)
			}
		})
	}
}

func TestStorageOnlyAcceptsValidatedOptionalMissingBlob(t *testing.T) {
	for _, tc := range []struct {
		id       blob.ID
		code     string
		accepted bool
	}{
		{"kopia.blobcfg", "NoSuchKey", true},
		{"kopia.blobcfg", "AccessDenied", false},
		{"kopia.repository", "NoSuchKey", false},
		{"x123", "NoSuchKey", false},
	} {
		t.Run(string(tc.id)+tc.code, func(t *testing.T) {
			s, err := newStorage(t.Context(), testPolicy(), "access", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return storageResponse(r, 404, "<Error><Code>"+tc.code+"</Code><Message>private provider text</Message></Error>"), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			var out outputBuffer
			err = s.GetBlob(t.Context(), tc.id, 0, -1, &out)
			if tc.accepted {
				if !errors.Is(err, blob.ErrBlobNotFound) || s.guard.Err() != nil {
					t.Fatalf("valid optional absence held: %v / %v", err, s.guard.Err())
				}
			} else if err == nil || errors.Is(err, blob.ErrBlobNotFound) || s.guard.Err() == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("required/invalid absence accepted or leaked: %v / %v", err, s.guard.Err())
			}
		})
	}
}

func TestStorageRejectsInvalidRangesWithoutReading(t *testing.T) {
	for _, tc := range []struct{ offset, length int64 }{{-1, -1}, {1, -1}, {0, -2}, {1 << 62, 1 << 62}} {
		s, err := newStorage(t.Context(), testPolicy(), "access", "secret", roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			t.Fatal("invalid range reached backend")
			return nil, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		var out outputBuffer
		if err = s.GetBlob(t.Context(), "p123", tc.offset, tc.length, &out); !errors.Is(err, blob.ErrInvalidRange) {
			t.Fatalf("invalid range accepted: %v", err)
		}
	}
}

func TestStorageBindsRangeToExactResponseOffsets(t *testing.T) {
	for _, tc := range []struct {
		name               string
		offset, length     int64
		status             int
		contentRange, body string
		valid              bool
	}{
		{"correct", 1, 2, 206, "bytes 1-2/5", "yt", true},
		{"wrong offset", 1, 2, 206, "bytes 2-3/5", "te", false},
		{"ignored range", 1, 2, 200, "", "yt", false},
		{"unsolicited partial", 0, -1, 206, "bytes 1-2/5", "yt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newStorage(t.Context(), testPolicy(), "access", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response := storageResponse(r, tc.status, tc.body)
				if tc.contentRange != "" {
					response.Header.Set("Content-Range", tc.contentRange)
				}
				return response, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			var out outputBuffer
			err = s.GetBlob(t.Context(), "p123", tc.offset, tc.length, &out)
			if tc.valid && (err != nil || out.String() != tc.body || s.guard.Err() != nil) {
				t.Fatalf("valid range held: %v / %v", err, s.guard.Err())
			}
			if !tc.valid && (err == nil || s.guard.Err() == nil) {
				t.Fatalf("wrong response range accepted: %q err=%v", out.String(), err)
			}
		})
	}
}
