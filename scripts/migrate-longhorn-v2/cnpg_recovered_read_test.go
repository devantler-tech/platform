package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestRecoveredFilesystemReadBindsFixedCommandsAndProducer(t *testing.T) {
	for _, scenario := range []string{"original", "different process", "partial walk", "command failed", "different mount", "same-device nested mount"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRecoveredStorageFixture(t)
			mount := `{"filesystems":[{"target":"/var/lib/postgresql/data","fsroot":"/","fstype":"ext4","maj:min":"253:0","id":60,"vfs-options":"rw,relatime"}]}`
			directories := "64768 10 directory\n64768 11 directory\n64768 12 directory\n64768 13 directory\n"
			walk := "d 64768 11\nd 64768 12\nf 64768 14\nd 64768 13\n"
			var calls []string
			c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program != "kubectl" || len(input) != 0 || len(args) < 3 {
					t.Fatal("unexpected recovered filesystem request")
				}
				calls = append(calls, args[2])
				if args[2] == "get" {
					if len(calls) > 1 && scenario == "different process" {
						f.pod["spec"].(object)["shareProcessNamespace"] = true
					}
					return json.Marshal(f.pod)
				}
				parts := strings.Join(args, " ")
				if args[2] != "exec" || !strings.Contains(parts, "--container=postgres --pod-running-timeout=20s -- /usr/bin/env -i LC_ALL=C /usr/bin/timeout --kill-after=2s 15s") {
					t.Fatal("filesystem command used an unbound container or environment")
				}
				if len(calls) == 2 || len(calls) == 6 {
					if args[14] != "/usr/bin/findmnt" || !slices.Equal(args[15:], []string{"--kernel", "--nocanonicalize", "--json", "--submounts", "--mountpoint", "/var/lib/postgresql/data", "--output", "TARGET,FSROOT,FSTYPE,MAJ:MIN,ID,VFS-OPTIONS"}) {
						t.Fatal("mount inventory differs from the native proof")
					}
					if scenario == "different mount" && len(calls) == 6 {
						return []byte(strings.Replace(mount, `"id":60`, `"id":61`, 1)), nil
					}
					if scenario == "same-device nested mount" {
						return []byte(strings.Replace(mount, `"vfs-options":"rw,relatime"`, `"vfs-options":"rw,relatime","children":[{"maj:min":"253:0"}]`, 1)), nil
					}
					return []byte(mount), nil
				}
				if len(calls) == 4 {
					if !slices.Equal(args[15:], []string{"-P", "/var/lib/postgresql/data/pgdata", "-xdev", "-printf", "%y %D %i\n"}) || args[14] != "/usr/bin/find" {
						t.Fatal("walk arguments differ from the native proof")
					}
					switch scenario {
					case "partial walk":
						return []byte(strings.TrimSuffix(walk, "\n")), nil
					case "command failed":
						return []byte(walk), errors.New("private canary")
					}
					return []byte(walk), nil
				}
				if args[14] != "/usr/bin/stat" || !slices.Equal(args[15:], []string{"--format=%d %i %F", "--", "/var/lib/postgresql/data", "/var/lib/postgresql/data/pgdata", "/var/lib/postgresql/data/pgdata/pg_wal", "/var/lib/postgresql/data/pgdata/pg_tblspc"}) {
					t.Fatal("stat arguments differ from the native proof")
				}
				return []byte(directories), nil
			}}
			proof, err := c.readRecoveredFilesystem(t.Context(), f.cluster, f.pod)
			if scenario == "original" {
				if err != nil || proof.Device != 64768 || proof.Entries != 4 || strings.Join(calls, ",") != "get,exec,exec,exec,exec,exec,get" {
					t.Fatalf("native filesystem evidence did not bind: %+v %v", proof, err)
				}
			} else if err == nil || proof != (cnpgFilesystemBinding{}) || strings.Contains(err.Error(), "private canary") {
				t.Fatalf("unproven response escaped: %+v %v", proof, err)
			}
		})
	}
}

// TestRecoveredReadUsesTheFixedLocalCommandInsideNativeProcessBrackets checks command provenance.
func TestRecoveredReadUsesTheFixedLocalCommandInsideNativeProcessBrackets(t *testing.T) {
	f := newRecoveredStorageFixture(t)
	cluster, pod := f.cluster, f.pod
	response, barrier := recoveredHistoryFixture()
	sequence := []string{}
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if program != "kubectl" || len(input) != 0 || len(args) < 3 {
			t.Fatal("unexpected recovered database command")
		}
		sequence = append(sequence, args[2])
		if args[2] == "get" {
			return json.Marshal(pod)
		}
		if args[2] != "exec" || args[len(args)-1] != "--command="+postgresRecoverySQL || !strings.Contains(strings.Join(args, " "), "/usr/bin/env -i PATH=") || !slices.Contains(args, "PGOPTIONS=-c search_path=pg_catalog -c default_transaction_read_only=on") || !strings.Contains(strings.Join(args, " "), "--host=/controller/run --port=5432 --no-psqlrc --no-password") {
			t.Fatal("recovery read changed its executable or SQL")
		}
		return json.Marshal(response)
	}}
	raw, err := c.readRecoveredPostgres(context.Background(), cluster, pod)
	if err != nil || strings.Join(sequence, ",") != "get,exec,get" || verifyCNPGRecoveredHistory(raw, barrier) != nil {
		t.Fatalf("bound recovered read failed: %v", err)
	}
}

// TestRecoveredReadCannotTargetAnActiveSourceDatabase prevents confusing source and restored evidence.
func TestRecoveredReadCannotTargetAnActiveSourceDatabase(t *testing.T) {
	observation := cnpgObservationFixture(t)
	c := client{context: "admin@prod", run: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("source database reached recovery command")
		return nil, nil
	}}
	if _, err := c.readRecoveredPostgres(context.Background(), observation.Cluster, observation.Pods[0]); err == nil {
		t.Fatal("source database accepted as isolated recovery")
	}
}
