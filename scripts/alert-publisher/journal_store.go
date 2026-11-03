package main

import "context"

// A session binds one invocation's reads and conditional writes to its own
// revision. Sharing a backend must never share an invocation's expected ETag.
type journalStore interface {
	Open(context.Context) (journalSession, error)
}

type journalSession interface {
	Read(context.Context) (journal, error)
	Save(context.Context, journal) error
	Close()
}

type localJournalStore struct{ dir string }

type localJournalSession struct {
	dir    string
	unlock func()
}

func (s localJournalStore) Open(ctx context.Context) (journalSession, error) {
	unlock, err := lockStore(ctx, s.dir)
	if err != nil {
		return nil, err
	}
	return &localJournalSession{dir: s.dir, unlock: unlock}, nil
}

func (s *localJournalSession) Read(context.Context) (journal, error)   { return readJournal(s.dir) }
func (s *localJournalSession) Save(_ context.Context, v journal) error { return saveJournal(s.dir, v) }
func (s *localJournalSession) Close()                                  { s.unlock() }
