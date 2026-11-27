package main

import (
	"context"
	"errors"
	"fmt"
)

// storeCensus counts what one account's store holds, without reading any
// content: mailboxes, messages, calendars with their events and tasks, and the
// account settings (sending identities, Sieve filter scripts and the vacation
// response). It stays in process memory; receipts carry only closed verdicts.
type storeCensus struct {
	mailboxes, messages, calendars, events, tasks, identities, sieveScripts int
	vacation                                                                bool
}

// calendarPage stays well inside the server's 500-object read limit.
const calendarPage = 256

// censusBound refuses an implausible store rather than paging without end.
const censusBound = 1 << 20

func queryTotal(result object) (int, error) {
	total, ok := result["total"].(float64)
	ids, listed := result["ids"].([]any)
	if !ok || !listed || total < 0 || total > censusBound || total != float64(int(total)) || text(result, "queryState") == "" {
		return 0, errors.New("census-total")
	}
	if len(ids) > int(total) {
		return 0, errors.New("census-total")
	}
	return int(total), nil
}

func (c *mailboxClient) count(ctx context.Context, method string) (int, error) {
	result, err := c.call(ctx, method, object{"calculateTotal": true, "limit": 1})
	if err != nil {
		return 0, err
	}
	return queryTotal(result)
}

type collectionChanges struct {
	state       string
	liveChanges bool
}

func (c *mailboxClient) collectionChanges(ctx context.Context, collection string) (collectionChanges, error) {
	changes, err := c.call(ctx, collection+"/changes", object{"sinceState": "n"})
	if err != nil {
		return collectionChanges{}, err
	}
	result := collectionChanges{state: text(changes, "newState")}
	if text(changes, "oldState") != "n" || result.state == "" || changes["hasMoreChanges"] != false {
		return collectionChanges{}, fmt.Errorf("census %s: incomplete change state", collection)
	}
	for _, field := range []string{"created", "updated", "destroyed"} {
		rows, ok := changes[field].([]any)
		if !ok || (result.state == "n" && len(rows) != 0) {
			return collectionChanges{}, fmt.Errorf("census %s: inconsistent changes", collection)
		}
		if field != "destroyed" && len(rows) != 0 {
			result.liveChanges = true
		}
	}
	return result, nil
}

// listAll avoids initializing an untouched collection. An initialized
// collection must retain its state while listed, so regenerated entries
// cannot pass as restored data.
func (c *mailboxClient) listAll(ctx context.Context, collection string) (int, error) {
	changes, err := c.collectionChanges(ctx, collection)
	if err != nil || changes.state == "n" {
		return 0, err
	}
	result, err := c.call(ctx, collection+"/get", object{"ids": nil, "properties": []any{"id"}})
	if err != nil {
		return 0, err
	}
	list, ok := result["list"].([]any)
	if text(result, "state") != changes.state {
		return 0, fmt.Errorf("census %s: collection changed during listing", collection)
	}
	if !ok || len(items(result, "list")) != len(list) || !noneMissing(result) {
		return 0, errors.New("census-list")
	}
	return len(list), nil
}

// noneMissing requires a /get answer's notFound list to be present and empty.
// It holds identifiers, not objects, so it is read as a raw list.
func noneMissing(result object) bool {
	missing, ok := result["notFound"].([]any)
	return ok && len(missing) == 0
}

// calendarObjects pages through every calendar object by identity and type.
// Stalwart serves an event as JSCalendar Event and a task as Task; any other
// type, a changed query or a short page refuses the census.
func (c *mailboxClient) calendarObjects(ctx context.Context) (events, tasks int, err error) {
	total, state := -1, ""
	for position := 0; ; position += calendarPage {
		page, err := c.call(ctx, "CalendarEvent/query", object{"calculateTotal": true, "position": position, "limit": calendarPage})
		if err != nil {
			return 0, 0, err
		}
		pageTotal, err := queryTotal(page)
		if err != nil || (total >= 0 && (pageTotal != total || text(page, "queryState") != state)) || page["position"] != float64(position) {
			return 0, 0, errors.New("census-calendar-query")
		}
		total, state = pageTotal, text(page, "queryState")
		ids, _ := page["ids"].([]any)
		if len(ids) == 0 {
			if total != 0 {
				return 0, 0, errors.New("census-calendar-total")
			}
			confirm, err := c.call(ctx, "CalendarEvent/query", object{"calculateTotal": true, "position": 0, "limit": 1})
			if err != nil {
				return 0, 0, err
			}
			if count, err := queryTotal(confirm); err != nil || count != 0 || text(confirm, "queryState") != state || confirm["position"] != float64(0) {
				return 0, 0, errors.New("census-calendar-query")
			}
			break
		}
		objects, err := c.call(ctx, "CalendarEvent/get", object{"ids": ids, "properties": []any{"id", "@type"}})
		if err != nil {
			return 0, 0, err
		}
		list := items(objects, "list")
		// The pinned Stalwart release reports the same calendar item change
		// state in queryState and get state.
		if raw, _ := objects["list"].([]any); text(objects, "state") != state || len(list) != len(ids) || len(raw) != len(ids) || !noneMissing(objects) {
			return 0, 0, errors.New("census-calendar-objects")
		}
		requested := map[any]bool{}
		for _, id := range ids {
			requested[id] = true
		}
		for _, o := range list {
			if id := text(o, "id"); !requested[id] {
				return 0, 0, errors.New("census-calendar-objects")
			}
			delete(requested, text(o, "id"))
			switch text(o, "@type") {
			case "Event":
				events++
			case "Task":
				tasks++
			default:
				return 0, 0, errors.New("census-calendar-type")
			}
		}
		if position+len(ids) >= total {
			break
		}
	}
	if events+tasks != total {
		return 0, 0, errors.New("census-calendar-total")
	}
	return events, tasks, nil
}

