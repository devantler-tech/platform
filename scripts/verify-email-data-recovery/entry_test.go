package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type refusedAPI struct{ calls int }

func (k *refusedAPI) call(context.Context, []byte, ...string) ([]byte, error) {
	k.calls++
	return nil, errors.New("private-provider-response-do-not-print")
}
func TestUnconfirmedRecoveryCannotReadOrWriteTheCluster(t *testing.T) {
	env := confirmedInvocation()
	env["GITHUB_RUN_ATTEMPT"] = "2"
	k := &refusedAPI{}
	var output bytes.Buffer
	if runRecovery(context.Background(), func(s string) string { return env[s] }, k, &output) != 2 || k.calls != 0 || output.String() != "EMAIL_DATA_RECOVERY=UNVERIFIED stage=dispatch cleanup=NOT_STARTED\n" {
		t.Fatal("an unconfirmed recovery reached the cluster or produced an unsafe receipt")
	}
}
func TestConfirmedRecoveryStartsWithReadOnlyPreflightAndClosedFailure(t *testing.T) {
	env := confirmedInvocation()
	k := &refusedAPI{}
	var output bytes.Buffer
	if runRecovery(context.Background(), func(s string) string { return env[s] }, k, &output) != 2 || k.calls != 1 || output.String() != "EMAIL_DATA_RECOVERY=UNVERIFIED stage=preflight cleanup=NOT_STARTED\n" || strings.Contains(output.String(), "private-provider") {
		t.Fatal("confirmed recovery skipped preflight or exposed provider data")
	}
}
