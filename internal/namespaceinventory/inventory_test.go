package namespaceinventory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testNamespace = "longhorn-benchmark-v1-1234"

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A deadline may pass before its cancellation notification is delivered.
type unsignalledDeadline struct {
	context.Context
	expires *atomic.Int64
}

func (c unsignalledDeadline) Deadline() (time.Time, bool) {
	return time.Unix(0, c.expires.Load()), true
}

func TestOpaqueRootAndUnsignalledExpiredContextCannotStartRequests(t *testing.T) {
	for _, failure := range []string{"opaque root", "expired context"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") })}
			endpoint := &url.URL{Scheme: "https", Host: "reviewed.invalid"}
			ctx := context.Background()
			if failure == "opaque root" {
				endpoint.Opaque = "//other.invalid/arbitrary"
			} else {
				expires := &atomic.Int64{}
				expires.Store(time.Now().Add(-time.Second).UnixNano())
				ctx = unsignalledDeadline{context.Background(), expires}
			}
			result, err := Observe(ctx, client, endpoint, testNamespace, "namespace-uid")
			if err == nil || calls != 0 || len(result.Bindings()) != 0 {
				t.Fatalf("invalid boundary started requests: count=%d error=%v", calls, err)
			}
		})
	}
}

func TestObserveUsesEveryServedVersionAndMetadataOnly(t *testing.T) {
	result, err := observeFixture(t, "")
	if err != nil || len(result.Bindings()) != 2 {
		t.Fatalf("complete metadata inventory failed: count=%d error=%v", len(result.Bindings()), err)
	}
	for _, binding := range result.Bindings() {
		if binding.Resource != map[string]string{"Widget": "widgets.example.io", "Secret": "secrets"}[binding.Kind] {
			t.Fatal("group/resource identity was discarded or versioned")
		}
	}
}

func TestNativeEmptyMetadataListMayContainNullItems(t *testing.T) {
	result, err := observeFixture(t, "native empty list")
	if err != nil || len(result.Bindings()) != 0 {
		t.Fatalf("native complete empty list was refused: %v", err)
	}
}

func TestFixedLonghornCalibrationReadsMetadataWithoutRetirementAuthority(t *testing.T) {
	result, err := observeFixtureInNamespace(t, "", "longhorn-system")
	if err != nil || len(result.Bindings()) != 2 {
		t.Fatalf("fixed calibration failed: %v", err)
	}
	result, err = observeFixtureInNamespace(t, "native empty list", "longhorn-system")
	if err == nil || len(result.Bindings()) != 0 {
		t.Fatal("empty production calibration accepted")
	}
}

func TestRetainedEmailInventoryCoversAllServedKinds(t *testing.T) {
	result, err := observeFixtureInNamespace(t, "", "email-data-recovery-41561215415")
	if err != nil || len(result.Bindings()) != 2 {
		t.Fatalf("fixed Email inventory failed: %v", err)
	}
	for _, failure := range []string{"continued list", "changed discovery", "namespace replaced", "full Secret response", "duplicate UID"} {
		result, err = observeFixtureInNamespace(t, failure, "email-data-recovery-41561215415")
		if err == nil || len(result.Bindings()) != 0 {
			t.Fatalf("incomplete Email inventory accepted: %s", failure)
		}
	}
	if result, err = Observe(context.Background(), nil, nil, "email-data-recovery-41561215416", "uid"); err == nil || len(result.Bindings()) != 0 {
		t.Fatal("arbitrary Email namespace admitted by general benchmark API")
	}
}

func TestFreshRetainedEmailInventoryHasItsOwnFixedNamespace(t *testing.T) {
	const namespace = "email-data-recovery-42818236850"
	result, err := observeFixtureInNamespace(t, "", namespace)
	if err != nil || len(result.Bindings()) != 2 {
		t.Fatalf("fixed fresh Email inventory failed: %v", err)
	}
	result, err = observeFixtureInNamespace(t, "namespace replaced", namespace)
	if err == nil || len(result.Bindings()) != 0 {
		t.Fatal("a replaced fresh namespace was accepted")
	}
	requests := 0
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected request")
	})}
	endpoint := &url.URL{Scheme: "https", Host: "reviewed.invalid"}
	if namespacePattern.MatchString(namespace) {
		t.Fatal("fresh recovery namespace overlaps the benchmark namespace grammar")
	}
	if result, err = Observe(context.Background(), client, endpoint, namespace, "uid"); err == nil || requests != 0 || len(result.Bindings()) != 0 {
		t.Fatal("fresh namespace admitted by the benchmark API")
	}
}

