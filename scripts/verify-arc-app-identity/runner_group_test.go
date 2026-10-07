package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureKSailID int64 = 737584922
const fixtureCanary = "devantler-tech/ksail/.github/workflows/verify-ksail-arc-delivery.yaml@refs/heads/main"

type groupFixture struct {
	mu                           sync.Mutex
	groups                       map[int64]map[string]any
	requests                     []string
	mutate                       func(string, map[string]any)
	deny                         string
	deleteFailure, revokeFailure bool
	created, deleted, revoked    bool
	cleanupRunner                bool
	runnerReads                  int
	rawPath, rawBody             string
	rawStatus                    int
	afterCreate                  func()
}

func fixtureGroup(id int64, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "default": false, "inherited": false,
		"visibility": "selected", "allows_public_repositories": true, "restricted_to_workflows": true,
		"selected_workflows": []string{fixtureCanary}, "workflow_restrictions_read_only": false}
}

func (f *groupFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		expected := "Bearer fixture-installation-token"
		if path == "/app/installations/42/access_tokens" {
			expected = "Bearer fixture-app-jwt"
		}
		if r.Header.Get("Authorization") != expected || r.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
			t.Error("wrong authentication boundary")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if path == f.deny {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		status := http.StatusOK
		if path == f.rawPath {
			if f.rawStatus > 0 {
				status = f.rawStatus
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(f.rawBody))
			return
		}
		var value map[string]any
		switch {
		case path == "/app/installations/42/access_tokens" && r.Method == http.MethodPost:
			var body struct {
				RepositoryIDs []int64           `json:"repository_ids"`
				Permissions   map[string]string `json:"permissions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || !slices.Equal(body.RepositoryIDs, []int64{fixtureKSailID}) || len(body.Permissions) != 2 || body.Permissions["organization_self_hosted_runners"] != "write" || body.Permissions["metadata"] != "read" {
				t.Error("installation token request was not narrow")
			}
			status = http.StatusCreated
			value = map[string]any{"token": "fixture-installation-token", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repository_selection": "selected", "permissions": map[string]string{"organization_self_hosted_runners": "write", "metadata": "read"}}
		case path == "/installation/token" && r.Method == http.MethodDelete:
			if f.revokeFailure {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.revoked = true
			w.WriteHeader(http.StatusNoContent)
			return
		case path == "/repos/devantler-tech/ksail":
			value = map[string]any{"id": fixtureKSailID, "full_name": "devantler-tech/ksail", "private": false, "archived": false}
		case path == "/installation/repositories":
			value = map[string]any{"total_count": 1, "repositories": []map[string]any{{"id": fixtureKSailID, "full_name": "devantler-tech/ksail", "private": false}}}
		case path == "/orgs/devantler-tech/actions/runner-groups" && r.Method == http.MethodPost:
			var body struct {
				Name       string   `json:"name"`
				Visibility string   `json:"visibility"`
				IDs        []int64  `json:"selected_repository_ids"`
				Runners    []int64  `json:"runners"`
				Public     bool     `json:"allows_public_repositories"`
				Restricted bool     `json:"restricted_to_workflows"`
				Workflows  []string `json:"selected_workflows"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Name != "ksail-capability-31415" || body.Visibility != "selected" || !slices.Equal(body.IDs, []int64{fixtureKSailID}) || body.Runners == nil || len(body.Runners) != 0 || !body.Public || !body.Restricted || !slices.Equal(body.Workflows, []string{fixtureCanary}) {
				t.Error("unsafe group creation")
			}
			f.created = true
			f.groups[99] = fixtureGroup(99, body.Name)
			if f.afterCreate != nil {
				f.afterCreate()
			}
			status = http.StatusCreated
			value = f.groups[99]
		case path == "/orgs/devantler-tech/actions/runner-groups":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < 1 {
				page = 1
			}
			ids := make([]int64, 0, len(f.groups))
			for id := range f.groups {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			groups := []map[string]any{}
			for i, id := range ids {
				if i >= (page-1)*100 && i < page*100 {
					groups = append(groups, f.groups[id])
				}
			}
			value = map[string]any{"total_count": len(f.groups), "runner_groups": groups}
		case strings.HasSuffix(path, "/repositories"):
			value = map[string]any{"total_count": 1, "repositories": []map[string]any{{"id": fixtureKSailID, "full_name": "devantler-tech/ksail", "private": false}}}
		case strings.HasSuffix(path, "/runners") || strings.HasSuffix(path, "/hosted-runners"):
			count := 0
			if strings.HasSuffix(path, "/runners") {
				f.runnerReads++
				if f.cleanupRunner && f.runnerReads > 1 {
					count = 1
				}
			}
			runners := []map[string]any{}
			if count > 0 {
				runners = append(runners, map[string]any{"id": 1})
			}
			value = map[string]any{"total_count": count, "runners": runners}
		case strings.HasPrefix(path, "/orgs/devantler-tech/actions/runner-groups/"):
			id, err := strconv.ParseInt(path[strings.LastIndex(path, "/")+1:], 10, 64)
			group, exists := f.groups[id]
			if err != nil || !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodDelete {
				if id != 99 || group["name"] != "ksail-capability-31415" {
					t.Error("attempt to delete a foreign group")
				}
				if f.deleteFailure {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				delete(f.groups, id)
				f.deleted = true
				w.WriteHeader(http.StatusNoContent)
				return
			}
			value = group
		default:
			t.Errorf("unexpected endpoint %s", r.Method+" "+path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Copy responses so a one-response fault does not silently rewrite remote state.
		encoded, _ := json.Marshal(value)
		var copy map[string]any
		_ = json.Unmarshal(encoded, &copy)
		if f.mutate != nil {
			f.mutate(path, copy)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(copy)
	}
}

func exerciseGroup(t *testing.T, f *groupFixture) outcome {
	t.Helper()
	server := httptest.NewTLSServer(f.handler(t))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return verifyRunnerGroupCapability(context.Background(), verificationOptions{githubURL: server.URL, client: client, now: time.Now}, "fixture-app-jwt", 42, "31415")
}

func TestRunnerGroupCapabilityExistingIsReadOnly(t *testing.T) {
	f := &groupFixture{groups: map[int64]map[string]any{10: fixtureGroup(10, "platform")}}
	if got := exerciseGroup(t, f); got != "PASS_EXISTING" {
		t.Fatalf("got %s", got)
	}
	if f.created || f.deleted || !f.revoked {
		t.Fatal("existing group mutated or token not revoked")
	}
}

func TestRunnerGroupCapabilityDisposableIsRemoved(t *testing.T) {
	f := &groupFixture{groups: map[int64]map[string]any{}}
	if got := exerciseGroup(t, f); got != "PASS_DISPOSABLE" {
		t.Fatalf("got %s", got)
	}
	if !f.created || !f.deleted || !f.revoked || len(f.groups) != 0 {
		t.Fatal("cleanup did not complete")
	}
}

func TestRunnerGroupCapabilityRefusesWrongPolicy(t *testing.T) {
	for _, mutation := range []struct {
		field string
		value any
	}{
		{"id", float64(0)}, {"name", "Default"}, {"default", true}, {"inherited", true}, {"visibility", "all"},
		{"allows_public_repositories", false}, {"restricted_to_workflows", false}, {"workflow_restrictions_read_only", true},
		{"selected_workflows", []string{}}, {"selected_workflows", []string{fixtureCanary, "devantler-tech/ksail/.github/workflows/ci.yaml@refs/heads/main"}},
		{"selected_workflows", []string{strings.Replace(fixtureCanary, "refs/heads/main", "refs/pull/1/merge", 1)}},
		{"restricted_to_workflows", nil}, {"default", nil},
	} {
		t.Run(fmt.Sprint(mutation.field, mutation.value), func(t *testing.T) {
			group := fixtureGroup(10, "platform")
			group[mutation.field] = mutation.value
			f := &groupFixture{groups: map[int64]map[string]any{10: group}}
			if mutation.field == "name" {
				// The selected group changes identity between listing and readback.
				group["name"] = "platform"
				f.mutate = func(path string, value map[string]any) {
					if path == runnerGroupsPath+"/10" {
						value["name"] = mutation.value
					}
				}
			}
			if got := exerciseGroup(t, f); strings.HasPrefix(string(got), "PASS") {
				t.Fatalf("unsafe policy passed: %s", got)
			}
			if f.created || f.deleted || !f.revoked {
				t.Fatal("bad existing group mutated or token not revoked")
			}
		})
	}
}

func TestRunnerGroupCapabilityRejectsIncompleteProof(t *testing.T) {
	for _, test := range []struct {
		name, path string
		mutation   func(map[string]any)
	}{
		{"missing-total", "/orgs/devantler-tech/actions/runner-groups", func(v map[string]any) { delete(v, "total_count") }},
		{"duplicate-group", "/orgs/devantler-tech/actions/runner-groups", func(v map[string]any) {
			a := v["runner_groups"].([]any)
			v["runner_groups"] = append(a, a[0])
			v["total_count"] = 2
		}},
		{"missing-repository", "/orgs/devantler-tech/actions/runner-groups/10/repositories", func(v map[string]any) { v["repositories"] = []any{}; v["total_count"] = 0 }},
		{"extra-repository", "/orgs/devantler-tech/actions/runner-groups/10/repositories", func(v map[string]any) {
			a := v["repositories"].([]any)
			v["repositories"] = append(a, map[string]any{"id": 42})
			v["total_count"] = 2
		}},
		{"self-hosted-runner", "/orgs/devantler-tech/actions/runner-groups/10/runners", func(v map[string]any) { v["runners"] = []any{map[string]any{"id": 1}}; v["total_count"] = 1 }},
		{"hosted-runner", "/orgs/devantler-tech/actions/runner-groups/10/hosted-runners", func(v map[string]any) { v["runners"] = []any{map[string]any{"id": 1}}; v["total_count"] = 1 }},
		{"wrong-repository-id", "/repos/devantler-tech/ksail", func(v map[string]any) { v["id"] = 42 }},
		{"private-repository", "/repos/devantler-tech/ksail", func(v map[string]any) { v["private"] = true }},
		{"broad-token", "/app/installations/42/access_tokens", func(v map[string]any) { v["permissions"].(map[string]any)["contents"] = "write" }},
		{"broad-selection", "/app/installations/42/access_tokens", func(v map[string]any) { v["repository_selection"] = "all" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &groupFixture{groups: map[int64]map[string]any{10: fixtureGroup(10, "platform")}, mutate: func(path string, v map[string]any) {
				if path == test.path {
					test.mutation(v)
				}
			}}
			if got := exerciseGroup(t, f); strings.HasPrefix(string(got), "PASS") {
				t.Fatalf("incomplete proof passed: %s", got)
			}
			if f.created || f.deleted || !f.revoked {
				t.Fatal("unexpected mutation or missing token revocation")
			}
		})
	}
}

func TestRunnerGroupCapabilityCompletePagination(t *testing.T) {
	f := &groupFixture{groups: map[int64]map[string]any{}}
	for id := int64(1); id <= 101; id++ {
		f.groups[id] = fixtureGroup(id, fmt.Sprintf("unrelated-%d", id))
	}
	f.groups[101] = fixtureGroup(101, "platform")
	if got := exerciseGroup(t, f); got != "PASS_EXISTING" {
		t.Fatalf("got %s", got)
	}
	if !slices.Contains(f.requests, "GET /orgs/devantler-tech/actions/runner-groups?per_page=100&page=2") {
		t.Fatal("second page not examined")
	}
}

func TestRunnerGroupCapabilityCleanupFailuresNeverPass(t *testing.T) {
	for _, kind := range []string{"delete", "revoke", "runner-appeared"} {
		t.Run(kind, func(t *testing.T) {
			f := &groupFixture{groups: map[int64]map[string]any{}, deleteFailure: kind == "delete", revokeFailure: kind == "revoke", cleanupRunner: kind == "runner-appeared"}
			if got := exerciseGroup(t, f); got != "FAIL_CLEANUP" {
				t.Fatalf("got %s", got)
			}
			if kind == "runner-appeared" && f.deleted {
				t.Fatal("deleted a group with a newly registered runner")
			}
		})
	}
}

func TestRunnerGroupCapabilityMissingPermissionIsHold(t *testing.T) {
	f := &groupFixture{groups: map[int64]map[string]any{}, deny: "/orgs/devantler-tech/actions/runner-groups"}
	if got := exerciseGroup(t, f); got != "HOLD_CAPABILITY" {
		t.Fatalf("got %s", got)
	}
	if f.created || f.deleted || !f.revoked {
		t.Fatal("scope refusal caused mutation or leaked token")
	}
}

func TestRunnerGroupCapabilityMalformedEvidence(t *testing.T) {
	for _, body := range []string{`{"total_count":0,"total_count":1,"runner_groups":[]}`, `{"total_count":0,"TOTAL_COUNT":1,"runner_groups":[]}`, `{"total_count":0,"runner_groups":[]} {}`, `{"total_count":-1,"runner_groups":[]}`, `{"total_count":0,"runner_groups":null}`, strings.Repeat(" ", maxResponse+1)} {
		t.Run(fmt.Sprint(len(body), body[:min(25, len(body))]), func(t *testing.T) {
			f := &groupFixture{groups: map[int64]map[string]any{}, rawPath: runnerGroupsPath, rawBody: body}
			if got := exerciseGroup(t, f); got != "HOLD_CAPABILITY" {
				t.Fatalf("got %s", got)
			}
			if f.created || f.deleted || !f.revoked {
				t.Fatal("malformed evidence caused mutation or missing revocation")
			}
		})
	}
}

func TestRunnerGroupCapabilityExpiredTokenIsRevoked(t *testing.T) {
	for _, expiry := range []time.Time{time.Now().Add(-time.Hour), time.Now().Add(2 * time.Hour)} {
		f := &groupFixture{groups: map[int64]map[string]any{}, mutate: func(path string, v map[string]any) {
			if path == "/app/installations/42/access_tokens" {
				v["expires_at"] = expiry.UTC().Format(time.RFC3339)
			}
		}}
		if got := exerciseGroup(t, f); got != "HOLD_CAPABILITY" {
			t.Fatalf("got %s", got)
		}
		if f.created || !f.revoked {
			t.Fatal("invalid token expiry was used or not revoked")
		}
	}
}

func TestRunnerGroupCapabilityBadCreatedPolicyIsRemoved(t *testing.T) {
	f := &groupFixture{groups: map[int64]map[string]any{}, mutate: func(path string, v map[string]any) {
		if path == runnerGroupsPath+"/99" {
			v["restricted_to_workflows"] = false
		}
	}}
	if got := exerciseGroup(t, f); got != "HOLD_CAPABILITY" {
		t.Fatalf("got %s", got)
	}
	if !f.created || !f.deleted || !f.revoked {
		t.Fatal("owned group survived rejected readback")
	}
}

func TestRunnerGroupCapabilityInvalidCreateFlagsStillCleanOwnedGroup(t *testing.T) {
	for _, field := range []string{"default", "inherited"} {
		for _, missing := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/missing=%t", field, missing), func(t *testing.T) {
				f := &groupFixture{groups: map[int64]map[string]any{}, mutate: func(path string, value map[string]any) {
					if path == runnerGroupsPath && value["id"] != nil {
						if missing {
							delete(value, field)
						} else {
							value[field] = true
						}
					}
				}}
				if got := exerciseGroup(t, f); got != "HOLD_CAPABILITY" {
					t.Fatalf("owned create with invalid policy returned %s", got)
				}
				if !f.created || !f.deleted || !f.revoked || len(f.groups) != 0 {
					t.Fatal("owned group survived an invalid create response")
				}
			})
		}
	}
}

func TestRunnerGroupCapabilityUnsafeLiveFlagsReportCleanupFailure(t *testing.T) {
	for _, field := range []string{"default", "inherited"} {
		t.Run(field, func(t *testing.T) {
			f := &groupFixture{groups: map[int64]map[string]any{}}
			f.afterCreate = func() { f.groups[99][field] = true }
			if got := exerciseGroup(t, f); got != failCleanup {
				t.Fatalf("unsafe live owned group returned %s", got)
			}
			if !f.created || f.deleted || !f.revoked || len(f.groups) != 1 {
				t.Fatal("unsafe group was deleted or cleanup was silently accepted")
			}
		})
	}
}

func TestRunnerGroupCapabilityCancellationWithOwnershipCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := false
	f := &groupFixture{groups: map[int64]map[string]any{}, mutate: func(path string, _ map[string]any) {
		if path == runnerGroupsPath+"/99" && !cancelled {
			cancelled = true
			cancel()
		}
	}}
	server := httptest.NewTLSServer(f.handler(t))
	defer server.Close()
	got := verifyRunnerGroupCapability(ctx, verificationOptions{githubURL: server.URL, client: server.Client(), now: time.Now}, "fixture-app-jwt", 42, "31415")
	if strings.HasPrefix(string(got), "PASS") {
		t.Fatal("cancelled API proof passed")
	}
	if !f.deleted || !f.revoked {
		t.Fatal("proven owned group or installation token not removed after cancellation")
	}
}

func TestRunnerGroupCapabilityRefusesLeftoverBeforeExistingPass(t *testing.T) {
	f := &groupFixture{groups: map[int64]map[string]any{10: fixtureGroup(10, "platform"), 99: fixtureGroup(99, "ksail-capability-31415")}}
	if got := exerciseGroup(t, f); got != "HOLD_OWNERSHIP" {
		t.Fatalf("got %s", got)
	}
	if f.created || f.deleted || !f.revoked {
		t.Fatal("leftover group was adopted or token not revoked")
	}
}

func TestRunnerGroupCapabilityRefusesBroadenedSelectedToken(t *testing.T) {
	f := &groupFixture{groups: map[int64]map[string]any{}, mutate: func(path string, v map[string]any) {
		if path == "/installation/repositories" {
			v["total_count"] = 2
			v["repositories"] = append(v["repositories"].([]any), map[string]any{"id": 42})
		}
	}}
	if got := exerciseGroup(t, f); got != "HOLD_CAPABILITY" {
		t.Fatalf("got %s", got)
	}
	if f.created || f.deleted || !f.revoked {
		t.Fatal("broad token was used or not revoked")
	}
}

func TestRunnerGroupCapabilityMalformedExpiryStillRevokesToken(t *testing.T) {
	for _, invalid := range []any{"not-a-time", nil, 42} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			f := &groupFixture{groups: map[int64]map[string]any{}, mutate: func(path string, v map[string]any) {
				if path == "/app/installations/42/access_tokens" {
					v["expires_at"] = invalid
				}
			}}
			if got := exerciseGroup(t, f); got != "HOLD_CAPABILITY" {
				t.Fatalf("got %s", got)
			}
			if f.created || !f.revoked {
				t.Fatal("malformed token metadata bypassed revocation")
			}
		})
	}
}

