package backupcatalogue

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kopia/kopia/repo/blob"
)

func listXML(key, truncated, continuation, next string) string {
	return `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>backup-fixture</Name><Prefix>velero/kopia/fixture/x</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><EncodingType>url</EncodingType><IsTruncated>` + truncated + `</IsTruncated><ContinuationToken>` + continuation + `</ContinuationToken><NextContinuationToken>` + next + `</NextContinuationToken><Contents><Key>velero/kopia/fixture/` + key + `</Key><LastModified>2026-06-10T00:00:00Z</LastModified><ETag>fixture</ETag><Size>5</Size></Contents></ListBucketResult>`
}

func listPolicy() Policy {
	p := testPolicy()
	p.MaxResponseBytes = 8192
	p.MaxBytes = 1 << 20
	return p
}

func TestStorageListsEveryBoundPage(t *testing.T) {
	calls := 0
	s, err := newStorage(t.Context(), listPolicy(), "access", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/backup-fixture/" || q.Get("prefix") != "velero/kopia/fixture/x" || q.Get("max-keys") != "1000" || q.Get("encoding-type") != "url" {
			t.Fatal("unbound list request")
		}
		calls++
		if calls == 1 && q.Get("continuation-token") == "" {
			return storageResponse(r, 200, listXML("x1", "true", "", "opaque")), nil
		}
		if calls == 2 && q.Get("continuation-token") == "opaque" {
			return storageResponse(r, 200, listXML("x2", "false", "opaque", "")), nil
		}
		t.Fatal("unexpected or repeated page")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var got []blob.ID
	err = s.ListBlobs(t.Context(), "x", func(m blob.Metadata) error {
		got = append(got, m.BlobID)
		if m.Length != 5 || m.Timestamp.IsZero() {
			t.Fatal("metadata missing")
		}
		return nil
	})
	if err != nil || calls != 2 || len(got) != 2 || got[0] != "x1" || got[1] != "x2" || s.guard.Err() != nil {
		t.Fatalf("complete listing not accepted: ids=%v calls=%d err=%v / %v", got, calls, err, s.guard.Err())
	}
}

func TestStorageRejectsIncompleteListMetadata(t *testing.T) {
	base := listXML("x1", "false", "", "")
	cases := map[string]string{
		"missing final marker": strings.Replace(base, "<IsTruncated>false</IsTruncated>", "", 1),
		"duplicate marker":     strings.Replace(base, "<IsTruncated>false</IsTruncated>", "<IsTruncated>true</IsTruncated><IsTruncated>false</IsTruncated>", 1),
		"truncated XML":        strings.TrimSuffix(base, "</ListBucketResult>"),
		"extra document":       base + base,
		"wrong bucket":         strings.Replace(base, "<Name>backup-fixture</Name>", "<Name>foreign</Name>", 1),
		"wrong prefix":         strings.Replace(base, "<Prefix>velero/kopia/fixture/x</Prefix>", "<Prefix>foreign/</Prefix>", 1),
		"foreign object":       strings.Replace(base, "<Key>velero/kopia/fixture/x1</Key>", "<Key>foreign/x1</Key>", 1),
		"wrong count":          strings.Replace(base, "<KeyCount>1</KeyCount>", "<KeyCount>0</KeyCount>", 1),
		"negative size":        strings.Replace(base, "<Size>5</Size>", "<Size>-1</Size>", 1),
		"missing timestamp":    strings.Replace(base, "<LastModified>2026-06-10T00:00:00Z</LastModified>", "", 1),
		"missing token":        listXML("x1", "true", "", ""),
		"unexpected token":     listXML("x1", "false", "foreign", ""),
		"wrong encoding":       strings.Replace(base, "<EncodingType>url</EncodingType>", "<EncodingType>other</EncodingType>", 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := newStorage(t.Context(), listPolicy(), "access", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) { return storageResponse(r, 200, data), nil }))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.ListBlobs(t.Context(), "x", func(blob.Metadata) error { return nil }); err == nil || s.guard.Err() == nil {
				t.Fatalf("incomplete listing accepted: %v / %v", err, s.guard.Err())
			}
		})
	}
}

func TestStorageRejectsRepeatedListCursorAndCallbackFailure(t *testing.T) {
	for _, mode := range []string{"cursor", "duplicate object", "callback"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s, err := newStorage(t.Context(), listPolicy(), "access", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return storageResponse(r, 200, listXML("x1", "true", "", "opaque")), nil
				}
				if mode == "cursor" {
					return storageResponse(r, 200, listXML("x2", "true", "opaque", "opaque")), nil
				}
				return storageResponse(r, 200, listXML("x1", "false", "opaque", "")), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			err = s.ListBlobs(t.Context(), "x", func(blob.Metadata) error {
				if mode == "callback" {
					return errors.New("private callback detail")
				}
				return nil
			})
			if err == nil || s.guard.Err() == nil || strings.Contains(err.Error(), "private") || calls > 2 {
				t.Fatalf("invalid continuation accepted: calls=%d err=%v / %v", calls, err, s.guard.Err())
			}
		})
	}
}
