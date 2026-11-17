package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCanarySeedProgressIsAnOriginalOwnerConditionalWrite(t *testing.T) {
	for _, fault := range []string{"", "other journal", "changed phase", "wrong owner", "stale source", "conflict", "lost response", "replacement readback", "changed retirement record", "unchanged readback"} {
		t.Run(fault, func(t *testing.T) {
			env := nativeSeedDispatch()
			for k, v := range env {
				t.Setenv(k, v)
			}
			r, before := seedStateFixture(t, "created")
			r.Source, before.Source = env["GITHUB_SHA"], env["GITHUB_SHA"]
			after := before
			after.Phase = "source-write-requested"
			stored := before
			if fault == "changed phase" {
				stored = after
			}
			if fault == "wrong owner" {
				t.Setenv("GITHUB_RUN_ID", "5678")
			}
			patches, reads, sourceReads := 0, 0, 0
			c := client{context: "admin@prod", nativeSeed: true, run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				switch program {
				case "git":
					sourceReads++
					if input != nil || strings.Join(args, " ") != "ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main" {
						t.Fatal("the write did not rebind reviewed main")
					}
					sha := r.Source
					if fault == "stale source" {
						sha = strings.Repeat("f", 40)
					}
					return []byte(sha + "\trefs/heads/main\n"), nil
				case "kubectl":
					if len(args) < 5 || args[0] != "--context=admin@prod" || args[4] != nativeSeedJournal {
						t.Fatal("progress escaped the fixed journal and context", args)
					}
					if args[2] == "patch" {
						patches++
						var patch []object
						if json.Unmarshal(input, &patch) != nil || len(patch) != 3 ||
							!reflect.DeepEqual(patch[0], object{"op": "test", "path": "/metadata/uid", "value": "seed-journal-uid"}) ||
							!reflect.DeepEqual(patch[1], object{"op": "test", "path": "/metadata/resourceVersion", "value": "17"}) ||
							text(patch[2], "op") != "replace" || text(patch[2], "path") != "/data/seed" {
							t.Fatal("progress was not one UID/revision conditional seed-only patch")
						}
						if err := json.Unmarshal([]byte(text(patch[2], "value")), &stored); err != nil || stored != after {
							t.Fatal("the patch changed the intended next state", err)
						}
						if fault == "conflict" || fault == "lost response" {
							return nil, errors.New("patch outcome is uncertain")
						}
						if fault == "unchanged readback" {
							stored = before
						}
						return []byte("configmap/" + nativeSeedJournal), nil
					}
					if args[2] != "get" || args[3] != "configmap" || input != nil {
						t.Fatal("unexpected seed progress operation", args)
					}
					reads++
					cm := resource(nativeSeedJournal)
					cm["metadata"].(object)["namespace"] = "longhorn-system"
					cm["metadata"].(object)["uid"] = "seed-journal-uid"
					cm["metadata"].(object)["resourceVersion"] = "17"
					if fault == "other journal" || patches > 0 && fault == "replacement readback" {
						cm["metadata"].(object)["uid"] = "replacement-uid"
					}
					if patches > 0 && fault == "changed retirement record" {
						m := r.Machines["prod-worker-1"]
						m.OSImage += " changed"
						r.Machines["prod-worker-1"] = m
					}
					raw, err := json.Marshal(r)
					if err != nil {
						t.Fatal(err)
					}
					seed, err := json.Marshal(stored)
					if err != nil {
						t.Fatal(err)
					}
					cm["data"] = object{"record": string(raw), "seed": string(seed)}
					return json.Marshal(cm)
				default:
					t.Fatal("unexpected command", program)
					return nil, errors.New("unexpected command")
				}
			}}
			err := saveCanarySeedState(t.Context(), c, "seed-journal-uid", before, after)
			if (err == nil) != (fault == "") {
				t.Fatal("unconfirmed or foreign progress was accepted", err)
			}
			wantPatches, wantReads, wantSourceReads := 0, 1, 0
			switch fault {
			case "stale source":
				wantSourceReads = 1
			case "conflict", "lost response":
				wantPatches, wantSourceReads = 1, 1
			case "", "replacement readback", "changed retirement record", "unchanged readback":
				wantPatches, wantReads, wantSourceReads = 1, 2, 1
			}
			if patches != wantPatches || reads != wantReads || sourceReads != wantSourceReads {
				t.Fatalf("unexpected calls or retry: patches=%d reads=%d main=%d", patches, reads, sourceReads)
			}
		})
	}
}

func TestCanarySeedJournalRejectsIncompleteNativeIdentity(t *testing.T) {
	for _, fault := range []string{"", "different name", "different namespace", "missing UID", "missing revision", "deleting", "missing seed", "ambiguous seed", "other scope"} {
		t.Run(fault, func(t *testing.T) {
			r, s := seedStateFixture(t, "created")
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			seed, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			c := client{nativeSeed: fault != "other scope", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				calls++
				if input != nil || program != "kubectl" || len(args) < 5 || args[2] != "get" || args[4] != nativeSeedJournal {
					t.Fatal("journal admission performed a mutation or escaped its scope")
				}
				cm := resource(nativeSeedJournal)
				meta := cm["metadata"].(object)
				meta["namespace"] = "longhorn-system"
				cm["data"] = object{"record": string(raw), "seed": string(seed)}
				switch fault {
				case "different name":
					meta["name"] = "foreign-journal"
				case "different namespace":
					meta["namespace"] = "foreign-namespace"
				case "missing UID":
					delete(meta, "uid")
				case "missing revision":
					delete(meta, "resourceVersion")
				case "deleting":
					meta["deletionTimestamp"] = "2026-11-15T05:00:00Z"
				case "missing seed":
					delete(cm["data"].(object), "seed")
				case "ambiguous seed":
					cm["data"].(object)["seed"] = strings.Replace(string(seed), `"Version":1`, `"Version":1,"Version":1`, 1)
				}
				return json.Marshal(cm)
			}}
			_, _, _, err = readCanarySeedJournal(t.Context(), c)
			if (err == nil) != (fault == "") || calls != map[bool]int{true: 0, false: 1}[fault == "other scope"] {
				t.Fatal("journal admission did not preserve the original native identity", err, calls)
			}
		})
	}
}

func TestCanarySeedSpecDigestRequiresACanonicalNonemptySpec(t *testing.T) {
	a, err := seedSpecDigest(object{"spec": object{"provider": "aws", "objectStorage": object{"bucket": "fixture", "prefix": "canary"}}})
	if err != nil || !seedSHA256.MatchString(a) {
		t.Fatal("the original destination spec could not be captured", err)
	}
	b, err := seedSpecDigest(object{"spec": object{"objectStorage": object{"prefix": "canary", "bucket": "fixture"}, "provider": "aws"}})
	if err != nil || a != b {
		t.Fatal("map order changed the destination identity")
	}
	changed, err := seedSpecDigest(object{"spec": object{"provider": "aws", "objectStorage": object{"bucket": "replacement", "prefix": "canary"}}})
	if err != nil || a == changed {
		t.Fatal("a changed destination retained its original identity")
	}
	for _, malformed := range []object{{}, {"spec": object{}}, {"spec": "not-an-object"}} {
		if _, err := seedSpecDigest(malformed); err == nil {
			t.Fatal("an incomplete destination was accepted")
		}
	}
}