func TestRunnerGroupCapabilityUncertainTokenMintReportsCleanupFailure(t *testing.T) {
	for _, response := range []struct {
		status int
		body   string
	}{
		{http.StatusCreated, `{}`},
		{http.StatusCreated, `{"token":"fixture-installation-token","token":"other"}`},
		{http.StatusInternalServerError, `{}`},
	} {
		t.Run(strconv.Itoa(response.status)+response.body, func(t *testing.T) {
			f := &groupFixture{groups: map[int64]map[string]any{}, rawPath: "/app/installations/42/access_tokens", rawStatus: response.status, rawBody: response.body}
			if got := exerciseGroup(t, f); got != failCleanup {
				t.Fatalf("uncertain mint reported %s instead of unproven cleanup", got)
			}
			if f.created || f.deleted {
				t.Fatal("uncertain token mint reached a group mutation")
			}
		})
	}
}

func TestRunnerGroupCapabilityUncertainCreateReportsOwnershipHold(t *testing.T) {
	for _, disconnected := range []bool{false, true} {
		t.Run(fmt.Sprint("disconnected=", disconnected), func(t *testing.T) {
			f := &groupFixture{groups: map[int64]map[string]any{}, rawPath: runnerGroupsPath, rawBody: `{"total_count":0,"runner_groups":[]}`}
			base := f.handler(t)
			writes := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == runnerGroupsPath {
					writes++
					if disconnected {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error("fixture could not close the uncertain write")
							return
						}
						_ = connection.Close()
						return
					}
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				base(w, r)
			}))
			defer server.Close()
			got := verifyRunnerGroupCapability(context.Background(), verificationOptions{githubURL: server.URL, client: server.Client(), now: time.Now}, "fixture-app-jwt", 42, "31415")
			if got != "HOLD_OWNERSHIP" || f.deleted || !f.revoked || writes != 1 {
				t.Fatalf("uncertain create: %s, deleted=%v revoked=%v writes=%d", got, f.deleted, f.revoked, writes)
			}
		})
	}
}
