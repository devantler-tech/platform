package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// censusStore is a JMAP account in the response shapes the pinned server
// returned for a disposable user on 2026-11-25.
type censusStore struct {
	mailboxes, messages, calendars, identities, scripts int
	objects                                             []string // JSCalendar @type per calendar object
	vacation                                            bool
	mutate                                              func(method string, args, result object)
	methods                                             []string
}

func (s *censusStore) serve(t *testing.T) *mailboxClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body object
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "request", http.StatusBadRequest)
			return
		}
		call := body["methodCalls"].([]any)[0].([]any)
		method, args := call[0].(string), call[1].(map[string]any)
		s.methods = append(s.methods, method)
		using := body["using"].([]any)
		if len(using) != 2 || using[0] != "urn:ietf:params:jmap:core" || using[1] != mailboxMethods[method] {
			t.Errorf("%s did not declare exactly its capability", method)
		}
		query := func(total int) object {
			ids := []any{}
			position, _ := args["position"].(float64)
			limit, _ := args["limit"].(float64)
			for i := int(position); i < total && i < int(position)+int(limit); i++ {
				ids = append(ids, fmt.Sprintf("id%d", i))
			}
			return object{"queryState": "q1", "canCalculateChanges": true, "position": position, "ids": ids, "total": float64(total)}
		}
		listed := func(count int) object {
			list := []any{}
			for i := range count {
				list = append(list, object{"id": fmt.Sprintf("id%d", i)})
			}
			return object{"state": "s1", "list": list, "notFound": []any{}}
		}
		var result object
		switch method {
		case "Mailbox/query":
			result = query(s.mailboxes)
		case "Email/query":
			result = query(s.messages)
		case "SieveScript/query":
			result = query(s.scripts)
		case "CalendarEvent/query":
			result = query(len(s.objects))
		case "Calendar/changes", "Identity/changes", "CalendarEvent/changes":
			result = object{"oldState": "n", "newState": "s1", "hasMoreChanges": false, "created": []any{}, "updated": []any{}, "destroyed": []any{}}
		case "Calendar/get":
			result = listed(s.calendars)
		case "Identity/get":
			result = listed(s.identities)
		case "CalendarEvent/get":
			list := []any{}
			for _, id := range args["ids"].([]any) {
				var index int
				_, _ = fmt.Sscanf(id.(string), "id%d", &index)
				list = append(list, object{"id": id, "@type": s.objects[index]})
			}
			result = object{"state": "q1", "list": list, "notFound": []any{}}
		case "VacationResponse/get":
			result = object{"state": "s1", "list": []any{}, "notFound": []any{"singleton"}}
			if s.vacation {
				result = object{"state": "s1", "list": []any{object{"id": "singleton"}}, "notFound": []any{}}
			}
		default:
			t.Errorf("census called %s", method)
		}
		result["accountId"] = "company-account"
		if s.mutate != nil {
			s.mutate(method, args, result)
		}
		encoded, _ := json.Marshal(object{"methodResponses": []any{[]any{method, result, "proof"}}})
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(server.Close)
	client, err := newMailboxClient(server.URL, "fixture-only")
	if err != nil {
		t.Fatal(err)
	}
	client.account = "company-account"
	return client
}

func TestCensusLeavesUninitializedCollectionsUntouched(t *testing.T) {
	store := censusFixture()
	store.mutate = func(method string, _, result object) {
		if strings.HasSuffix(method, "/changes") {
			result["newState"] = "n"
		}
	}
	census, err := store.serve(t).census(context.Background())
	if err != nil || census.calendars != 0 || census.events != 0 || census.identities != 0 {
		t.Fatalf("an untouched collection did not remain empty: %v", err)
	}
	for _, method := range store.methods {
		switch method {
		case "Calendar/get", "CalendarEvent/get", "CalendarEvent/query", "Identity/get":
			t.Fatalf("census could initialize an empty collection through %s", method)
		}
	}
}

func TestCensusRechecksAnEmptyCalendarEventQuery(t *testing.T) {
	store := censusFixture()
	store.objects = nil
	queries := 0
	store.mutate = func(method string, _, result object) {
		if method != "CalendarEvent/query" {
			return
		}
		queries++
		if queries > 1 {
			result["queryState"] = "q2"
		}
	}
	if _, err := store.serve(t).census(context.Background()); err == nil || queries != 2 {
		t.Fatal("the empty calendar event query was accepted after a state change")
	}
}

