package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixtureReader(change func(string, string) string) readFunc {
	return func(_ context.Context, path string) ([]byte, error) {
		var body string
		switch {
		case path == corootProxy+"/api/user":
			body = `{"projects":[{"id":"test-project","name":"platform"}]}`
		case strings.Contains(path, "/overview/applications?"):
			body = `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"applications":[{"status":"ok","logs":{"value":""}}]}}`
		case strings.Contains(path, "/overview/risks?"):
			body = `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"risks":null}}`
		case strings.Contains(path, "/alerts?"):
			body = `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"total":0,"firing":0,"resolved":0,"alerts":[]}}`
		case path == rulesPath || path == bindingsPath:
			body = `{"items":[]}`
		default:
			return nil, errors.New("unexpected target")
		}
		if change != nil {
			body = change(path, body)
		}
		return []byte(body), nil
	}
}

func TestObservation(t *testing.T) {
	s, err := observe(context.Background(), fixtureReader(nil), time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.Applications != 1 || s.ApplicationIssues != 0 || s.RuleObjects != 0 || s.PortForwardEnabledAndBound {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	if s.FullAcceptanceVerified {
		t.Fatal("a scoped API observation claimed full platform acceptance")
	}
}

func TestCorootAbsoluteWindowUsesMilliseconds(t *testing.T) {
	// Coroot v1.28.1's utils.ParseTime divides absolute query integers by 1,000.
	// Decode requests as the server does, rather than accepting any query string.
	now := time.Date(2026, time.November, 2, 21, 0, 0, 123456789, time.FixedZone("offset", 3600))
	seen := 0
	serverWindows := map[string]time.Time{}
	reader := func(ctx context.Context, path string) ([]byte, error) {
		if strings.Contains(path, "/overview/") || strings.Contains(path, "/alerts?") {
			u, err := url.ParseRequestURI(path)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]time.Time{"from": now.Add(-time.Hour), "to": now} {
				value, err := strconv.ParseInt(u.Query().Get(key), 10, 64)
				if err != nil {
					t.Fatalf("invalid %s: %v", key, err)
				}
				serverTime := time.Unix(value/1000, 0)
				if !serverTime.Equal(want.Truncate(time.Second)) {
					t.Errorf("%s %s addresses %s, want %s", u.Path, key, serverTime.UTC(), want.UTC().Truncate(time.Second))
				}
				serverWindows[key] = serverTime
			}
			seen++
		}
		return fixtureReader(nil)(ctx, path)
	}
	s, err := observe(context.Background(), reader, now)
	if err != nil {
		t.Fatal(err)
	}
	if seen != 3 || s.WindowTo.Sub(s.WindowFrom) != time.Hour {
		t.Fatalf("query surfaces=%d window=%s", seen, s.WindowTo.Sub(s.WindowFrom))
	}
	if !s.WindowFrom.Equal(serverWindows["from"]) || !s.WindowTo.Equal(serverWindows["to"]) {
		t.Fatal("the reported window differs from Coroot's interpreted interval")
	}
}

func TestFindingsAreNotDiscarded(t *testing.T) {
	reader := fixtureReader(func(path, body string) string {
		switch {
		case strings.Contains(path, "/overview/applications?"):
			return `{"context":{"status":{"status":"warning"},"alerts":{"critical":2}},"data":{"applications":[{"status":"critical","logs":{"value":"3 unique errors"}},{"status":"unknown","logs":{"value":""}}]}}`
		case strings.Contains(path, "/overview/risks?"):
			return `{"context":{"status":{"status":"ok"},"alerts":{"critical":1,"warning":1}},"data":{"risks":[{"severity":"warning","dismissal":{"reason":"accepted"}},{"severity":"unknown"}]}}`
		case strings.Contains(path, "/alerts?"):
			return `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"total":4,"firing":3,"resolved":0,"alerts":[{}]}}`
		case path == rulesPath:
			return `{"items":[{"spec":{"rules":[{"id":"R2001","name":"Port forward to pod","enabled":true}]}}]}`
		case path == bindingsPath:
			return `{"items":[{"spec":{"rules":[{"ruleID":"R2001"}]}}]}`
		default:
			return body
		}
	})
	s, err := observe(context.Background(), reader, time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.ApplicationIssues != 1 || s.UnknownApplications != 1 || s.LogErrorPatterns != 3 ||
		s.Alerts != 3 || s.UnresolvedAlerts != 4 || s.Risks != 2 || s.DismissedRisks != 1 || s.UnknownRisks != 1 ||
		!s.PortForwardEnabledAndBound || s.CorootHealth != "warning" {
		t.Fatalf("a finding was lost: %+v", s)
	}
}

func TestIncompleteEvidenceFailsClosed(t *testing.T) {
	for _, bad := range []string{
		`{}`, `null`, `{"data":null}`, `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"applications":[]}}`,
		`{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"applications":[{"status":"ok"}]}}`,
		`{"context":{"status":{"status":"ok"},"alerts":{"critical":-1}},"data":{"applications":[{"status":"ok","logs":{"value":""}}]}}`,
		`{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"applications":[{"status":"surprise","logs":{"value":""}}]}}`,
		`{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"applications":[{"status":"ok","logs":{"value":"unrecognized"}}]}}`,
	} {
		t.Run(bad, func(t *testing.T) {
			reader := fixtureReader(func(path, body string) string {
				if strings.Contains(path, "/overview/applications?") {
					return bad
				}
				return body
			})
			if _, err := observe(context.Background(), reader, time.Now()); err == nil {
				t.Fatal("incomplete or invalid evidence was accepted")
			}
		})
	}
	for _, projects := range []string{`[]`, `[{"name":"platform","id":"../other"}]`, `[{"name":"platform","id":"x"},{"name":"platform","id":"y"}]`} {
		reader := fixtureReader(func(path, body string) string {
			if path == corootProxy+"/api/user" {
				return `{"projects":` + projects + `}`
			}
			return body
		})
		if _, err := observe(context.Background(), reader, time.Now()); err == nil {
			t.Fatal("missing, ambiguous or unsafe project was accepted")
		}
	}
	if _, err := observe(context.Background(), func(context.Context, string) ([]byte, error) {
		return nil, errors.New("private transport detail must not be printed")
	}, time.Now()); err == nil {
		t.Fatal("transport failure was accepted")
	}
}

func TestDispatchBoundary(t *testing.T) {
	env := map[string]string{
		"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch",
		"GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1",
		"RUNTIME_HEALTH_CONFIRM": "observe-runtime-health",
		"GITHUB_SHA":             strings.Repeat("a", 40),
	}
	get := func(key string) string { return env[key] }
	if !allowedEnvironment(get) {
		t.Fatal("confirmed first main dispatch refused")
	}
	for key, bad := range map[string]string{
		"GITHUB_REPOSITORY": "outside/platform", "GITHUB_EVENT_NAME": "pull_request",
		"GITHUB_REF": "refs/heads/proposed", "GITHUB_RUN_ATTEMPT": "2",
		"RUNTIME_HEALTH_CONFIRM": "yes", "GITHUB_SHA": "short",
	} {
		old := env[key]
		env[key] = bad
		if allowedEnvironment(get) {
			t.Fatalf("unsafe %s accepted", key)
		}
		env[key] = old
	}
}

func TestAlertQueryContextSpikeIsPreserved(t *testing.T) {
	reader := fixtureReader(func(path, body string) string {
		if strings.Contains(path, "/alerts?") {
			return `{"context":{"status":{"status":"ok"},"alerts":{"critical":7}},"data":{"total":0,"firing":0,"resolved":0,"alerts":[]}}`
		}
		return body
	})
	s, err := observe(context.Background(), reader, time.Now())
	if err != nil || s.Alerts != 7 {
		t.Fatalf("a later context observation was dropped: alerts=%d err=%v", s.Alerts, err)
	}
}

func TestOtherSurfacesRefuseIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct{ surface, body string }{
		{"/overview/risks?", `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{}}`},
		{"/overview/risks?", `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"risks":[{}]}}`},
		{"/alerts?", `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"alerts":[],"total":0}}`},
		{"/alerts?", `{"context":{"status":{"status":"ok"},"alerts":{}},"data":{"alerts":null,"total":0,"firing":0,"resolved":0}}`},
		{rulesPath, `{"items":[{}]}`},
		{bindingsPath, `{"items":null}`},
	} {
		reader := fixtureReader(func(path, body string) string {
			if strings.Contains(path, tc.surface) {
				return tc.body
			}
			return body
		})
		if _, err := observe(context.Background(), reader, time.Now()); err == nil {
			t.Fatalf("incomplete %s evidence accepted", tc.surface)
		}
	}
}

type onlyReader struct{ io.Reader }

func TestRuleNamesResolveExactly(t *testing.T) {
	for _, tc := range []struct {
		name, reference string
		want            bool
	}{
		{"stable ID", `{"ruleID":"R2001"}`, true},
		{"exact name", `{"ruleName":"Port forward to pod"}`, true},
		{"chart mismatch", `{"ruleName":"Port forward"}`, false},
		{"unknown name", `{"ruleName":"other"}`, false},
		{"ID precedes name", `{"ruleID":"R0000","ruleName":"Port forward to pod"}`, false},
		{"name precedes tags", `{"ruleName":"other","ruleTags":["k8s"]}`, false},
		{"matching tag", `{"ruleTags":["other","k8s"]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := fixtureReader(func(path, body string) string {
				if path == rulesPath {
					return `{"items":[{"spec":{"rules":[{"id":"R2001","name":"Port forward to pod","tags":["k8s"],"enabled":true}]}}]}`
				}
				if path == bindingsPath {
					return `{"items":[{"spec":{"rules":[` + tc.reference + `]}}]}`
				}
				return body
			})
			s, err := observe(context.Background(), reader, time.Now())
			if err != nil || s.PortForwardEnabledAndBound != tc.want {
				t.Fatalf("resolved=%v want=%v err=%v", s.PortForwardEnabledAndBound, tc.want, err)
			}
		})
	}
}

func TestNativeResponseSizeIsBounded(t *testing.T) {
	var output boundedOutput
	_, err := io.Copy(&output, onlyReader{strings.NewReader(strings.Repeat("x", 8*1024*1024+1))})
	if err == nil || len(output.Bytes()) > 8*1024*1024 {
		t.Fatal("the command output limit was bypassed")
	}
}
