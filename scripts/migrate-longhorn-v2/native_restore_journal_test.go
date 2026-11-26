package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func nativeRestoreDispatch(state nativeRestoreState) map[string]string {
	env := dispatchFixture()
	env["GITHUB_SHA"], env["GITHUB_RUN_ID"] = state.Source, state.Run
	env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_CONFIRM"] = nativeRestoreOperation, nativeRestoreConfirmation
	env["LONGHORN_V2_NODE"], env["LONGHORN_V2_DATABASE"] = "", ""
	return env
}

func TestNativeRestoreWorkflowUsesTheConfirmedMainDispatch(t *testing.T) {
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	offered := 0
	for _, choice := range choices {
		if choice == nativeRestoreOperation {
			offered++
		}
	}
	prepare := valueObject(workflow, "jobs", "prepare")
	step := migrationDispatchStep(t, prepare, "Require confirmed first attempt on main")
	command := text(step, "run")
	caseBody := nativeRestoreOperation + `) [[ -z "${LONGHORN_V2_NODE}" && -z "${LONGHORN_V2_DATABASE}" && "${LONGHORN_V2_CONFIRM}" == ` +
		nativeRestoreConfirmation + " ]] ;;"
	if offered != 1 || !strings.Contains(strings.Join(strings.Fields(command), " "), caseBody) ||
		!strings.Contains(command, `"${DISPATCH_REF}" == refs/heads/main && "${GITHUB_RUN_ATTEMPT}" == 1`) || !migrationDispatchMustRun(step) {
		t.Fatal("native restore dispatch lost its exact first-attempt confirmation")
	}
}

func TestNativeRestoreWrongConfirmationCannotReachTheCluster(t *testing.T) {
	_, _, state := nativeRestoreStateFixture(t, "created")
	for key, value := range nativeRestoreDispatch(state) {
		t.Setenv(key, value)
	}
	t.Setenv("LONGHORN_V2_CONFIRM", nativeSeedConfirmation)
	c := client{context: "admin@prod", run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("an unconfirmed restore reached an external command")
		return nil, nil
	}}
	if err := runNativeRestore(t.Context(), c, state.Source, state.Run, poolConfigFixture()); err == nil {
		t.Fatal("an unrelated confirmation admitted the native restore workflow")
	}
}

