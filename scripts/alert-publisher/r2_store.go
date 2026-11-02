package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/devantler-tech/platform/internal/strictjson"
	"golang.org/x/time/rate"
)

type r2Settings struct {
	AccountID string `json:"accountId"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Instance  string `json:"instance"`
}

type r2Credentials struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
}

type r2ObjectAPI interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type r2JournalStore struct {
	client r2ObjectAPI
	facts  r2Settings
	writes *rate.Limiter
}

type r2JournalSession struct {
	store      *r2JournalStore
	etag       string
	generation uint64
	nonce      string
	read       bool
	failed     bool
	closed     bool
}

const maxR2ObjectBytes = 132 * 1024

type r2Envelope struct {
	Version    int             `json:"version"`
	Instance   string          `json:"instance"`
	Generation uint64          `json:"generation"`
	Nonce      string          `json:"nonce"`
	Journal    json.RawMessage `json:"journal"`
}

func newR2JournalStore(facts r2Settings, credential r2Credentials) (*r2JournalStore, error) {
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(facts.AccountID) || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`).MatchString(facts.Bucket) || !regexp.MustCompile(`^[a-z0-9][a-z0-9/_-]{0,190}\.json$`).MatchString(facts.Key) || strings.Contains(facts.Key, "//") || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`).MatchString(facts.Instance) {
		return nil, errStore
	}
	for _, value := range []string{credential.AccessKeyID, credential.SecretAccessKey} {
		if !regexp.MustCompile(`^[A-Za-z0-9/+=_-]{16,128}$`).MatchString(value) {
			return nil, errStore
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	client := s3.New(s3.Options{
		Region: "auto", BaseEndpoint: aws.String("https://" + facts.AccountID + ".r2.cloudflarestorage.com"), UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: credential.AccessKeyID, SecretAccessKey: credential.SecretAccessKey, Source: "explicit-private-file"}, nil
		}),
		HTTPClient: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: noRedirect},
		Retryer:    aws.NopRetryer{}, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &r2JournalStore{client: client, facts: facts, writes: rate.NewLimiter(rate.Every(time.Second), 1)}, nil
}

func (s *r2JournalStore) Open(ctx context.Context) (journalSession, error) {
	if ctx.Err() != nil {
		return nil, errStore
	}
	return &r2JournalSession{store: s}, nil
}

func (s *r2JournalStore) Initialize(ctx context.Context) error {
	nonce, err := opaqueID()
	if err != nil {
		return errStore
	}
	raw, err := json.Marshal(journal{Version: 1, Routes: map[string]routeState{}})
	if err != nil {
		return errStore
	}
	body, err := json.Marshal(r2Envelope{Version: 1, Instance: s.facts.Instance, Generation: 1, Nonce: nonce, Journal: raw})
	if err != nil || s.writes.Wait(ctx) != nil || ctx.Err() != nil {
		return errStore
	}
	result, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.facts.Bucket), Key: aws.String(s.facts.Key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))), ContentType: aws.String("application/json"), IfNoneMatch: aws.String("*")})
	if err != nil || result == nil || result.ETag == nil || !validR2ETag(*result.ETag) {
		return errStore
	}
	session, err := s.Open(ctx)
	if err != nil {
		return errStore
	}
	defer session.Close()
	if _, err := session.Read(ctx); err != nil {
		return errStore
	}
	readback := session.(*r2JournalSession)
	if readback.etag != *result.ETag || readback.generation != 1 || readback.nonce != nonce {
		return errStore
	}
	return nil
}

func (s *r2JournalSession) Read(ctx context.Context) (journal, error) {
	if s.closed || s.failed || s.read {
		return journal{}, errStore
	}
	s.failed = true
	result, err := s.store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.store.facts.Bucket), Key: aws.String(s.store.facts.Key)})
	if err != nil || result == nil || result.Body == nil {
		return journal{}, errStore
	}
	body, readErr := io.ReadAll(io.LimitReader(result.Body, maxR2ObjectBytes+1))
	closeErr := result.Body.Close()
	if readErr != nil || closeErr != nil || len(body) > maxR2ObjectBytes || result.ContentLength == nil || *result.ContentLength != int64(len(body)) || result.ETag == nil || !validR2ETag(*result.ETag) {
		return journal{}, errStore
	}
	var envelope r2Envelope
	if strictjson.Validate(body, r2CanonicalFields()) != nil || !exactJSONFields(body, "version", "instance", "generation", "nonce", "journal") || json.Unmarshal(body, &envelope) != nil || envelope.Version != 1 || envelope.Instance != s.store.facts.Instance || envelope.Generation == 0 || !validID(envelope.Nonce) || !exactJSONFields(envelope.Journal, "version", "routes", "pending", "receipts", "attemptDay", "attemptCount") {
		return journal{}, errStore
	}
	state, err := decodeJournal(envelope.Journal)
	if err != nil {
		return journal{}, errStore
	}
	s.etag = *result.ETag
	s.generation = envelope.Generation
	s.nonce = envelope.Nonce
	s.read = true
	s.failed = false
	return state, nil
}
func (s *r2JournalSession) Save(ctx context.Context, state journal) error {
	if s.closed || s.failed || !s.read || s.generation == math.MaxUint64 {
		return errStore
	}
	// An uncertain write poisons this invocation's revision. No retry or later
	// GitHub write may proceed on a revision whose outcome was not acknowledged.
	s.failed = true
	raw, err := json.Marshal(state)
	if err != nil {
		return errStore
	}
	if _, err := decodeJournal(raw); err != nil {
		return errStore
	}
	nonce, err := opaqueID()
	if err != nil {
		return errStore
	}
	body, err := json.Marshal(r2Envelope{Version: 1, Instance: s.store.facts.Instance, Generation: s.generation + 1, Nonce: nonce, Journal: raw})
	if err != nil || len(body) > maxR2ObjectBytes || s.store.writes.Wait(ctx) != nil || ctx.Err() != nil {
		return errStore
	}
	result, err := s.store.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.store.facts.Bucket), Key: aws.String(s.store.facts.Key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))), ContentType: aws.String("application/json"), IfMatch: aws.String(s.etag)})
	if err != nil || result == nil || result.ETag == nil || !validR2ETag(*result.ETag) || *result.ETag == s.etag {
		return errStore
	}
	s.etag = *result.ETag
	s.generation++
	s.nonce = nonce
	s.failed = false
	return nil
}
func (s *r2JournalSession) Close() { s.closed = true }

func validR2ETag(value string) bool { return regexp.MustCompile(`^"[0-9a-f]{32}"$`).MatchString(value) }

func exactJSONFields(raw []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

// Reuse the repository's evidence decoder, including canonical names at every
// typed journal path; encoding/json alone would accept case aliases.
func r2CanonicalFields() map[string][]string {
	fields := map[string][]string{
		"":                   {"version", "instance", "generation", "nonce", "journal"},
		"journal":            {"version", "routes", "pending", "receipts", "attemptDay", "attemptCount"},
		"journal.pending[]":  {"id", "episode", "route", "state", "createdAt", "issue", "sent", "attempts", "retryAt"},
		"journal.receipts[]": {"operation", "issue", "comment", "verifiedAt"},
	}
	for name := range routes {
		fields["journal.routes."+name] = []string{"observedAt", "state", "episode", "issue"}
	}
	return fields
}
