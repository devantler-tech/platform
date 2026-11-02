package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// downloadRecoveryCredentials reads the one immutable archive bound to completed backup evidence.
func downloadRecoveryCredentials(ctx context.Context, client *http.Client, target target, prefix string, backup credentialBackup) (recoveryCredentials, error) {
	refuse := func() (recoveryCredentials, error) { return recoveryCredentials{}, errEvidence }
	if !regexp.MustCompile(`^velero/[a-z0-9][a-z0-9-]{0,62}$`).MatchString(prefix) || !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`).MatchString(backup.Name) || backup.Start.IsZero() || backup.End.Before(backup.Start) {
		return refuse()
	}
	key := prefix + "/backups/" + backup.Name + "/" + backup.Name + ".tar.gz"
	raw, _, err := signedRead(ctx, client, target, "/"+target.Bucket, url.Values{"list-type": {"2"}, "prefix": {key}, "max-keys": {"2"}}, "", 1<<20)
	if err != nil {
		return refuse()
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
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&listing) != nil || decoder.Decode(new(any)) != io.EOF || listing.XMLName.Local != "ListBucketResult" ||
		(listing.XMLName.Space != "" && listing.XMLName.Space != "http://s3.amazonaws.com/doc/2006-03-01/") || listing.Name != target.Bucket || listing.Prefix != key ||
		listing.KeyCount == nil || *listing.KeyCount != 1 || listing.MaxKeys == nil || *listing.MaxKeys != 2 || len(listing.Contents) != 1 || listing.IsTruncated == nil || *listing.IsTruncated || listing.NextContinuationToken != "" {
		return refuse()
	}
	object := listing.Contents[0]
	if object.Key != key || object.Size <= 0 || object.Size > maxCredentialArchive ||
		!regexp.MustCompile(`^"[a-fA-F0-9]{32}(?:-[1-9][0-9]{0,5})?"$`).MatchString(object.ETag) ||
		object.LastModified.Before(backup.Start.Add(-5*time.Minute)) || object.LastModified.After(backup.End.Add(5*time.Minute)) {
		return refuse()
	}
	raw, headers, err := signedRead(ctx, client, target, "/"+target.Bucket+"/"+key, nil, object.ETag, object.Size)
	defer clear(raw)
	modified, timeErr := http.ParseTime(headers.Get("Last-Modified"))
	if err != nil || timeErr != nil || int64(len(raw)) != object.Size || headers.Get("ETag") != object.ETag || !modified.Equal(object.LastModified.Truncate(time.Second)) {
		return refuse()
	}
	return readCredentialArchive(raw)
}
