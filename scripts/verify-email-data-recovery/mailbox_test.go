package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMailboxCallUsesAuthenticatedExactMethodReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "ned@devantler.tech" || password != "fixture-only" || r.URL.Path != "/jmap" || r.Method != http.MethodPost {
			http.Error(w, "rejected", http.StatusUnauthorized)
			return
		}
		var body object
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "request", http.StatusBadRequest)
			return
		}
		calls, _ := body["methodCalls"].([]any)
		if len(calls) != 1 {
			http.Error(w, "calls", http.StatusBadRequest)
			return
		}
		call, _ := calls[0].([]any)
		if len(call) != 3 || call[0] != "Email/get" || call[2] != "proof" {
			http.Error(w, "method", http.StatusBadRequest)
			return
		}
		args, _ := call[1].(map[string]any)
		if text(args, "accountId") != "company-account" {
			http.Error(w, "account", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"methodResponses":[["Email/get",{"accountId":"company-account","state":"s1","list":[{"id":"known-message"}]},"proof"]]}`)
	}))
	defer server.Close()
	client, err := newMailboxClient(server.URL, "fixture-only")
	if err != nil {
		t.Fatal("local diagnostic endpoint was refused")
	}
	client.account = "company-account"
	result, err := client.call(context.Background(), "Email/get", object{"ids": []any{"known-message"}})
	if err != nil || text(result, "state") != "s1" || len(items(result, "list")) != 1 {
		t.Fatal("authenticated mailbox read failed")
	}
}

func TestMailboxRejectsForeignMethodAccountAndCallReceipts(t *testing.T) {
	for name, response := range map[string]string{
		"error":         `{"methodResponses":[["error",{"type":"forbidden"},"proof"]]}`,
		"wrong-method":  `{"methodResponses":[["Email/set",{"accountId":"company-account"},"proof"]]}`,
		"wrong-account": `{"methodResponses":[["Email/get",{"accountId":"other-account"},"proof"]]}`,
		"wrong-call":    `{"methodResponses":[["Email/get",{"accountId":"company-account"},"other"]]}`,
		"missing":       `{"methodResponses":[]}`,
		"multiple":      `{"methodResponses":[["Email/get",{"accountId":"company-account"},"proof"],["Email/get",{"accountId":"company-account"},"proof"]]}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, response) }))
			defer server.Close()
			client, _ := newMailboxClient(server.URL, "fixture-only")
			client.account = "company-account"
			if _, err := client.call(context.Background(), "Email/get", object{}); err == nil {
				t.Fatal("foreign or incomplete receipt was accepted")
			}
		})
	}
}

func TestMailboxCredentialsNeverFollowRedirectOrErrorText(t *testing.T) {
	forwarded := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, "private-mail-body-do-not-log")
	}))
	defer server.Close()
	client, _ := newMailboxClient(server.URL, "fixture-only")
	client.account = "company-account"
	_, err := client.call(context.Background(), "Email/get", object{})
	if err == nil || forwarded || strings.Contains(err.Error(), "private-mail-body") || strings.Contains(err.Error(), "fixture-only") {
		t.Fatal("redirect or provider error exposed private data")
	}
	for _, endpoint := range []string{"http://example.com", "http://127.0.0.1:1/foreign", "http://user:password@127.0.0.1:1", "https://other.example.com"} {
		if _, err := newMailboxClient(endpoint, "fixture-only"); err == nil {
			t.Fatal("unowned diagnostic endpoint accepted")
		}
	}
}

func TestRecoveryMarkerCleanupCannotDeleteOtherMessages(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "foreign-message"}[foreign], func(t *testing.T) {
			imported, mailDeleted, boxDeleted := false, false, false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/jmap/upload/company-account/" {
					data, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(data), "Message-ID: <email-recovery-"+nonce+"@example.invalid>") {
						t.Error("uploaded message lost its unique identity")
					}
					_, _ = io.WriteString(w, `{"accountId":"company-account","blobId":"upload-blob"}`)
					return
				}
				var envelope object
				_ = json.NewDecoder(r.Body).Decode(&envelope)
				calls, _ := envelope["methodCalls"].([]any)
				if len(calls) != 1 {
					http.Error(w, "calls", http.StatusBadRequest)
					return
				}
				tuple, _ := calls[0].([]any)
				method, _ := tuple[0].(string)
				args, _ := tuple[1].(map[string]any)
				result := object{"accountId": "company-account", "state": "s1"}
				switch method {
				case "Mailbox/set":
					if args["create"] != nil {
						created := child(args, "create")
						box := child(created, "proof")
						if box["isSubscribed"] != false {
							t.Error("test folder was subscribed")
						}
						result["created"] = object{"proof": object{"id": "owned-box"}}
					} else {
						if !mailDeleted || args["ifInState"] != "s1" {
							t.Error("mailbox deleted before safe message cleanup")
						}
						boxDeleted = true
						result["destroyed"] = []any{"owned-box"}
					}
				case "Email/import":
					imported = true
					email := child(child(args, "emails"), "proof")
					if text(email, "blobId") != "upload-blob" || child(email, "mailboxIds")["owned-box"] != true {
						t.Error("import escaped its own mailbox")
					}
					result["created"] = object{"proof": object{"id": "owned-email", "blobId": "owned-blob"}}
				case "Email/get":
					result["list"] = []any{object{"id": "owned-email", "blobId": "owned-blob", "messageId": []any{"email-recovery-" + nonce + "@example.invalid"}, "mailboxIds": object{"owned-box": true}, "textBody": []any{object{"partId": "1"}}, "bodyValues": object{"1": object{"value": "Email recovery verification " + nonce + "\r\n", "isTruncated": false}}}}
				case "Email/query":
					ids := []any{}
					if imported && !mailDeleted {
						ids = append(ids, "owned-email")
					}
					if foreign {
						ids = append(ids, "foreign-email")
					}
					result["ids"] = ids
					result["total"] = len(ids)
				case "Email/set":
					ids, _ := args["destroy"].([]any)
					if len(ids) != 1 || ids[0] != "owned-email" || args["ifInState"] != "s1" {
						t.Error("message cleanup lost its identity/state fence")
					}
					mailDeleted = true
					result["destroyed"] = []any{"owned-email"}
				case "Mailbox/get":
					result["list"] = []any{object{"id": "owned-box", "name": "Email recovery verification " + nonce, "isSubscribed": false}}
				default:
					t.Error("unexpected mailbox operation")
				}
				_ = json.NewEncoder(w).Encode(object{"methodResponses": []any{[]any{method, result, "proof"}}})
			}))
			defer server.Close()
			client, _ := newMailboxClient(server.URL, "fixture-only")
			client.account = "company-account"
			marker, err := client.createMarker(context.Background(), nonce)
			if err != nil || !imported {
				t.Fatal("own recovery marker was not imported")
			}
			err = client.cleanupMarker(context.Background(), marker)
			if foreign {
				if err == nil || mailDeleted || boxDeleted {
					t.Fatal("foreign mailbox content allowed destructive cleanup")
				}
			} else if err != nil || !mailDeleted || !boxDeleted {
				t.Fatal("owned test message and folder were not cleaned up")
			}
		})
	}
}