// One stored Event is served by core v1 and events.k8s.io under the same UID.
// That alias is one object; any other identity difference stays a conflict.
func TestEventAliasesAreOneObject(t *testing.T) {
	result, err := observeFixtureInNamespace(t, "event alias", "email-data-recovery-41561215415")
	if err != nil {
		t.Fatalf("Event aliases were read as conflicting identities: %v", err)
	}
	events := 0
	for _, binding := range result.Bindings() {
		if binding.Kind == "Event" {
			events++
		}
	}
	if len(result.Bindings()) != 3 || events != 1 {
		t.Fatalf("want secret, widget and one Event, got %d bindings with %d Events", len(result.Bindings()), events)
	}
	if result, err = observeFixtureInNamespace(t, "event alias renamed", "email-data-recovery-41561215415"); err == nil || len(result.Bindings()) != 0 {
		t.Fatal("two different Events with one UID were accepted")
	}
}

func TestIncompleteOrUnboundObservationDiscardsEverything(t *testing.T) {
	for _, failure := range []string{"discovery failed", "changed discovery", "wrong group version", "missing namespace scope", "missing list verb", "remote deletable API", "partial APIService roster", "changed APIService roster", "full Secret response", "missing items", "continued list", "unknown remainder", "duplicate UID", "duplicate UID different kind", "duplicate identical item", "namespace replaced", "deadline on final read", "redirect", "oversized", "duplicate field"} {
		t.Run(failure, func(t *testing.T) {
			result, err := observeFixture(t, failure)
			if err == nil || len(result.Bindings()) != 0 {
				t.Fatal("partial or unbound inventory was retained")
			}
		})
	}
}

func observeFixture(t *testing.T, failure string) (Inventory, error) {
	return observeFixtureInNamespace(t, failure, testNamespace)
}

