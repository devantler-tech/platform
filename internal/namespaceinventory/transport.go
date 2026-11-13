// Package namespaceinventory observes metadata under one receipted temporary
// benchmark namespace. A complete observation is not deletion authorization.
package namespaceinventory

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

const metadataObjectAccept = "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1"
const metadataListAccept = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
const responseLimit = 1024 * 1024
const totalLimit = 16 * 1024 * 1024
const requestLimit = 512

var namespacePattern = regexp.MustCompile(`^longhorn-benchmark-(v1|v2)-[1-9][0-9]{0,18}$`)
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
var groupPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
var versionPattern = regexp.MustCompile(`^v[1-9][0-9]*((alpha|beta)[1-9][0-9]*)?$`)

type reader struct {
	client          http.Client
	endpoint        *url.URL
	namespace, uid  string
	requests, bytes int
}

func observationActive(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.New("namespace observation is inactive")
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return errors.New("namespace observation deadline passed")
	}
	return nil
}

func (r *reader) get(ctx context.Context, path, accept string) ([]byte, error) {
	if err := observationActive(ctx); err != nil {
		return nil, err
	}
	if r.requests >= requestLimit {
		return nil, errors.New("namespace observation request bound exceeded")
	}
	r.requests++
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	target := *r.endpoint
	target.Path, target.RawPath, target.RawQuery = path, "", ""
	if accept == metadataListAccept {
		target.RawQuery = "limit=256"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("could not construct fixed namespace read")
	}
	req.Header.Set("Accept", accept)
	response, err := r.client.Do(req)
	if err != nil {
		return nil, errors.New("namespace read could not complete")
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("namespace read has no response body")
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	readExpired := observationActive(ctx) != nil
	if readExpired {
		cancel()
	}
	closed := make(chan error, 1)
	go func() { closed <- response.Body.Close() }()
	deadline, _ := ctx.Deadline()
	cleanupTimer := time.NewTimer(time.Until(deadline))
	defer cleanupTimer.Stop()
	select {
	case err = <-closed:
	case <-ctx.Done():
		return nil, errors.New("namespace response cleanup exceeded its deadline")
	case <-cleanupTimer.C:
		return nil, errors.New("namespace response cleanup exceeded its wall-clock deadline")
	}
	r.bytes += len(raw)
	if readErr != nil || err != nil || readExpired || observationActive(ctx) != nil || response.StatusCode != http.StatusOK || len(raw) > responseLimit || r.bytes > totalLimit {
		return nil, errors.New("namespace response is incomplete or outside its bounds")
	}
	return raw, nil
}
