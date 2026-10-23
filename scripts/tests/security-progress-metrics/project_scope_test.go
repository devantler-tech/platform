package securityprogress_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const unscopedMetrics = "# synthetic fixture\nplatform_security_collection_success{surface=\"policy\"} 1\nplatform_security_posture_freshness_observable 0\n"

func bindProject(t *testing.T, server *httptest.Server, project string) (string, string, error) {
	t.Helper()
	path, err := filepath.Abs("../../../k8s/bases/infrastructure/coroot/components/security-progress/bind-project-metrics.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", path)
	cmd.Env = append(os.Environ(), "COROOT_URL="+server.URL, "COROOT_PROJECT="+project)
	cmd.Stdin = strings.NewReader(unscopedMetrics)
	var output, diagnostic strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &diagnostic
	err = cmd.Run()
	return output.String(), diagnostic.String(), err
}

func TestProjectBindingScopesLabeledAndUnlabeledMetrics(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/user" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"name":"Anonymous","role":"Admin","anonymous":true,"projects":[{"id":"other","name":"other"},{"id":"synthetic-platform","name":"platform"}]}`)
	}))
	defer server.Close()
	output, diagnostic, err := bindProject(t, server, "platform")
	if err != nil {
		t.Fatalf("binding failed: %v; %s", err, diagnostic)
	}
	for _, want := range []string{
		`platform_security_collection_success{coroot_project_id="synthetic-platform",surface="policy"} 1`,
		`platform_security_posture_freshness_observable{coroot_project_id="synthetic-platform"} 0`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing correctly scoped sample %s in %s", want, output)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("expected one bounded discovery request, got %d", requests.Load())
	}
}

func TestProjectBindingNeverPublishesMetricsWithoutVerifiedScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing", http.StatusOK, `{"projects":[{"id":"other","name":"other"}]}`},
		{"empty", http.StatusOK, `{"projects":[]}`},
		{"duplicate", http.StatusOK, `{"projects":[{"id":"one","name":"platform"},{"id":"two","name":"platform"}]}`},
		{"empty_id", http.StatusOK, `{"projects":[{"id":"","name":"platform"}]}`},
		{"non_string_id", http.StatusOK, `{"projects":[{"id":12,"name":"platform"}]}`},
		{"illegal_id", http.StatusOK, `{"projects":[{"id":"invalid/label","name":"platform"}]}`},
		{"oversized_id", http.StatusOK, `{"projects":[{"id":"` + strings.Repeat("x", 129) + `","name":"platform"}]}`},
		{"malformed_json", http.StatusOK, `not JSON`},
		{"invalid_projects", http.StatusOK, `{"projects":"not an array"}`},
		{"forbidden", http.StatusForbidden, `PRIVATE synthetic response`},
		{"server_failure", http.StatusInternalServerError, `PRIVATE synthetic response`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			output, diagnostic, err := bindProject(t, server, "platform")
			if err == nil || output != "" {
				t.Fatalf("unverified scope published samples: err=%v stdout=%q stderr=%q", err, output, diagnostic)
			}
			if strings.Contains(diagnostic, "PRIVATE") {
				t.Fatalf("private API response appeared in logs: %s", diagnostic)
			}
		})
	}
}

func TestProjectBindingRejectsTruncatedHTTPWithValidJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(buffer, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n"+
			`{"projects":[{"id":"synthetic-platform","name":"platform"}]}`)
		_ = buffer.Flush()
	}))
	defer server.Close()
	output, diagnostic, err := bindProject(t, server, "platform")
	if err == nil || output != "" {
		t.Fatalf("failed HTTP transport accepted JSON scope: err=%v stdout=%q stderr=%q", err, output, diagnostic)
	}
}
