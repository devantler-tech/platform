package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeGitHub struct {
	mu             sync.Mutex
	issues         []remoteIssue
	comments       []remoteComment
	writes         int
	fail           error
	commitThenFail bool
	wrongActor     bool
	closed         bool
	onVerify       func()
}

func (f *fakeGitHub) verify(context.Context) error {
	if f.onVerify != nil {
		f.onVerify()
	}
	return nil
}
func (f *fakeGitHub) find(_ context.Context, op operation) (receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result receipt
	for _, i := range f.issues {
		if strings.Contains(i.Body, op.marker()) {
			if result.Issue != 0 {
				return receipt{}, errQuarantined
			}
			if f.wrongActor {
				return receipt{}, errQuarantined
			}
			result = receipt{Issue: i.Number}
		}
	}
	for _, c := range f.comments {
		if strings.Contains(c.Body, op.marker()) {
			if result.Issue != 0 {
				return receipt{}, errQuarantined
			}
			if f.wrongActor {
				return receipt{}, errQuarantined
			}
			result = receipt{Issue: c.Issue, Comment: c.ID}
		}
	}
	return result, nil
}
func (f *fakeGitHub) isOpen(context.Context, int) (bool, error) { return !f.closed, nil }
func (f *fakeGitHub) write(_ context.Context, op operation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.fail != nil && !f.commitThenFail {
		return f.fail
	}
	if op.Issue == 0 {
		f.issues = append(f.issues, remoteIssue{Number: len(f.issues) + 1, Body: op.body()})
	} else {
		f.comments = append(f.comments, remoteComment{Issue: op.Issue, ID: len(f.comments) + 1, Body: op.body()})
	}
	return f.fail
}

var fixtureTime = time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)

func eventAt(state string, minute int) observation {
	return observation{Route: "cnpg-degraded", State: state, ObservedAt: fixtureTime.Add(time.Duration(minute) * time.Minute), Complete: state == "healthy"}
}
func engine(t *testing.T, dir string, api *fakeGitHub) *publisher {
	t.Helper()
	return &publisher{dir: filepath.Join(dir, "state"), api: api, now: func() time.Time { return fixtureTime.Add(30 * time.Minute) }}
}

func TestRepeatedConcurrentObservationsShareOneIncident(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := engine(t, dir, api).run(context.Background(), ptr(eventAt("firing", 0))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if api.writes != 1 || len(api.issues) != 1 {
		t.Fatalf("duplicate incident: writes=%d issues=%d", api.writes, len(api.issues))
	}
	if !strings.Contains(api.issues[0].Body, "@devantler") {
		t.Fatal("initial actionable notification must mention maintainer")
	}
}
func ptr[T any](v T) *T { return &v }

func TestRecoveryAndRecurrencePreserveOpenRemediation(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{}
	p := engine(t, dir, api)
	for _, e := range []observation{eventAt("healthy", 0), eventAt("firing", 1), eventAt("healthy", 2), eventAt("firing", 3), eventAt("firing", 4)} {
		if err := p.run(context.Background(), &e); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.issues) != 1 || len(api.comments) != 2 {
		t.Fatalf("wrong lifecycle: issues=%d comments=%d", len(api.issues), len(api.comments))
	}
	for _, c := range api.comments {
		if strings.Contains(c.Body, "@devantler") {
			t.Fatal("transition repeats notification mention")
		}
	}
}
func TestUnknownAndIncompleteReadsCannotBecomeRecovery(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{}
	p := engine(t, dir, api)
	if err := p.run(context.Background(), ptr(eventAt("firing", 0))); err != nil {
		t.Fatal(err)
	}
	e := eventAt("healthy", 1)
	e.Complete = false
	if err := p.run(context.Background(), &e); !errors.Is(err, errInput) {
		t.Fatalf("incomplete health accepted: %v", err)
	}
	if err := p.run(context.Background(), ptr(eventAt("unknown", 2))); err != nil {
		t.Fatal(err)
	}
	if len(api.issues) != 1 || len(api.comments) != 1 || !strings.Contains(api.comments[0].Body, "unconfirmed") {
		t.Fatal("lost signal was treated as healthy")
	}
}
func TestStaleObservationsDoNotUndoNewerState(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{}
	p := engine(t, dir, api)
	for _, e := range []observation{eventAt("firing", 2), eventAt("healthy", 1), eventAt("firing", 0)} {
		if err := p.run(context.Background(), &e); err != nil {
			t.Fatal(err)
		}
	}
	if api.writes != 1 {
		t.Fatal("stale event wrote a transition")
	}
}
func TestAmbiguousCommittedWriteReconcilesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{fail: errAmbiguous, commitThenFail: true}
	if err := engine(t, dir, api).run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errAmbiguous) {
		t.Fatal(err)
	}
	api.fail = nil
	if err := engine(t, dir, api).run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if api.writes != 1 {
		t.Fatal("ambiguous creation repeated")
	}
	s, err := readJournal(engine(t, dir, api).dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Receipts) != 1 || s.Receipts[0].Issue != 1 || len(s.Receipts[0].Operation) != 32 {
		t.Fatal("confirmed receipt did not survive restart")
	}
}
func TestUnprovenWriteStaysQuarantinedWithoutReplay(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{fail: errAmbiguous}
	if err := engine(t, dir, api).run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errAmbiguous) {
		t.Fatal(err)
	}
	api.fail = nil
	for range 3 {
		if err := engine(t, dir, api).run(context.Background(), nil); !errors.Is(err, errAmbiguous) {
			t.Fatal(err)
		}
	}
	if api.writes != 1 {
		t.Fatal("unproven write was repeated")
	}
}
func TestRefusedWriteRetriesDurablyButNeverRecordsFalseDelivery(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{fail: &retryError{after: fixtureTime.Add(time.Hour)}}
	p := engine(t, dir, api)
	if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.As(err, new(*retryError)) {
		t.Fatal(err)
	}
	api.fail = nil
	if err := p.run(context.Background(), nil); !errors.As(err, new(*retryError)) {
		t.Fatal("rate-limit deadline lost")
	}
	if api.writes != 1 {
		t.Fatal("retry happened before deadline")
	}
	p.now = func() time.Time { return fixtureTime.Add(2 * time.Hour) }
	if err := p.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if api.writes != 2 || len(api.issues) != 1 {
		t.Fatal("refused write was lost or duplicated")
	}
}
func TestWrongActorFailsClosed(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{wrongActor: true}
	if err := engine(t, dir, api).run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errQuarantined) {
		t.Fatal("wrong actor counted as delivery")
	}
	if api.writes != 1 {
		t.Fatal("unexpected write count")
	}
}

