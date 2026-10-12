package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A transport repair on a new deployment must recover the terminal old journal
// before publishing unchanged placement, without starting another maintenance.
func TestLegacyPublicationRestoresOnlyTerminalUnpublishedOriginalJournal(t *testing.T) {
	for _, state := range []string{"terminal-paused", "terminal-released", "running", "unknown", "missing-liveness", "published", "changed-spec", "foreign-owner", "replaced-storage", "resized-storage", "journal-conflict", "unknown-phase"} {
		t.Run(state, func(t *testing.T) {
			a := fakeFixture()
			e := testEngine(a)
			ctx := context.Background()
			if err := e.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			j, o, err := e.readJournal(ctx)
			if err != nil {
				t.Fatal(err)
			}
			j.Phase, j.PublicationNotStarted = "prepared", true
			if err = e.writeJournal(ctx, j, o); err != nil {
				t.Fatal(err)
			}
			if state == "terminal-released" {
				if err = e.restore(ctx); err != nil {
					t.Fatal(err)
				}
				j, _, err = e.readJournal(ctx)
				if err != nil {
					t.Fatal(err)
				}
				j.Phase = "prepared" // Only the restoration receipt was lost.
			}
			if state == "published" {
				j.PublicationNotStarted = false
			}
			if state == "unknown-phase" {
				j.Phase = "unknown"
			}
			_, o, err = e.readJournal(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.writeJournal(ctx, j, o); err != nil {
				t.Fatal(err)
			}
			oldHolder, oldSource := j.Holder, j.Source
			e.holder, e.source = "101.1", strings.Repeat("b", 40)
			proofs := 0
			e.previousFinished = func(_ context.Context, previous journal) error {
				proofs++
				if previous.Holder != oldHolder || previous.Source != oldSource {
					t.Fatal("liveness proof not bound to original writer and source")
				}
				if state == "running" || state == "unknown" {
					return errors.New(state)
				}
				return nil
			}
			if state == "missing-liveness" {
				e.previousFinished = nil
			}
			e.revisionSafe = func(context.Context) error {
				t.Fatal("legacy restoration started a new selector maintenance")
				return errors.New("unexpected maintenance")
			}
			switch state {
			case "changed-spec":
				fakeSet(a.objects[fakeKey("deployment", "crossview", "crossview-postgres")][0], "/spec/template", object{"drift": true}, false)
			case "foreign-owner":
				fakeSet(a.objects[fakeKey("deployment", "crossview", "crossview-postgres")][0], "/metadata/annotations/"+escape(ownerKey), "foreign", false)
			case "replaced-storage":
				fakeSet(a.objects[fakeKey("persistentvolumeclaims", "*", "")][0], "/metadata/uid", "replacement", false)
			case "resized-storage":
				fakeSet(a.objects[fakeKey("persistentvolumeclaims", "*", "")][0], "/spec/resources/requests/storage", "2Gi", false)
			case "journal-conflict":
				e.c.command = func(c context.Context, input []byte, args ...string) ([]byte, error) {
					if args[0] == "patch" && args[1] == "configmap" {
						return nil, errPatchConflict
					}
					return a.command(c, input, args...)
				}
			}
			writes := a.writes
			err = e.legacyPublicationSafe(ctx)
			allowed := state == "terminal-paused" || state == "terminal-released"
			if (err == nil) != allowed {
				t.Fatalf("legacy handoff verdict for %s: %v", state, err)
			}
			if !allowed {
				if a.writes != writes {
					t.Fatal("unproven legacy handoff mutated resources")
				}
				return
			}
			current, _, readErr := e.readJournal(ctx)
			if readErr != nil || current.Holder != e.holder || current.Source != e.source || current.ResourceHolder != oldHolder || current.Phase != "restored" || !current.PublicationNotStarted || proofs != 1 {
				t.Fatalf("terminal handoff lost its restoration evidence: %+v err=%v", current, readErr)
			}
			if err = e.provenReleased(ctx, current); err != nil || a.migrated {
				t.Fatalf("original consumers were not restored without migration: %v", err)
			}
		})
	}
}