// census reads the account's store census, refusing a calendar or identity
// collection that was changed while being counted.
func (c *mailboxClient) census(ctx context.Context) (storeCensus, error) {
	var s storeCensus
	var err error
	if s.mailboxes, err = c.count(ctx, "Mailbox/query"); err != nil {
		return s, err
	}
	if s.messages, err = c.count(ctx, "Email/query"); err != nil {
		return s, err
	}
	if s.calendars, err = c.listAll(ctx, "Calendar"); err != nil {
		return s, err
	}
	if s.calendars > 0 {
		if s.events, s.tasks, err = c.calendarObjects(ctx); err != nil {
			return s, err
		}
	} else {
		// Calendar/get established that no calendar exists. A calendar event
		// query can create a default calendar when the server rebuilds its
		// cache. Use the owner's change feed here to reject surviving events
		// or tasks while allowing a collection whose last objects were deleted.
		changes, err := c.collectionChanges(ctx, "CalendarEvent")
		if err != nil || changes.liveChanges {
			return s, errors.New("census-calendar-objects-without-calendar")
		}
	}
	if s.identities, err = c.listAll(ctx, "Identity"); err != nil {
		return s, err
	}
	if s.sieveScripts, err = c.count(ctx, "SieveScript/query"); err != nil {
		return s, err
	}
	result, err := c.call(ctx, "VacationResponse/get", object{"ids": []any{"singleton"}, "properties": []any{"id"}})
	if err != nil {
		return s, err
	}
	list := items(result, "list")
	s.vacation = len(list) == 1 && text(list[0], "id") == "singleton"
	if !s.vacation {
		return s, errors.New("census-vacation")
	}
	return s, nil
}

// censusVerdicts compares the restored census with the source's, read before
// and after the backup. Unchanged sources require equal counts; a source that
// changed during the backup bounds each restored count by its two readings.
// Each verdict is VERIFIED, or EMPTY where the source held nothing to restore.
func censusVerdicts(before, after, restored storeCensus) (map[string]string, error) {
	within := func(field func(storeCensus) int) bool {
		low, high := field(before), field(after)
		if low > high {
			low, high = high, low
		}
		return field(restored) >= low && field(restored) <= high
	}
	verdict := func(present bool, fields ...func(storeCensus) int) (string, bool) {
		for _, field := range fields {
			if !within(field) {
				return "", false
			}
		}
		if present {
			return "VERIFIED", true
		}
		return "EMPTY", true
	}
	held := func(fields ...func(storeCensus) int) bool {
		for _, field := range fields {
			if field(before) > 0 || field(after) > 0 {
				return true
			}
		}
		return false
	}
	mailboxes := func(s storeCensus) int { return s.mailboxes }
	messages := func(s storeCensus) int { return s.messages }
	calendars := func(s storeCensus) int { return s.calendars }
	events := func(s storeCensus) int { return s.events }
	tasks := func(s storeCensus) int { return s.tasks }
	identities := func(s storeCensus) int { return s.identities }
	scripts := func(s storeCensus) int { return s.sieveScripts }
	verdicts := map[string]string{}
	for name, check := range map[string][]func(storeCensus) int{
		"mailboxes": {mailboxes},
		"messages":  {messages},
		"calendars": {calendars, events},
		"tasks":     {tasks},
		"settings":  {identities, scripts},
	} {
		result, ok := verdict(held(check...), check...)
		if !ok {
			return nil, errors.New("census-" + name)
		}
		verdicts[name] = result
	}
	// The marker message and its mailbox were created before the backup.
	if verdicts["mailboxes"] != "VERIFIED" || verdicts["messages"] != "VERIFIED" {
		return nil, errors.New("census-marker")
	}
	// Account settings always include the vacation response; the restored
	// account also authenticated with the source credential to be read.
	if !before.vacation || !after.vacation || !restored.vacation {
		return nil, errors.New("census-settings")
	}
	verdicts["settings"] = "VERIFIED"
	return verdicts, nil
}
