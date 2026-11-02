package main

import (
	"context"
	"testing"
	"time"
)

func TestReviewedPolicyLoadsTheActualConsumerConfiguration(t *testing.T) {
	t.Chdir("../..")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := reviewedPolicy(ctx); err != nil {
		t.Fatal("the committed consumer configuration cannot establish its read-only repository policy")
	}
}
