package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"reflect"
	"strings"
	"time"
)

type recoveryCommand = func(context.Context, []string, []byte) ([]byte, error)

const protectedClusterPath = "/apis/postgresql.cnpg.io/v1/namespaces/wedding-app/clusters/wedding-db"
const transportByteLimit = 1 << 20

var configViewArgs = []string{"config", "view", "--minify", "--flatten", "--raw", "-o", "json"}

// limitedCapture bounds credential output while it is being captured. Neither
// the exported kubeconfig nor its stderr is printed or persisted.
type limitedCapture struct{ buffer bytes.Buffer }

// Len reports captured bytes without exposing their contents.
func (b *limitedCapture) Len() int { return b.buffer.Len() }

// Write refuses output beyond the bound, including writes made by io.Copy.
func (b *limitedCapture) Write(p []byte) (int, error) {
	remaining := transportByteLimit - b.Len()
	if len(p) > remaining {
		n, _ := b.buffer.Write(p[:remaining])
		return n, errors.New("configuration output exceeds capture bound")
	}
	return b.buffer.Write(p)
}

// captureConfig bounds both streams and returns only sanitized failure text.
func captureConfig(cmd *exec.Cmd) ([]byte, error) {
	var stdout, stderr limitedCapture
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if cmd.Run() != nil {
		return nil, errors.New("protected configuration export failed")
	}
	return stdout.buffer.Bytes(), nil
}

// kubectlCommand keeps the selected context for all existing operations. The
// one local configuration export also has its own subprocess deadline.
func kubectlCommand(contextName string) recoveryCommand {
	return func(ctx context.Context, args []string, body []byte) ([]byte, error) {
		if reflect.DeepEqual(args, configViewArgs) {
			bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return captureConfig(exec.CommandContext(bounded, "kubectl", append([]string{"--context", contextName}, args...)...))
		}
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", contextName, "--request-timeout=30s"}, args...)...)
		cmd.Stdin = bytes.NewReader(body)
		return cmd.Output()
	}
}

