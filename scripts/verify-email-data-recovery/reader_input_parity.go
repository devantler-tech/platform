package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"
)

// runReaderInputParity proves the fixed original input record still equals fresh native inputs.
// It has no create, update, stop or disposal path and cannot claim independent recovery.
func runReaderInputParity(ctx context.Context, env func(string) string, k kubernetes, verifyMain func(context.Context, string) error, loadStore func(func(string) string) (*readerCustodyStore, error), output io.Writer) int {
	stage := "dispatch"
	fail := func() int {
		if output != nil {
			_, _ = fmt.Fprintf(output, "EMAIL_READER_INPUT_PARITY=UNVERIFIED stage=%s restart=NOT_AUTHORIZED recovery=UNVERIFIED\n", stage)
		}
		return 2
	}
	if output == nil || verifyMain == nil || loadStore == nil || authorizeReaderInputOperation(env, "verify-retained-email-reader-inputs.yaml", "verify-retained-email-reader-inputs") != nil {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	stage = "source"
	if verifyMain(ctx, env("GITHUB_SHA")) != nil {
		return fail()
	}
	d := &recoveryDriver{i: retainedRecovery, k: readerInputKubernetes{inner: k}}
	var err error
	d.source, err = d.readSource(ctx)
	if err != nil {
		return fail()
	}
	stage = "inputs"
	reader, claim := env("EMAIL_RETAINED_READER_UID"), env("EMAIL_RETAINED_CLAIM_UID")
	original, err := d.captureCustodyInputs(ctx, reader, claim)
	if err != nil {
		return fail()
	}
	expected, err := readerCustodyDocument(original)
	if err != nil {
		return fail()
	}
	stage = "credentials"
	store, err := loadStore(env)
	if err != nil || store == nil || store.client == nil || store.originalRecord == nil {
		return fail()
	}
	defer store.client.CloseIdleConnections()
	verify := func() error {
		if verifyMain(ctx, env("GITHUB_SHA")) != nil {
			return errors.New("parity-source")
		}
		fresh, err := d.captureCustodyInputs(ctx, reader, claim)
		if err != nil || !reflect.DeepEqual(original, fresh) {
			return errors.New("parity-inputs")
		}
		return nil
	}
	stage = "parity"
	if verify() != nil {
		return fail()
	}
	found, err := store.matchesOriginalInputs(ctx, expected)
	if err != nil || !found || verify() != nil {
		return fail()
	}
	if _, err := fmt.Fprintln(output, "EMAIL_READER_INPUT_PARITY=VERIFIED source_writes=0 restart=NOT_AUTHORIZED recovery=UNVERIFIED"); err != nil {
		return 2
	}
	return 0
}