func observeFixtureInNamespace(t *testing.T, failure, namespace string) (Inventory, error) {
	t.Helper()
	testNamespace := namespace
	discoveries, namespaces := 0, 0
	expires := &atomic.Int64{}
	expires.Store(time.Now().Add(time.Minute).UnixNano())
	seen := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil && failure == "" {
				t.Errorf("fixture response failed: %v", err)
			}
		}
		if r.Method != "GET" || r.ContentLength > 0 {
			t.Error("inventory attempted a non-read request")
			w.WriteHeader(500)
			return
		}
		seen[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/apis/apiregistration.k8s.io/v1/apiservices" {
			continuation, rv := "", "10"
			if failure == "partial APIService roster" {
				continuation = "next"
			}
			if failure == "changed APIService roster" && seen[r.URL.Path] > 1 {
				rv = "11"
			}
			service := ""
			if failure == "remote deletable API" {
				service = `,"service":{"name":"remote","namespace":"system"}`
			}
			respond(fmt.Sprintf(`{"apiVersion":"apiregistration.k8s.io/v1","kind":"APIServiceList","metadata":{"resourceVersion":"1","continue":%q},"items":[{"metadata":{"name":"v1.example.io","uid":"api-service","resourceVersion":%q},"spec":{"group":"example.io","version":"v1"%s}}]}`, continuation, rv, service))
			return
		}
		if r.URL.Path == "/api/v1/namespaces/"+testNamespace {
			namespaces++
			if failure == "deadline on final read" && namespaces > 1 {
				expires.Store(time.Now().Add(-time.Second).UnixNano())
			}
			if r.Header.Get("Accept") != metadataObjectAccept {
				t.Error("Namespace was fetched without metadata-only negotiation")
			}
			uid := "namespace-uid"
			if failure == "namespace replaced" && namespaces > 1 {
				uid = "replacement"
			}
			respond(fmt.Sprintf(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":%q,"uid":%q,"resourceVersion":"7"}}`, testNamespace, uid))
			return
		}
		if r.URL.Path == "/api" {
			discoveries++
			if failure == "discovery failed" {
				w.WriteHeader(503)
				return
			}
			respond(`{"apiVersion":"v1","kind":"APIVersions","versions":["v1"]}`)
			return
		}
		if r.URL.Path == "/apis" {
			if failure == "changed discovery" && discoveries > 1 {
				respond(`{"apiVersion":"v1","kind":"APIGroupList","groups":[]}`)
				return
			}
			events := ""
			if strings.HasPrefix(failure, "event alias") {
				events = `,{"name":"events.k8s.io","versions":[{"groupVersion":"events.k8s.io/v1","version":"v1"}]}`
			}
			respond(`{"apiVersion":"v1","kind":"APIGroupList","groups":[{"name":"example.io","versions":[{"groupVersion":"example.io/v1","version":"v1"},{"groupVersion":"example.io/v2","version":"v2"}]}` + events + `]}`)
			return
		}
		gv := ""
		switch r.URL.Path {
		case "/api/v1":
			gv = "v1"
		case "/apis/example.io/v1":
			gv = "example.io/v1"
		case "/apis/example.io/v2":
			gv = "example.io/v2"
		case "/apis/events.k8s.io/v1":
			gv = "events.k8s.io/v1"
		}
		if gv != "" {
			if failure == "wrong group version" {
				gv = "wrong/v1"
			}
			resource, kind := "secrets", "Secret"
			if strings.HasPrefix(gv, "example.io/") {
				resource, kind = "widgets", "Widget"
			}
			if gv == "events.k8s.io/v1" {
				resource, kind = "events", "Event"
			}
			verbs := `["get","list","delete"]`
			if failure == "missing list verb" {
				verbs = `["get","delete"]`
			}
			body := fmt.Sprintf(`{"apiVersion":"v1","kind":"APIResourceList","groupVersion":%q,"resources":[{"name":%q,"kind":%q,"namespaced":true,"verbs":%s},{"name":"widgets/status","kind":"Widget","namespaced":true,"verbs":["get","update"]}]}`, gv, resource, kind, verbs)
			if failure == "missing namespace scope" {
				body = strings.Replace(body, `"namespaced":true,`, "", 1)
			}
			if strings.HasPrefix(failure, "event alias") && gv == "v1" {
				body = strings.Replace(body, `"resources":[`, `"resources":[{"name":"events","kind":"Event","namespaced":true,"verbs":["get","list","delete"]},`, 1)
			}
			respond(body)
			return
		}
		if strings.Contains(r.URL.Path, "/namespaces/"+testNamespace+"/") {
			if r.Header.Get("Accept") != metadataListAccept || r.URL.RawQuery != "limit=256" {
				t.Error("collection read was not fixed, complete and metadata-only")
			}
			if failure == "redirect" {
				http.Redirect(w, r, "/unapproved", http.StatusFound)
				return
			}
			if failure == "oversized" {
				respond(strings.Repeat("x", 2*1024*1024))
				return
			}
			if failure == "full Secret response" {
				respond(`{"apiVersion":"v1","kind":"SecretList","metadata":{"resourceVersion":"8"},"items":[]}`)
				return
			}
			if failure == "native empty list" {
				respond(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"8"},"items":null}`)
				return
			}
			if failure == "missing items" {
				respond(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"8"}}`)
				return
			}
			name, uid := "widget", "widget-uid"
			if strings.HasSuffix(r.URL.Path, "/secrets") {
				name, uid = "token", "secret-uid"
			}
			if failure == "duplicate UID different kind" {
				name, uid = "same-name", "same-uid"
			}
			if strings.HasSuffix(r.URL.Path, "/events") {
				name, uid = "reader.1", "event-uid"
				if failure == "event alias renamed" && strings.HasPrefix(r.URL.Path, "/apis/") {
					name = "other.1"
				}
			}
			if failure == "duplicate UID" && strings.HasSuffix(r.URL.Path, "/v2/namespaces/"+testNamespace+"/widgets") {
				name = "different-widget"
			}
			continuation, remainder := "", ""
			if failure == "continued list" {
				continuation = "next"
			}
			if failure == "unknown remainder" {
				remainder = `,"remainingItemCount":1`
			}
			body := fmt.Sprintf(`{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","metadata":{"resourceVersion":"8","continue":%q%s},"items":[{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":%q,"namespace":%q,"uid":%q,"resourceVersion":"9"}}]}`, continuation, remainder, name, testNamespace, uid)
			if failure == "duplicate field" {
				body = strings.Replace(body, `"uid":`, `"uid":"foreign","uid":`, 1)
			}
			if failure == "duplicate identical item" {
				prefix, items, _ := strings.Cut(body, `"items":[`)
				item := strings.TrimSuffix(items, "]}")
				body = prefix + `"items":[` + item + "," + item + "]}"
			}
			respond(body)
			return
		}
		t.Errorf("unapproved request target: %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL)
	ctx := context.Context(context.Background())
	if failure == "deadline on final read" {
		ctx = unsignalledDeadline{context.Background(), expires}
	}
	var result Inventory
	var err error
	switch namespace {
	case "longhorn-system":
		result, err = CalibrateLonghornSystem(ctx, server.Client(), endpoint, "namespace-uid")
	case "email-data-recovery-41561215415":
		result, err = ObserveRetainedEmailRecovery(ctx, server.Client(), endpoint, "namespace-uid")
	case "email-data-recovery-42818236850":
		result, err = ObserveFreshRetainedEmailRecovery(ctx, server.Client(), endpoint, "namespace-uid")
	default:
		result, err = Observe(ctx, server.Client(), endpoint, testNamespace, "namespace-uid")
	}
	server.Close()
	if failure == "" && (seen["/apis/example.io/v1/namespaces/"+testNamespace+"/widgets"] != 1 || seen["/apis/example.io/v2/namespaces/"+testNamespace+"/widgets"] != 1) {
		t.Error("a served version was omitted")
	}
	return result, err
}