func TestClosedIncidentRecurrenceCreatesOneNewIssue(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{}
	p := engine(t, dir, api)
	for _, e := range []observation{eventAt("firing", 0), eventAt("healthy", 1)} {
		if err := p.run(context.Background(), &e); err != nil {
			t.Fatal(err)
		}
	}
	api.closed = true
	if err := p.run(context.Background(), ptr(eventAt("firing", 2))); err != nil {
		t.Fatal(err)
	}
	if len(api.issues) != 2 || len(api.comments) != 1 {
		t.Fatal("closed recurrence did not get a new incident")
	}
}

func TestClosedIssueWithNewConfirmedFiringStillNeedsTracking(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{}
	p := engine(t, dir, api)
	for _, e := range []observation{eventAt("firing", 0), eventAt("unknown", 1)} {
		if err := p.run(context.Background(), &e); err != nil {
			t.Fatal(err)
		}
	}
	api.closed = true
	if err := p.run(context.Background(), ptr(eventAt("firing", 2))); err != nil {
		t.Fatal(err)
	}
	if len(api.issues) != 2 || len(api.comments) != 1 {
		t.Fatal("a closed issue suppressed newly confirmed firing")
	}
}

func TestUTCWriteBudgetRefreshesAfterSlowPreflight(t *testing.T) {
	start := time.Date(2026, 10, 24, 23, 59, 50, 0, time.UTC)
	clock := start
	dir := filepath.Join(t.TempDir(), "state")
	unlock, err := lockStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	err = saveJournal(dir, journal{Version: 1, Routes: map[string]routeState{}, AttemptDay: "2026-10-24", AttemptCount: 11})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeGitHub{onVerify: func() { clock = start.Add(2 * time.Minute) }}
	p := publisher{dir: dir, api: api, now: func() time.Time { return clock }}
	e := observation{Route: "cnpg-degraded", State: "firing", ObservedAt: start.Add(-time.Minute)}
	if err = p.run(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
	s, err := readJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.AttemptDay != "2026-10-25" || s.AttemptCount != 1 || len(s.Receipts) != 1 || !s.Receipts[0].VerifiedAt.Equal(clock) {
		t.Fatal("publication used a stale UTC day or receipt time")
	}
}

func TestOutboxRejectsOverflowWithoutAcknowledgingObservation(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{fail: errAmbiguous}
	p := engine(t, dir, api)
	p.now = func() time.Time { return fixtureTime.Add(6 * time.Hour) }
	for i := range 64 {
		state := "firing"
		if i%2 == 1 {
			state = "healthy"
		}
		err := p.run(context.Background(), ptr(eventAt(state, i)))
		if !errors.Is(err, errAmbiguous) {
			t.Fatal(err)
		}
	}
	if err := p.run(context.Background(), ptr(eventAt("firing", 64))); !errors.Is(err, errFull) {
		t.Fatal("unbounded outbox", err)
	}
	s, err := readJournal(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Pending) != 64 || s.Routes["cnpg-degraded"].ObservedAt.Equal(eventAt("firing", 64).ObservedAt) {
		t.Fatal("rejected observation was acknowledged")
	}
}

func TestSixthSuccessfulAttemptStillReconciles(t *testing.T) {
	dir := t.TempDir()
	api := &fakeGitHub{fail: &retryError{after: fixtureTime}}
	p := engine(t, dir, api)
	for range 5 {
		if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.As(err, new(*retryError)) {
			t.Fatal(err)
		}
	}
	api.fail = nil
	if err := p.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if api.writes != 6 || len(api.issues) != 1 {
		t.Fatal("last permitted attempt was not delivered once")
	}
}
func TestInputCanariesNeverReachMessagesOrErrors(t *testing.T) {
	for _, raw := range []string{
		`{"route":"cnpg-degraded","state":"firing","observedAt":"2026-10-24T20:00:00Z","repository":"private-canary.example"}`,
		`{"route":"private-canary.example","state":"firing","observedAt":"2026-10-24T20:00:00Z"}`,
		`{"route":"cnpg-degraded","state":"private-canary.example","observedAt":"2026-10-24T20:00:00Z"}`,
	} {
		_, err := decodeObservation(strings.NewReader(raw))
		if err == nil || strings.Contains(err.Error(), "private-canary") {
			t.Fatal("unsafe input accepted or echoed")
		}
	}
	for _, state := range []string{"firing", "healthy", "unknown"} {
		op := operation{Route: "cnpg-degraded", State: state, ID: "0123456789abcdef0123456789abcdef", Episode: "fedcba9876543210fedcba9876543210"}
		if strings.Contains(op.body(), "private-canary") {
			t.Fatal("renderer leaks source data")
		}
		if !json.Valid(mustJSON(t, op)) {
			t.Fatal("operation is not serializable")
		}
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