func censusFixture() *censusStore {
	objects := make([]string, 300)
	for i := range objects {
		objects[i] = "Event"
		if i%7 == 0 {
			objects[i] = "Task"
		}
	}
	return &censusStore{mailboxes: 9, messages: 1234, calendars: 2, identities: 1, scripts: 1, objects: objects, vacation: true}
}

func TestCensusCountsEveryCategoryByIdentityOnly(t *testing.T) {
	store := censusFixture()
	census, err := store.serve(t).census(context.Background())
	if err != nil {
		t.Fatalf("census failed: %v", err)
	}
	if census != (storeCensus{mailboxes: 9, messages: 1234, calendars: 2, events: 257, tasks: 43, identities: 1, sieveScripts: 1, vacation: true}) {
		t.Fatalf("census miscounted: %+v", census)
	}
	pages := 0
	for _, method := range store.methods {
		if strings.HasSuffix(method, "/set") || strings.HasSuffix(method, "/import") {
			t.Fatalf("census wrote through %s", method)
		}
		if method == "CalendarEvent/get" {
			pages++
		}
	}
	if pages != 2 {
		t.Fatalf("300 calendar objects were read in %d pages, want 2", pages)
	}
}

func TestCensusRefusesIncompleteOrUnknownAnswers(t *testing.T) {
	for name, mutate := range map[string]func(string, object, object){
		"unknown-calendar-type": func(method string, _, result object) {
			if method == "CalendarEvent/get" {
				items(result, "list")[0]["@type"] = "Group"
			}
		},
		"short-calendar-page": func(method string, _, result object) {
			if method == "CalendarEvent/get" {
				result["list"] = result["list"].([]any)[1:]
			}
		},
		"duplicate-calendar-object": func(method string, _, result object) {
			if method == "CalendarEvent/get" {
				list := result["list"].([]any)
				list[1] = list[0]
			}
		},
		"calendar-object-not-found": func(method string, _, result object) {
			if method == "CalendarEvent/get" {
				result["notFound"] = []any{"id0"}
			}
		},
		"calendar-last-get-state-changed": func(method string, args, result object) {
			if method == "CalendarEvent/get" && args["ids"].([]any)[0] == "id256" {
				result["state"] = "q2"
			}
		},
		"query-changed-between-pages": func(method string, args, result object) {
			if method == "CalendarEvent/query" && args["position"] != float64(0) {
				result["queryState"] = "q2"
			}
		},
		"total-changed-between-pages": func(method string, args, result object) {
			if method == "CalendarEvent/query" && args["position"] != float64(0) {
				result["total"] = float64(301)
			}
		},
		"missing-total":      func(_ string, _, result object) { delete(result, "total") },
		"fractional-total":   func(_ string, _, result object) { result["total"] = 1.5 },
		"negative-total":     func(_ string, _, result object) { result["total"] = float64(-1) },
		"missing-list-state": func(_ string, _, result object) { delete(result, "state") },
		"listed-not-found": func(method string, _, result object) {
			if method == "Identity/get" {
				result["notFound"] = []any{"id9"}
			}
		},
		"calendar-created-during-list": func(method string, _, result object) {
			if method == "Calendar/get" {
				result["state"] = "s2"
			}
		},
		"identity-created-during-list": func(method string, _, result object) {
			if method == "Identity/get" {
				result["state"] = "s2"
			}
		},
		"incomplete-identity-state": func(method string, _, result object) {
			if method == "Identity/changes" {
				result["hasMoreChanges"] = true
			}
		},
		"missing-vacation": func(method string, _, result object) {
			if method == "VacationResponse/get" {
				result["list"] = []any{}
			}
		},
		"foreign-account": func(method string, _, result object) {
			if method == "SieveScript/query" {
				result["accountId"] = "another-account"
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := censusFixture()
			store.mutate = mutate
			if _, err := store.serve(t).census(context.Background()); err == nil {
				t.Fatal("an incomplete or unknown census answer was accepted")
			}
		})
	}
}

