package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

type readerRestartIDs struct {
	Volume   retainedVolumeWitness
	Backend  string
	Replicas []string
}

// authorizeReaderRestartWitness binds seven independent original-copy identities to one preparation workflow.
func authorizeReaderRestartWitness(env func(string) string) (readerRestartIDs, error) {
	fail := func() (readerRestartIDs, error) { return readerRestartIDs{}, errors.New("reader-restart-dispatch") }
	confirmation := func(key string) string {
		if key == "EMAIL_READER_INPUTS_CONFIRM" {
			return env("EMAIL_READER_WITNESS_CONFIRM")
		}
		return env(key)
	}
	if authorizeReaderInputOperation(confirmation, "prepare-retained-email-reader-witness.yaml", "prepare-retained-email-reader-witness") != nil {
		return fail()
	}
	ids := readerRestartIDs{Volume: retainedVolumeWitness{env("EMAIL_RETAINED_READER_UID"), env("EMAIL_RETAINED_CLAIM_UID"), env("EMAIL_RETAINED_PV_UID")}, Backend: env("EMAIL_RETAINED_BACKEND_UID")}
	raw := []byte(env("EMAIL_RETAINED_REPLICA_UIDS"))
	if !validRetainedVolumeWitness(ids.Volume) || len(raw) == 0 || len(raw) > 256 || strictjson.Validate(raw, nil) != nil || json.Unmarshal(raw, &ids.Replicas) != nil || len(ids.Replicas) != 3 {
		return fail()
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	seen := map[string]bool{ids.Volume.ReaderUID: true, ids.Volume.ClaimUID: true, ids.Volume.PVUID: true}
	for _, uid := range append([]string{ids.Backend}, ids.Replicas...) {
		if !uuid.MatchString(uid) || seen[uid] {
			return fail()
		}
		seen[uid] = true
	}
	slices.Sort(ids.Replicas)
	return ids, nil
}

// runReaderRestartWitness prepares metadata custody only; no reader or storage mutation is authorized.
func runReaderRestartWitness(ctx context.Context, env func(string) string, k kubernetes, verifyMain func(context.Context, string) error, loadStore func(func(string) string) (*readerCustodyStore, error), output io.Writer, store bool) int {
	stage := "dispatch"
	fail := func() int {
		if output != nil {
			_, _ = fmt.Fprintf(output, "EMAIL_READER_RESTART_WITNESS=UNVERIFIED stage=%s restart=NOT_AUTHORIZED recovery=UNVERIFIED\n", stage)
		}
		return 2
	}
	ids, err := authorizeReaderRestartWitness(env)
	if output == nil || verifyMain == nil || loadStore == nil || k == nil || err != nil {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	stage = "source"
	if verifyMain(ctx, env("GITHUB_SHA")) != nil {
		return fail()
	}
	stage = "credentials"
	inputs, err := loadStore(env)
	if err != nil || inputs == nil || inputs.client == nil || inputs.originalRecord == nil {
		return fail()
	}
	defer inputs.client.CloseIdleConnections()
	stage = "graph"
	original, err := captureReaderRestartDocument(ctx, env, k, ids, inputs)
	if err != nil {
		return fail()
	}
	verify := func() error {
		if verifyMain(ctx, env("GITHUB_SHA")) != nil {
			return errors.New("reader-restart-source")
		}
		fresh, err := captureReaderRestartDocument(ctx, env, k, ids, inputs)
		if err != nil || !reflect.DeepEqual(original, fresh) {
			return errors.New("reader-restart-changed")
		}
		return nil
	}
	if verify() != nil {
		return fail()
	}
	stage = "private-handoff"
	if store {
		marker, err := readOwnedProofFile(env("EMAIL_READER_INPUTS_WORK_DIR"), "restore-result", 128)
		if err != nil || string(marker) != "OPENBAO_BACKUP_RESTORE=PASS records=1 version=2.6.3\n" {
			return fail()
		}
		before, err := readRestartWitnessFile(env("EMAIL_READER_INPUTS_WORK_DIR"))
		if err != nil || !reflect.DeepEqual(before, original) || verify() != nil {
			return fail()
		}
		stage = "custody"
		digest, err := inputs.createRestartWitness(ctx, before, verify)
		if err != nil {
			return fail()
		}
		if _, err := fmt.Fprintf(output, "EMAIL_READER_RESTART_WITNESS=STORED record_sha256=%s source_writes=0 key=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED\n", digest); err != nil {
			return 2
		}
		return 0
	}
	if writeRestartWitnessFile(env("EMAIL_READER_INPUTS_WORK_DIR"), original) != nil || verify() != nil {
		return fail()
	}
	if _, err := fmt.Fprintln(output, "EMAIL_READER_RESTART_WITNESS=PREFLIGHT source_writes=0 key=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED"); err != nil {
		return 2
	}
	return 0
}
