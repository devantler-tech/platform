package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const nativeR2ProofKey = "proofs/alert-journal-native-v1.json"
const nativeR2ProofInstance = "alert-journal-native-proof"

// This opt-in test writes one retained proof object and never contacts GitHub.
// Existing objects are refused: restarting a failed proof is not recovery.
func nativeR2ProofStore(t *testing.T) *r2JournalStore {
	t.Helper()
	if os.Getenv("ALERT_R2_PROVIDER_PROOF") != "run-once" {
		t.Skip("actual R2 proof needs approved independent storage and private proof files")
	}
	facts, credential, err := loadR2Facts(os.Getenv("ALERT_R2_PROOF_CONFIG_FILE"), os.Getenv("ALERT_R2_PROOF_CREDENTIAL_FILE"))
	if err != nil || facts.Key != nativeR2ProofKey || facts.Instance != nativeR2ProofInstance {
		t.Fatal("native proof refused private inputs")
	}
	store, err := newR2JournalStore(facts, credential)
	if err != nil {
		t.Fatal("native proof refused provider settings")
	}
	return store
}

type nativeR2LostAcknowledgement struct {
	r2ObjectAPI
	writes int
}

func (c *nativeR2LostAcknowledgement) PutObject(ctx context.Context, input *s3.PutObjectInput, options ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	c.writes++
	result, err := c.r2ObjectAPI.PutObject(ctx, input, options...)
	if err != nil || result == nil {
		return nil, errStore
	}
	// The actual provider acknowledged a committed write; hide that acknowledgement
	// only from the caller, reproducing a lost response without a second write.
	return nil, errors.New("simulated lost acknowledgement")
}

func TestR2NativeProviderConflictLostResponseAndProcessExit(t *testing.T) {
	store := nativeR2ProofStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if store.Initialize(ctx) != nil {
		t.Fatal("native create-once proof failed; do not retry or replace the object")
	}
	if store.Initialize(ctx) == nil {
		t.Fatal("provider accepted a second create-once initialization")
	}
	read := func() (journalSession, journal) {
		t.Helper()
		session, err := store.Open(ctx)
		if err != nil {
			t.Fatal("native session open failed")
		}
		t.Cleanup(session.Close)
		state, err := session.Read(ctx)
		if err != nil {
			t.Fatal("native session read failed")
		}
		return session, state
	}
	winner, state := read()
	stale, _ := read()
	if winner.Save(ctx, state) != nil || stale.Save(ctx, state) == nil || stale.Save(ctx, state) == nil {
		t.Fatal("native conditional conflict or poisoned-session refusal failed")
	}
	uncertain, state := read()
	client := store.client
	lost := &nativeR2LostAcknowledgement{r2ObjectAPI: client}
	store.client = lost
	if uncertain.Save(ctx, state) == nil {
		t.Fatal("uncertain provider response was reported as acknowledged")
	}
	if uncertain.Save(ctx, state) == nil || lost.writes != 1 {
		t.Fatal("uncertain provider response authorized a retry")
	}
	store.client = client
	recovered, _ := read()
	if recovered.(*r2JournalSession).generation != 3 {
		t.Fatal("fresh session did not recover the provider-committed revision")
	}
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestR2NativeProviderCrashChild$")
	child.Env = []string{
		"ALERT_R2_PROVIDER_PROOF=run-once",
		"ALERT_R2_PROOF_CRASH_CHILD=1",
		"ALERT_R2_PROOF_CONFIG_FILE=" + os.Getenv("ALERT_R2_PROOF_CONFIG_FILE"),
		"ALERT_R2_PROOF_CREDENTIAL_FILE=" + os.Getenv("ALERT_R2_PROOF_CREDENTIAL_FILE"),
	}
	child.Stdout, child.Stderr = io.Discard, io.Discard
	var exited *exec.ExitError
	if err := child.Run(); !errors.As(err, &exited) || exited.ExitCode() != 23 {
		t.Fatal("native durable-boundary child did not exit as expected")
	}
	restarted, durable := read()
	if restarted.(*r2JournalSession).generation != 4 || len(durable.Pending) != 1 || durable.Pending[0].ID != "33333333333333333333333333333333" {
		t.Fatal("native process restart lost the persisted intent")
	}
	t.Log("R2_PROVIDER_STORAGE_PROOF=PASS conditional-conflict lost-response durable-intent")
}

func TestR2NativeProviderCrashChild(t *testing.T) {
	if os.Getenv("ALERT_R2_PROOF_CRASH_CHILD") != "1" {
		t.Skip("owned native proof child only")
	}
	store := nativeR2ProofStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := store.Open(ctx)
	if err != nil {
		t.Fatal("native child open failed")
	}
	if _, err = session.Read(ctx); err != nil {
		t.Fatal("native child read failed")
	}
	var envelope r2Envelope
	if json.Unmarshal([]byte(pendingR2Object), &envelope) != nil {
		t.Fatal("native intent fixture failed")
	}
	state, err := decodeJournal(envelope.Journal)
	if err != nil || session.Save(ctx, state) != nil {
		t.Fatal("native child persistence failed")
	}
	// Exit without Close or deferred cleanup at the durable intent boundary.
	os.Exit(23)
}