func TestCensusVerdictsBindTheRestoreToTheSource(t *testing.T) {
	source := storeCensus{mailboxes: 9, messages: 1234, calendars: 2, events: 257, tasks: 43, identities: 1, sieveScripts: 1, vacation: true}
	verdicts, err := censusVerdicts(source, source, source)
	if err != nil {
		t.Fatalf("an equal restore was refused: %v", err)
	}
	for _, category := range []string{"messages", "mailboxes", "calendars", "tasks", "settings"} {
		if verdicts[category] != "VERIFIED" {
			t.Fatalf("%s was not verified", category)
		}
	}
	empty := source
	empty.tasks = 0
	if verdicts, err = censusVerdicts(empty, empty, empty); err != nil || verdicts["tasks"] != "EMPTY" {
		t.Fatal("an account without tasks was not reported as empty")
	}
	arrived := source
	arrived.messages += 2
	between := source
	between.messages++
	for _, restored := range []storeCensus{source, between, arrived} {
		if _, err = censusVerdicts(source, arrived, restored); err != nil {
			t.Fatal("a restore taken while mail arrived was refused")
		}
	}
	for name, change := range map[string]func(*storeCensus){
		"lost-message":    func(s *storeCensus) { s.messages-- },
		"extra-message":   func(s *storeCensus) { s.messages++ },
		"lost-mailbox":    func(s *storeCensus) { s.mailboxes-- },
		"lost-calendar":   func(s *storeCensus) { s.calendars-- },
		"lost-event":      func(s *storeCensus) { s.events-- },
		"lost-task":       func(s *storeCensus) { s.tasks-- },
		"lost-identity":   func(s *storeCensus) { s.identities-- },
		"lost-filter":     func(s *storeCensus) { s.sieveScripts-- },
		"lost-vacation":   func(s *storeCensus) { s.vacation = false },
		"event-as-a-task": func(s *storeCensus) { s.events--; s.tasks++ },
	} {
		t.Run(name, func(t *testing.T) {
			restored := source
			change(&restored)
			if _, err := censusVerdicts(source, source, restored); err == nil {
				t.Fatal("a restore that differs from an unchanged source was accepted")
			}
		})
	}
	if _, err = censusVerdicts(source, arrived, storeCensus{mailboxes: 9, messages: 1237, calendars: 2, events: 257, tasks: 43, identities: 1, sieveScripts: 1, vacation: true}); err == nil {
		t.Fatal("a restore outside the source's two readings was accepted")
	}
	noMarker := storeCensus{calendars: 1, identities: 1, vacation: true}
	if _, err = censusVerdicts(noMarker, noMarker, noMarker); err == nil {
		t.Fatal("a store without the diagnostic message and mailbox was verified")
	}
}

func TestReceiptRequiresTheRestoredCensus(t *testing.T) {
	if _, err := recoveryReceipt(nil); err == nil {
		t.Fatal("a pass receipt was issued without the restored census")
	}
	verdicts := map[string]string{"messages": "VERIFIED", "mailboxes": "VERIFIED", "calendars": "VERIFIED", "tasks": "EMPTY", "settings": "VERIFIED"}
	receipt, err := recoveryReceipt(verdicts)
	if err != nil || receipt != "EMAIL_DATA_RECOVERY=PASS messages=VERIFIED mailboxes=VERIFIED calendars=VERIFIED tasks=EMPTY settings=VERIFIED storage=ENCRYPTED source=RETAINED restore_cleanup=VERIFIED backup=RETAINED_24H" {
		t.Fatalf("unexpected receipt %q: %v", receipt, err)
	}
	for _, change := range []struct{ category, verdict string }{{"messages", "EMPTY"}, {"mailboxes", "EMPTY"}, {"settings", "EMPTY"}, {"calendars", "1234"}, {"tasks", ""}, {"extra", "VERIFIED"}} {
		changed := map[string]string{}
		for k, v := range verdicts {
			changed[k] = v
		}
		changed[change.category] = change.verdict
		if _, err := recoveryReceipt(changed); err == nil {
			t.Fatalf("receipt accepted %s=%q", change.category, change.verdict)
		}
	}
}