// protectedConfig accepts only the existing certificate-authenticated context.
// Unknown fields fail closed: in particular, silently dropping impersonation,
// alternate authentication or a declared proxy could change effective identity.
type protectedConfig struct {
	APIVersion     string          `json:"apiVersion"`
	Kind           string          `json:"kind"`
	Preferences    json.RawMessage `json:"preferences"`
	CurrentContext string          `json:"current-context"`
	Contexts       []struct {
		Name    string `json:"name"`
		Context struct {
			Cluster   string `json:"cluster"`
			User      string `json:"user"`
			Namespace string `json:"namespace"`
		} `json:"context"`
	} `json:"contexts"`
	Clusters []struct {
		Name    string `json:"name"`
		Cluster struct {
			Server        string `json:"server"`
			CAData        string `json:"certificate-authority-data"`
			TLSServerName string `json:"tls-server-name"`
		} `json:"cluster"`
	} `json:"clusters"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			CertificateData string `json:"client-certificate-data"`
			KeyData         string `json:"client-key-data"`
		} `json:"user"`
	} `json:"users"`
}

type clusterRequestFailure struct{ reason string }

// Error excludes endpoint, response and credential details from failure output.
func (clusterRequestFailure) Error() string { return "protected Cluster request failed" }

// invalidResponseReason classifies only a complete Kubernetes Status envelope.
// Response text stays in bounded memory and never identifies a failed predicate.
func invalidResponseReason(body io.Reader) string {
	const unclassified = "SERVER_INVALID"
	const statusByteLimit = 64 << 10
	data, err := io.ReadAll(io.LimitReader(body, statusByteLimit+1))
	if err != nil || len(data) > statusByteLimit {
		return unclassified
	}
	var status struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Status     string `json:"status"`
		Reason     string `json:"reason"`
		Code       int    `json:"code"`
		Details    *struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if json.Unmarshal(data, &status) != nil || status.APIVersion != "v1" || status.Kind != "Status" || status.Status != "Failure" || status.Reason != "Invalid" || status.Code != http.StatusUnprocessableEntity {
		return unclassified
	}
	if status.Details == nil || len(status.Details.Causes) == 0 {
		return "SERVER_INVALID_NO_CAUSES"
	}
	for _, cause := range status.Details.Causes {
		if cause.Reason == "" {
			return unclassified
		}
	}
	return "SERVER_INVALID_WITH_CAUSES"
}

// protectedClusterCommand resolves credentials once, before the final source
// proof and GET. Only the fixed Cluster GET/PATCH shapes use the reused mTLS
// connection; all other existing operations keep their kubectl implementation.
func protectedClusterCommand(ctx context.Context, command recoveryCommand) (recoveryCommand, func(), error) {
	refuse := errors.New("protected Cluster transport configuration unavailable")
	data, err := command(ctx, configViewArgs, nil)
	if err != nil || len(data) > transportByteLimit {
		return nil, nil, refuse
	}
	var config protectedConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || decoder.Decode(new(any)) != io.EOF || config.APIVersion != "v1" || config.Kind != "Config" || config.CurrentContext != "admin@prod" || len(config.Contexts) != 1 || len(config.Clusters) != 1 || len(config.Users) != 1 {
		return nil, nil, refuse
	}
	selected, cluster, user := config.Contexts[0], config.Clusters[0], config.Users[0]
	if selected.Name != "admin@prod" || cluster.Name == "" || user.Name == "" || selected.Context.Cluster != cluster.Name || selected.Context.User != user.Name {
		return nil, nil, refuse
	}
	endpoint, err := url.Parse(cluster.Cluster.Server)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.RawPath != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, nil, refuse
	}
	ca, caErr := base64.StdEncoding.DecodeString(cluster.Cluster.CAData)
	cert, certErr := base64.StdEncoding.DecodeString(user.User.CertificateData)
	key, keyErr := base64.StdEncoding.DecodeString(user.User.KeyData)
	pool := x509.NewCertPool()
	pair, pairErr := tls.X509KeyPair(cert, key)
	if caErr != nil || certErr != nil || keyErr != nil || pairErr != nil || !pool.AppendCertsFromPEM(ca) {
		return nil, nil, refuse
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || defaultTransport == nil {
		return nil, nil, refuse
	}
	transport := defaultTransport.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, ServerName: cluster.Cluster.TLSServerName, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	endpoint.Path = protectedClusterPath
	getArgs := []string{"get", "cluster.postgresql.cnpg.io", clusterName, "-n", namespace, "--show-managed-fields=true", "-o", "json"}
	patch := patchArgs("cluster", object{"metadata": object{"name": clusterName}})
	dryRun := append(append([]string{}, patch...), "--dry-run=server")
	return func(ctx context.Context, args []string, body []byte) ([]byte, error) {
		if len(args) < 2 || !strings.HasPrefix(args[1], "cluster") {
			return command(ctx, args, body)
		}
		method := http.MethodGet
		requestURL := *endpoint
		var reader io.Reader
		switch {
		case reflect.DeepEqual(args, getArgs) && body == nil:
		case (reflect.DeepEqual(args, patch) || reflect.DeepEqual(args, dryRun)) && len(body) != 0 && json.Valid(body):
			method = http.MethodPatch
			query := url.Values{"fieldManager": {fieldManager}}
			if reflect.DeepEqual(args, dryRun) {
				query.Set("dryRun", "All")
			}
			requestURL.RawQuery = query.Encode()
			// Opaque, nonempty bodies have no GetBody. HTTP/1 cannot replay this
			// non-idempotent PATCH, even after a stale reused connection fails.
			reader = io.NopCloser(bytes.NewReader(body))
		default:
			return nil, clusterRequestFailure{"UNKNOWN"}
		}
		request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), reader)
		if err != nil {
			return nil, clusterRequestFailure{"UNKNOWN"}
		}
		request.Header.Set("Accept", "application/json")
		if method == http.MethodPatch {
			request.ContentLength = int64(len(body))
			request.Header.Set("Content-Type", "application/json-patch+json")
		}
		response, err := httpClient.Do(request)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return nil, ctxError(err)
			}
			return nil, clusterRequestFailure{"UNKNOWN"}
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			reasons := map[int]string{400: "SERVER_BAD_REQUEST", 401: "SERVER_UNAUTHORIZED", 403: "SERVER_FORBIDDEN", 404: "SERVER_NOT_FOUND", 409: "SERVER_CONFLICT", 422: "SERVER_INVALID", 429: "SERVER_THROTTLED", 500: "SERVER_INTERNAL", 503: "SERVER_UNAVAILABLE", 504: "SERVER_TIMEOUT"}
			reason := reasons[response.StatusCode]
			if response.StatusCode == http.StatusUnprocessableEntity {
				reason = invalidResponseReason(response.Body)
			}
			if reason == "" {
				reason = "UNKNOWN"
			}
			return nil, clusterRequestFailure{reason}
		}
		result, err := io.ReadAll(io.LimitReader(response.Body, transportByteLimit+1))
		var cluster object
		if err != nil || len(result) > transportByteLimit || json.Unmarshal(result, &cluster) != nil || str(cluster, "apiVersion") != "postgresql.cnpg.io/v1" || str(cluster, "kind") != "Cluster" || !validMeta(cluster, namespace) || id(cluster).name != clusterName {
			return nil, clusterRequestFailure{"UNKNOWN"}
		}
		return result, nil
	}, httpClient.CloseIdleConnections, nil
}

// ctxError preserves only known cancellation categories, never URL-bearing errors.
func ctxError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return context.Canceled
}
