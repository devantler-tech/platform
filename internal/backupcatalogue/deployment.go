package backupcatalogue

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

type repositoryObserver func(context.Context, Policy, string, string, string, string, http.RoundTripper) (Catalogue, error)

// ObserveDeployment reads one of the two historical backup repositories. The
// caller supplies reviewed deployment values and fixed native Kubernetes GETs;
// this function binds their identities and scope before any backend access and
// requires the same complete bindings afterwards. No Secret data is serialized.
func ObserveDeployment(ctx context.Context, namespace string, expected Policy, load func(context.Context) (DeploymentDocuments, error), parent string) (Catalogue, error) {
	if ctx == nil || load == nil || !contextActive(ctx) {
		return Catalogue{}, errReadHeld
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns: 2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 4, ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()
	return observeBoundDeployment(ctx, namespace, expected, load, parent, transport, observeRepository)
}

func observeBoundDeployment(ctx context.Context, namespace string, expected Policy, load func(context.Context) (DeploymentDocuments, error), parent string, transport http.RoundTripper, read repositoryObserver) (Catalogue, error) {
	if ctx == nil || load == nil || read == nil || transport == nil || !contextActive(ctx) {
		return Catalogue{}, errReadHeld
	}
	documents, err := load(ctx)
	if err != nil || !contextActive(ctx) {
		return Catalogue{}, observationFailure{"deployment-read-before"}
	}
	before, err := bindDeployment(namespace, expected, documents)
	if err != nil {
		return Catalogue{}, observationFailure{"deployment-bind-before"}
	}
	result, err := read(ctx, before.policy, before.accessKey, before.secretKey, before.password, parent, transport)
	if err != nil || !contextActive(ctx) {
		return Catalogue{}, observationFailure{"repository-metadata"}
	}
	documents, err = load(ctx)
	if err != nil || !contextActive(ctx) {
		return Catalogue{}, observationFailure{"deployment-read-after"}
	}
	after, err := bindDeployment(namespace, expected, documents)
	if err != nil || !contextActive(ctx) {
		return Catalogue{}, observationFailure{"deployment-bind-after"}
	}
	if !sameDeployment(before, after) {
		return Catalogue{}, observationFailure{"deployment-changed"}
	}
	return result, nil
}
