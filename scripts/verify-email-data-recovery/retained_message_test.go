package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const retainedFixtureNonce = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type retainedMessageServer struct {
	calls  map[string]int
	mutate func(string, int, object)
	raw    func(string, http.ResponseWriter) bool
}

// These independent HTTP receipts exercise the real client, including its
// authentication, request capability, response bounds and repeated readback.
func retainedMessageEndpoint(t *testing.T, f *retainedMessageServer) string {
	t.Helper()
	f.calls = map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz/ready" && r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "ned@devantler.tech" || password != "private-retained-fixture" {
			t.Error("readback lost its fixed account authentication")
			http.Error(w, "private-provider-canary", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/jmap/session" {
			f.calls["session"]++
			_, _ = io.WriteString(w, `{"primaryAccounts":{"urn:ietf:params:jmap:mail":"company-account"},"accounts":{"company-account":{"isReadOnly":false}}}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/jmap" {
			t.Error("a mailbox write or foreign endpoint reached the server")
			http.Error(w, "private-provider-canary", http.StatusForbidden)
			return
		}
		var envelope object
		if json.NewDecoder(r.Body).Decode(&envelope) != nil {
			t.Error("invalid readback envelope")
			return
		}
		calls, _ := envelope["methodCalls"].([]any)
		if len(calls) != 1 {
			t.Error("readback sent multiple methods")
			return
		}
		tuple, _ := calls[0].([]any)
		if len(tuple) != 3 || tuple[2] != "proof" {
			t.Error("readback lost its call identity")
			return
		}
		method, _ := tuple[0].(string)
		args, _ := tuple[1].(map[string]any)
		if text(args, "accountId") != "company-account" {
			t.Error("readback selected a different account")
			return
		}
		f.calls[method]++
		if f.raw != nil && f.raw(method, w) {
			return
		}
		result := object{"accountId": "company-account"}
		switch method {
		case "Mailbox/get":
			if _, present := args["ids"]; present || !reflect.DeepEqual(args["properties"], []any{"id", "name", "isSubscribed"}) {
				t.Error("mailbox discovery did not request the complete bounded list")
			}
			result["state"] = "mailbox-state"
			result["notFound"] = []any{}
			result["list"] = []any{object{"id": "inbox", "name": "Inbox", "isSubscribed": true}, object{"id": "diagnostic-box", "name": "Email recovery verification " + retainedFixtureNonce, "isSubscribed": false}}
		case "Email/query":
			if !reflect.DeepEqual(args["filter"], object{"inMailbox": "diagnostic-box"}) || args["calculateTotal"] != true || args["position"] != float64(0) || args["limit"] != float64(2) {
				t.Error("diagnostic query lost its mailbox or completeness bound")
			}
			result["queryState"] = "query-state"
			result["canCalculateChanges"] = false
			result["position"] = float64(0)
			result["total"] = float64(1)
			result["ids"] = []any{"diagnostic-email"}
		case "Email/get":
			if !reflect.DeepEqual(args["ids"], []any{"diagnostic-email"}) || args["fetchTextBodyValues"] != true {
				t.Error("message readback lost its exact identity or body")
			}
			result["state"] = "email-state"
			result["notFound"] = []any{}
			result["list"] = []any{object{"id": "diagnostic-email", "blobId": "diagnostic-blob", "messageId": []any{"email-recovery-" + retainedFixtureNonce + "@example.invalid"}, "mailboxIds": object{"diagnostic-box": true}, "textBody": []any{object{"partId": "1"}}, "bodyValues": object{"1": object{"value": "Email recovery verification " + retainedFixtureNonce + "\r\n", "isTruncated": false, "isEncodingProblem": false}}}}
		default:
			t.Error("a mutating or unexpected JMAP method reached the server")
			http.Error(w, "private-provider-canary", http.StatusForbidden)
			return
		}
		if f.mutate != nil {
			f.mutate(method, f.calls[method], result)
		}
		_ = json.NewEncoder(w).Encode(object{"methodResponses": []any{[]any{method, result, "proof"}}})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func retainedFixtureClient(t *testing.T, endpoint string) *mailboxClient {
	t.Helper()
	c, err := newRetainedMailboxClient(endpoint, "private-retained-fixture")
	if err != nil {
		t.Fatal("local retained endpoint was refused")
	}
	return c
}

// Removing either endpoint's repeated discovery would miss content or folder
// changes after its initial matching read.
func TestRetainedMessageReadsMatchingDiagnosticTwiceWithoutWrites(t *testing.T) {
	r, s := &retainedMessageServer{}, &retainedMessageServer{}
	reader := retainedFixtureClient(t, retainedMessageEndpoint(t, r))
	source := retainedFixtureClient(t, retainedMessageEndpoint(t, s))
	identity, err := verifyRetainedMessage(context.Background(), reader, source)
	if err != nil || identity != (messageIdentity{email: "diagnostic-email", blob: "diagnostic-blob", mailbox: "diagnostic-box", messageID: "email-recovery-" + retainedFixtureNonce + "@example.invalid", body: "Email recovery verification " + retainedFixtureNonce}) {
		t.Fatal("matching retained diagnostic was not verified")
	}
	for _, endpoint := range []*retainedMessageServer{r, s} {
		if !reflect.DeepEqual(endpoint.calls, map[string]int{"session": 1, "Mailbox/get": 2, "Email/query": 2, "Email/get": 2}) {
			t.Fatal("readback did not repeat the complete diagnostic observation")
		}
	}
}

// Bypassing the HTTP capability would let a future accidental call to an
// existing mutating mailbox helper reach the actual server.
func TestRetainedMessageCapabilityRejectsWritesAtHTTPBoundary(t *testing.T) {
	reached := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	c := retainedFixtureClient(t, server.URL)
	c.account = "company-account"
	for _, method := range []string{"Email/import", "Email/set", "Mailbox/set"} {
		if _, err := c.call(context.Background(), method, object{}); err == nil {
			t.Fatal("mutating JMAP method was accepted")
		}
	}
	for _, req := range []struct{ method, path, content, body string }{
		{"POST", "/jmap/upload/company-account/", "message/rfc822", "private-message-canary"},
		{"DELETE", "/jmap", "application/json", `{}`},
		{"GET", "/jmap?foreign=1", "", ""},
		{"GET", "/jmap/session", "", "private-message-canary"},
		{"POST", "/jmap", "application/json", `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/get",{"accountId":"company-account"},"proof"],["Email/set",{"accountId":"company-account"},"write"]]}`},
		{"POST", "/jmap", "application/json", `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/get",{"accountId":"foreign-account"},"proof"]]}`},
		{"POST", "/jmap", "application/json", strings.Repeat("x", 65537)},
	} {
		if _, err := c.request(context.Background(), req.method, req.path, req.content, []byte(req.body)); err == nil {
			t.Fatal("foreign endpoint, write, account or oversized envelope was accepted")
		}
	}
	if reached != 0 {
		t.Fatal("read-only capability forwarded a refused request")
	}
}

// Each mutation violates an independent completeness, uniqueness, identity or
// exact diagnostic-content requirement; none may become recovery evidence.
func TestRetainedMessageRefusesAmbiguousIncompleteOrChangedDiagnostics(t *testing.T) {
	cases := map[string]func(string, int, object){
		"absent": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				o["list"] = []any{}
			}
		},
		"duplicate-diagnostic": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				o["list"] = append(o["list"].([]any), object{"id": "second-box", "name": "Email recovery verification " + strings.Repeat("a", 64), "isSubscribed": false})
			}
		},
		"duplicate-mailbox-id": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				items(o, "list")[0]["id"] = "diagnostic-box"
			}
		},
		"invalid-mailbox-id": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				items(o, "list")[0]["id"] = "foreign/path"
			}
		},
		"subscribed": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				items(o, "list")[1]["isSubscribed"] = true
			}
		},
		"wrong-nonce": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				items(o, "list")[1]["name"] = "Email recovery verification " + strings.Repeat("a", 63)
			}
		},
		"mailbox-pagination": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				o["position"] = float64(1)
			}
		},
		"mailbox-not-found": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				o["notFound"] = []any{"missing-box"}
			}
		},
		"mailbox-bound": func(method string, _ int, o object) {
			if method == "Mailbox/get" {
				for n := 0; n < 257; n++ {
					o["list"] = append(o["list"].([]any), object{"id": "box-" + strings.Repeat("a", n+1), "name": "other", "isSubscribed": false})
				}
			}
		},
		"query-pagination": func(method string, _ int, o object) {
			if method == "Email/query" {
				o["position"] = float64(1)
			}
		},
		"query-incomplete-total": func(method string, _ int, o object) {
			if method == "Email/query" {
				o["total"] = float64(2)
			}
		},
		"query-missing-total": func(method string, _ int, o object) {
			if method == "Email/query" {
				delete(o, "total")
			}
		},
		"query-foreign": func(method string, _ int, o object) {
			if method == "Email/query" {
				o["ids"] = []any{"diagnostic-email", "foreign-email"}
				o["total"] = float64(2)
			}
		},
		"query-duplicate": func(method string, _ int, o object) {
			if method == "Email/query" {
				o["ids"] = []any{"diagnostic-email", "diagnostic-email"}
				o["total"] = float64(2)
			}
		},
		"query-invalid-id": func(method string, _ int, o object) {
			if method == "Email/query" {
				o["ids"] = []any{"foreign/path"}
			}
		},
		"message-not-found": func(method string, _ int, o object) {
			if method == "Email/get" {
				o["notFound"] = []any{"diagnostic-email"}
			}
		},
		"message-foreign": func(method string, _ int, o object) {
			if method == "Email/get" {
				items(o, "list")[0]["mailboxIds"] = object{"diagnostic-box": true, "inbox": true}
			}
		},
		"message-invalid-blob": func(method string, _ int, o object) {
			if method == "Email/get" {
				items(o, "list")[0]["blobId"] = "blob/path"
			}
		},
		"message-wrong-id": func(method string, _ int, o object) {
			if method == "Email/get" {
				items(o, "list")[0]["messageId"] = []any{"foreign@example.invalid"}
			}
		},
		"message-wrong-body": func(method string, _ int, o object) {
			if method == "Email/get" {
				child(child(items(o, "list")[0], "bodyValues"), "1")["value"] = "private-foreign-body"
			}
		},
		"message-truncated": func(method string, _ int, o object) {
			if method == "Email/get" {
				child(child(items(o, "list")[0], "bodyValues"), "1")["isTruncated"] = true
			}
		},
		"message-encoding": func(method string, _ int, o object) {
			if method == "Email/get" {
				child(child(items(o, "list")[0], "bodyValues"), "1")["isEncodingProblem"] = true
			}
		},
		"changed-message": func(method string, n int, o object) {
			if method == "Email/get" && n == 2 {
				items(o, "list")[0]["blobId"] = "replacement-blob"
			}
		},
		"changed-mailbox": func(method string, n int, o object) {
			if method == "Mailbox/get" && n == 2 {
				items(o, "list")[1]["isSubscribed"] = true
			}
		},
	}
	for name, mutate := range cases {
		for _, endpoint := range []string{"reader", "source"} {
			t.Run(name+"/"+endpoint, func(t *testing.T) {
				r, s := &retainedMessageServer{}, &retainedMessageServer{}
				if endpoint == "reader" {
					r.mutate = mutate
				} else {
					s.mutate = mutate
				}
				_, err := verifyRetainedMessage(context.Background(), retainedFixtureClient(t, retainedMessageEndpoint(t, r)), retainedFixtureClient(t, retainedMessageEndpoint(t, s)))
				if err == nil {
					t.Fatal("ambiguous, incomplete or changed diagnostic was accepted")
				}
				for _, secret := range []string{retainedFixtureNonce, "private-foreign-body", "private-retained-fixture", "diagnostic-box"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatal("private diagnostic data escaped in an error")
					}
				}
			})
		}
	}
}

func TestRetainedMessageRefusesOversizedAndProviderErrorBodiesPrivately(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider-error", true: "oversized"}[oversized], func(t *testing.T) {
			r := &retainedMessageServer{raw: func(method string, w http.ResponseWriter) bool {
				if method != "Mailbox/get" {
					return false
				}
				if oversized {
					_, _ = io.WriteString(w, strings.Repeat("x", (4<<20)+1))
				} else {
					http.Error(w, "private-provider-canary", http.StatusServiceUnavailable)
				}
				return true
			}}
			s := &retainedMessageServer{}
			_, err := verifyRetainedMessage(context.Background(), retainedFixtureClient(t, retainedMessageEndpoint(t, r)), retainedFixtureClient(t, retainedMessageEndpoint(t, s)))
			if err == nil || strings.Contains(err.Error(), "private-") {
				t.Fatal("oversized or private provider response was accepted or exposed")
			}
		})
	}
}
