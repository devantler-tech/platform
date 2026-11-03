package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type unavailableJournal struct {
	closed bool
}

func (s *unavailableJournal) Open(context.Context) (journalSession, error) { return s, nil }
func (s *unavailableJournal) Read(context.Context) (journal, error)        { return journal{}, errStore }
func (s *unavailableJournal) Save(context.Context, journal) error {
	panic("unreadable journal was overwritten")
}
func (s *unavailableJournal) Close() { s.closed = true }

func TestUnavailableRemoteJournalCannotFallBackToALocalIncident(t *testing.T) {
	api := &fakeGitHub{}
	p := engine(t, t.TempDir(), api)
	remote := &unavailableJournal{}
	p.store = remote
	if err := p.run(context.Background(), ptr(eventAt("firing", 0))); !errors.Is(err, errStore) {
		t.Fatalf("unreadable remote state permitted publication: %v", err)
	}
	if api.writes != 0 || !remote.closed {
		t.Fatal("unavailable remote state did not stop publication and close the session")
	}
	if _, err := os.Stat(filepath.Join(p.dir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("remote failure created a fallback local journal")
	}
}

type delayedJournal struct {
	base      journalStore
	afterSave func(journal)
}

type delayedSession struct {
	journalSession
	afterSave func(journal)
}

func (s delayedJournal) Open(ctx context.Context) (journalSession, error) {
	session, err := s.base.Open(ctx)
	if err != nil {
		return nil, err
	}
	return delayedSession{journalSession: session, afterSave: s.afterSave}, nil
}
func (s delayedSession) Save(ctx context.Context, state journal) error {
	if err := s.journalSession.Save(ctx, state); err != nil {
		return err
	}
	s.afterSave(state)
	return nil
}

func TestPublicationRechecksBoundariesAfterDurableSendIntent(t *testing.T) {
	for _, kind := range []string{"midnight", "expired", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			api := &fakeGitHub{}
			p := engine(t, t.TempDir(), api)
			now := time.Date(2026, 10, 24, 23, 59, 59, 0, time.UTC)
			p.now = func() time.Time { return now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			shifted := false
			p.store = delayedJournal{base: localJournalStore{dir: p.dir}, afterSave: func(state journal) {
				if shifted || len(state.Pending) == 0 || !state.Pending[0].Sent {
					return
				}
				shifted = true
				switch kind {
				case "midnight":
					now = now.Add(2 * time.Second)
				case "expired":
					now = now.Add(7*24*time.Hour + time.Second)
				case "cancelled":
					cancel()
				}
			}}
			err := p.run(ctx, ptr(eventAt("firing", 0)))
			if err == nil || api.writes != 0 {
				t.Fatalf("publication crossed the %s boundary after storage returned: %v writes=%d", kind, err, api.writes)
			}
			state, readErr := readJournal(p.dir)
			if readErr != nil || len(state.Pending) != 1 {
				t.Fatal("publication boundary lost durable work", readErr)
			}
			if kind == "midnight" {
				var retry *retryError
				if !errors.As(err, &retry) || state.Pending[0].Sent || !state.Pending[0].RetryAt.After(now) {
					t.Fatal("day change was not durably deferred", err)
				}
				now = now.Add(2 * time.Minute)
				if err := p.run(context.Background(), nil); err != nil || api.writes != 1 {
					t.Fatal("deferred work could not resume once under its new day budget", err)
				}
				state, err = readJournal(p.dir)
				if err != nil || state.AttemptDay != "2026-10-25" || state.AttemptCount != 1 {
					t.Fatal("resumed publication was not charged to the current UTC day", err)
				}
			} else if !state.Pending[0].Sent {
				t.Fatal("late expiry or cancellation discarded the saved intent")
			}
		})
	}
}
