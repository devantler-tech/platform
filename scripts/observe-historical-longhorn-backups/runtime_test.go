package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestReviewedPolicyUsesActualProductionVariableLayers(t *testing.T) {
	t.Chdir("../..")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	policy, err := reviewedPolicy(ctx)
	if err != nil {
		t.Fatal("reviewed source variables could not be bound")
	}
	if policy.Prefix != "velero/prod/" || policy.Bucket == "" || !strings.HasSuffix(policy.Endpoint, ".r2.cloudflarestorage.com") || policy.MaxRequests != 4096 || policy.MaxBytes != 512<<20 {
		t.Fatal("wrong source scope or observation bounds")
	}
}

func TestCommandOutputCannotBypassBoundThroughReaderFrom(t *testing.T) {
	output := boundedOutput{limit: 8}
	_, err := io.Copy(&output, io.LimitReader(strings.NewReader(strings.Repeat("x", 16)), 16))
	if err != errHeld || !output.held || output.Len() > 8 {
		t.Fatal("stream copy bypassed command output bound")
	}
}

func TestDocumentReadDiscardsEveryPartialResponse(t *testing.T) {
	for failAt := 0; failAt < 4; failAt++ {
		calls := 0
		read := func(context.Context, string, ...string) ([]byte, error) {
			index := calls
			calls++
			if index == failAt {
				return []byte("partial private response"), errHeld
			}
			return []byte("fixture"), nil
		}
		docs, err := readDocuments(context.Background(), read, "wedding-app")
		if err != errHeld || len(docs.Location)+len(docs.Repository)+len(docs.CloudSecret)+len(docs.RepositorySecret) != 0 || calls != failAt+1 {
			t.Fatal("partial private data escaped")
		}
	}
}