func TestCreateNativeRestoreJournalRequiresTheOriginalSeedAndNeverAdoptsAWrite(t *testing.T) {
	for _, fault := range []string{"", "wrong context", "wrong grant", "prior journal", "replaced seed", "stale main", "lost create response", "replaced readback", "changed seed readback"} {
		t.Run(fault, func(t *testing.T) {
			record, seed, state := nativeRestoreStateFixture(t, "created")
			for key, value := range nativeRestoreDispatch(state) {
				t.Setenv(key, value)
			}
			if fault == "wrong grant" {
				t.Setenv("GITHUB_RUN_ATTEMPT", "2")
			}
			recordData, _ := json.Marshal(record)
			seedData, _ := json.Marshal(seed)
			var stored object
			if fault == "prior journal" {
				stored = object{"metadata": object{"name": nativeRestoreJournal, "namespace": "longhorn-system", "uid": "other-restore", "resourceVersion": "9"}}
			}
			creates, calls := 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				calls++
				if program == "git" {
					source := state.Source
					if fault == "stale main" {
						source = strings.Repeat("f", 40)
					}
					return []byte(source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 5 || args[0] != "--context=admin@prod" {
					t.Fatal("journal creation escaped the protected context", program, args)
				}
				if args[2] == "create" {
					creates++
					if args[3] != "--filename=/dev/stdin" || args[4] != "--output=json" || json.Unmarshal(input, &stored) != nil ||
						text(stored, "metadata", "name") != nativeRestoreJournal || text(stored, "metadata", "namespace") != "longhorn-system" {
						t.Fatal("journal creation targeted another object", args)
					}
					if decoded, err := decodeNativeRestoreState(text(stored, "data", "restore"), record, seed); err != nil || !reflect.DeepEqual(decoded, state) {
						t.Fatal("journal creation did not preserve the verified initial state", err)
					}
					valueObject(stored, "metadata")["uid"] = "original-restore-journal"
					valueObject(stored, "metadata")["resourceVersion"] = "10"
					if fault == "lost create response" {
						return nil, errors.New("response lost after create")
					}
					return json.Marshal(stored)
				}
				if args[2] != "get" || args[3] != "configmap" || input != nil {
					t.Fatal("journal initializer attempted an unexpected write", args)
				}
				switch args[4] {
				case nativeRestoreJournal:
					if stored == nil {
						return nil, nil
					}
					if creates > 0 && fault == "replaced readback" {
						valueObject(stored, "metadata")["uid"] = "replacement-restore-journal"
					}
					return json.Marshal(stored)
				case nativeSeedJournal:
					uid := state.SeedJournal.UID
					if fault == "replaced seed" || fault == "changed seed readback" && creates > 0 {
						uid = "another-seed-journal"
					}
					return json.Marshal(object{"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": object{"name": nativeSeedJournal, "namespace": "longhorn-system", "uid": uid, "resourceVersion": "9"},
						"data":     object{"record": string(recordData), "seed": string(seedData)}})
				default:
					t.Fatal("journal initializer read an unrelated object", args)
					return nil, nil
				}
			}}
			if fault == "wrong context" {
				c.context = "oidc@prod"
			}
			source := nativeRestoreSeedReceipt{Journal: state.SeedJournal, Record: record, Seed: seed}
			id, err := createNativeRestoreJournal(t.Context(), c, source, state)
			wantCreates := 0
			switch fault {
			case "", "lost create response", "replaced readback", "changed seed readback":
				wantCreates = 1
			}
			if (err != nil) != (fault != "") || (id == "original-restore-journal") != (fault == "") || creates != wantCreates {
				t.Fatalf("incorrect ownership verdict for %q: id=%q creates=%d err=%v", fault, id, creates, err)
			}
			if (fault == "wrong context" || fault == "wrong grant") && calls != 0 {
				t.Fatal("an unauthorized initializer reached the source or cluster")
			}
			if stored != nil {
				if id, err = createNativeRestoreJournal(t.Context(), c, source, state); err == nil || id != "" || creates != wantCreates {
					t.Fatal("a retry adopted or rewrote a preserved restore journal", id, err)
				}
			}
		})
	}
}

func TestNativeRestoreProgressKeepsBothOriginalJournalsAndNeverRetries(t *testing.T) {
	for _, fault := range []string{"", "wrong grant", "wrong owner", "other journal", "changed phase", "changed seed UID", "stale source", "conflict", "lost response", "replacement readback", "changed seed readback", "unchanged readback"} {
		t.Run(fault, func(t *testing.T) {
			record, seed, before := nativeRestoreStateFixture(t, "write-requested")
			_, _, after := nativeRestoreStateFixture(t, "write-verified")
			for key, val := range nativeRestoreDispatch(before) {
				t.Setenv(key, val)
			}
			if fault == "wrong grant" {
				t.Setenv("LONGHORN_V2_OPERATION", nativeSeedOperation)
			}
			if fault == "wrong owner" {
				t.Setenv("GITHUB_RUN_ID", "9999")
			}
			stored := before
			if fault == "changed phase" {
				stored = after
			}
			patches, reads, sourceReads := 0, 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					sourceReads++
					if input != nil || strings.Join(args, " ") != "ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main" {
						t.Fatal("the restore write did not rebind reviewed main")
					}
					sha := before.Source
					if fault == "stale source" {
						sha = strings.Repeat("f", 40)
					}
					return []byte(sha + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 5 || args[0] != "--context=admin@prod" || args[3] != "configmap" ||
					(args[4] != nativeRestoreJournal && args[4] != nativeSeedJournal) {
					t.Fatal("restore progress escaped its fixed two journals", program, args)
				}
				if args[2] == "patch" {
					patches++
					var patch []object
					if args[4] != nativeRestoreJournal || json.Unmarshal(input, &patch) != nil || len(patch) != 3 ||
						!reflect.DeepEqual(patch[0], object{"op": "test", "path": "/metadata/uid", "value": "restore-journal-uid"}) ||
						!reflect.DeepEqual(patch[1], object{"op": "test", "path": "/metadata/resourceVersion", "value": "17"}) ||
						text(patch[2], "op") != "replace" || text(patch[2], "path") != "/data/restore" {
						t.Fatal("progress was not one original-UID/revision restore-only patch")
					}
					if err := json.Unmarshal([]byte(text(patch[2], "value")), &stored); err != nil || !reflect.DeepEqual(stored, after) {
						t.Fatal("the patch changed the intended next state", err)
					}
					if fault == "conflict" || fault == "lost response" {
						return nil, errors.New("fixture patch outcome is uncertain")
					}
					if fault == "unchanged readback" {
						stored = before
					}
					return []byte("configmap/" + nativeRestoreJournal), nil
				}
				if args[2] != "get" || input != nil {
					t.Fatal("unexpected restore progress operation", args)
				}
				reads++
				cm := resource(args[4])
				cm["apiVersion"], cm["kind"] = "v1", "ConfigMap"
				meta := cm["metadata"].(object)
				meta["namespace"], meta["resourceVersion"] = "longhorn-system", "17"
				if args[4] == nativeSeedJournal {
					meta["uid"] = before.SeedJournal.UID
					if fault == "changed seed UID" || patches > 0 && fault == "changed seed readback" {
						meta["uid"] = "replacement-seed-journal"
					}
					raw, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					body, err := json.Marshal(seed)
					if err != nil {
						t.Fatal(err)
					}
					cm["data"] = object{"record": string(raw), "seed": string(body)}
				} else {
					meta["uid"] = "restore-journal-uid"
					if fault == "other journal" || patches > 0 && fault == "replacement readback" {
						meta["uid"] = "replacement-restore-journal"
					}
					body, err := json.Marshal(stored)
					if err != nil {
						t.Fatal(err)
					}
					cm["data"] = object{"restore": string(body)}
				}
				return json.Marshal(cm)
			}}
			err := saveNativeRestoreState(t.Context(), c, "restore-journal-uid", before, after)
			if (err == nil) != (fault == "") {
				t.Fatal("unconfirmed or foreign restore progress was accepted", fault, err)
			}
			wantPatches, wantReads, wantSourceReads := 0, 2, 0
			switch fault {
			case "wrong grant", "wrong owner":
				wantReads = 0
			case "stale source":
				wantSourceReads = 1
			case "conflict", "lost response":
				wantPatches, wantSourceReads = 1, 1
			case "", "replacement readback", "changed seed readback", "unchanged readback":
				wantPatches, wantReads, wantSourceReads = 1, 4, 1
			}
			if patches != wantPatches || reads != wantReads || sourceReads != wantSourceReads {
				t.Fatalf("unexpected calls or retry: patches=%d reads=%d main=%d", patches, reads, sourceReads)
			}
		})
	}
}

func TestNativeRestoreProgressRefusesEveryOtherWorkflowGrant(t *testing.T) {
	_, _, state := nativeRestoreStateFixture(t, "created")
	for key, other := range map[string]string{
		"GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/heads/candidate", "GITHUB_REPOSITORY": "other/platform",
		"GITHUB_RUN_ATTEMPT": "2", "RUNNER_ENVIRONMENT": "self-hosted", "GITHUB_WORKFLOW_REF": "other",
		"GITHUB_SHA": "short", "GITHUB_RUN_ID": "other", "LONGHORN_V2_OPERATION": nativeSeedOperation,
		"LONGHORN_V2_CONFIRM": nativeSeedConfirmation, "LONGHORN_V2_NODE": "prod-worker-1", "LONGHORN_V2_DATABASE": "wedding-db",
	} {
		t.Run(key, func(t *testing.T) {
			env := nativeRestoreDispatch(state)
			env[key] = other
			if authorizeNativeRestore(func(key string) string { return env[key] }) == nil {
				t.Fatal("a different workflow or operation can write restore progress")
			}
		})
	}
}
