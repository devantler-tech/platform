package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestLoopbackRequestReadsTheExporter exercises the real static Bash request,
// rather than accepting a fake response from a script that never reaches HTTP.
func TestLoopbackRequestReadsTheExporter(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:9187")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/metrics" || r.Proto != "HTTP/1.0" {
			t.Errorf("unexpected exporter request: %s %s %s", r.Method, r.URL.Path, r.Proto)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if _, err := fmt.Fprintln(w, "cnpg_collector_fencing_on 1"); err != nil {
			t.Error(err)
		}
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-p", "-c", metricsRequest)
	cmd.Env = append(os.Environ(), "BASH_ENV=/no-such-startup-script")
	if os.Getenv("WEDDING_REPAIR_OPERAND_TEST") == "true" {
		// CI uses the immutable image currently observed on the failed Pod.
		// Host networking lets this read-only container reach this local fixture.
		cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "--network=host", "--entrypoint=/usr/bin/timeout", "ghcr.io/cloudnative-pg/postgresql@sha256:42708a75345b7a48fdd9257b071830783a97fd228529196b6313187a7198e185", "--kill-after=2s", "10s", "/bin/bash", "--noprofile", "--norc", "-p", "-c", metricsRequest)
	}
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] == "exec" {
			return b, nil
		}
		return json.Marshal(fixture().pods[0])
	}}
	if !c.fenced(ctx, identity{"wedding-db-1", "target-uid"}) {
		t.Fatalf("real exporter response was not acknowledged: %q", b)
	}
}

// TestFenceReadDoesNotNeedPodIngress catches a proxy read that cannot cross
// the database's default-deny network policy, even when the instance is fenced.
func TestFenceReadDoesNotNeedPodIngress(t *testing.T) {
	var reads []string
	c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		reads = append(reads, args[0])
		if args[0] == "exec" {
			if !reflect.DeepEqual(args[:8], []string{"exec", "-n", "wedding-app", "wedding-db-1", "-c", "postgres", "--", "/usr/bin/timeout"}) {
				t.Fatalf("probe escaped the exact target container: %v", args)
			}
			return []byte("HTTP/1.0 200 OK\r\nContent-Length: 28\r\n\r\ncnpg_collector_fencing_on 1\n"), nil
		}
		if len(args) > 1 && args[1] == "pod" {
			return json.Marshal(fixture().pods[0])
		}
		return nil, errors.New("Pod ingress denied")
	}}
	if !c.fenced(context.Background(), identity{"wedding-db-1", "target-uid"}) {
		t.Fatal("instance-owned fencing proof incorrectly requires Pod ingress")
	}
	if !reflect.DeepEqual(reads, []string{"get", "exec", "get"}) {
		t.Fatalf("metrics were not bracketed by target identity reads: %v", reads)
	}
}

// TestFullyQualifiedDatabaseResources prevents selecting an unrelated Cluster API.
func TestFullyQualifiedDatabaseResources(t *testing.T) {
	c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[1] == "cluster" || args[1] == "backups" {
			t.Fatal("ambiguous CR kind could select a different API group")
		}
		return fakeRead(fixture(), args[1:])
	}}
	if err := repair(context.Background(), c, testOptions(), false); err != nil {
		t.Fatal(err)
	}
}

// TestDuplicateRecoveryObservationIsNotClean requires a distinct replacement observation.
func TestDuplicateRecoveryObservationIsNotClean(t *testing.T) {
	s := fixture()
	p := plan{primary: id(s.pods[1]), healthy: id(s.pods[2]), target: id(s.pods[0])}
	s.pods = []object{s.pods[1], s.pods[2], s.pods[2]}
	at(s.cluster, "status")["readyInstances"] = float64(3)
	appendCondition(s.cluster, object{"type": "Ready", "status": "True"})
	if ok, err := (client{}).recovered(context.Background(), s, testOptions(), p, id(s.claims[0])); ok || err == nil {
		t.Fatal("duplicate observations cleared recovery without a replacement")
	}
}

// TestDispatchBoundary refuses each independently invalid execution binding.
func TestDispatchBoundary(t *testing.T) {
	good := map[string]string{"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/replace-wedding-standby.yaml@refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "WEDDING_REPAIR_CONFIRM": "retain-volumes-replace-failed-standby", "GITHUB_SHA": strings.Repeat("a", 40)}
	for key := range good {
		t.Run(key, func(t *testing.T) {
			for k, v := range good {
				t.Setenv(k, v)
			}
			t.Setenv(key, "wrong")
			previousFlags, previousArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine = previousFlags; os.Args = previousArgs })
			flag.CommandLine = flag.NewFlagSet("repair", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = []string{"repair", "--execute", "--cluster-uid", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "--pod-uid", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}
			err := run()
			if err == nil || !strings.Contains(err.Error(), "execution requires") {
				t.Fatalf("protected boundary returned %v", err)
			}
		})
	}
}

// TestResumeDispatchMustMatchExplicitConfirmation refuses a continuation not
// explicitly selected by the protected workflow, even with ordinary approval.
func TestResumeDispatchMustMatchExplicitConfirmation(t *testing.T) {
	for _, tc := range []struct{ flag, input string }{{"true", "false"}, {"false", "true"}, {"true", ""}, {"true", "yes"}} {
		t.Run(tc.flag+"/"+tc.input, func(t *testing.T) {
			for k, v := range map[string]string{"GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/replace-wedding-standby.yaml@refs/heads/main", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "WEDDING_REPAIR_CONFIRM": "retain-volumes-replace-failed-standby", "GITHUB_SHA": strings.Repeat("a", 40), "WEDDING_REPAIR_RESUME_FENCED": tc.input} {
				t.Setenv(k, v)
			}
			previousFlags, previousArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine = previousFlags; os.Args = previousArgs })
			flag.CommandLine = flag.NewFlagSet("repair", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = []string{"repair", "--execute", "--resume-fenced=" + tc.flag, "--cluster-uid", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "--pod-uid", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}
			if err := run(); err == nil || !strings.Contains(err.Error(), "explicitly selected") {
				t.Fatalf("continuation binding returned %v", err)
			}
		})
	}
}

// TestInvalidCLIInputsStopBeforeObservation refuses implicit identities and
// stray arguments instead of invoking any Kubernetes read or write.
func TestInvalidCLIInputsStopBeforeObservation(t *testing.T) {
	for _, args := range [][]string{{"repair"}, {"repair", "--resume-fenced", "extra"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			previousFlags, previousArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine = previousFlags; os.Args = previousArgs })
			flag.CommandLine = flag.NewFlagSet("repair", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = args
			if err := run(); err == nil || (!strings.Contains(err.Error(), "unexpected argument") && !strings.Contains(err.Error(), "UIDs are required")) {
				t.Fatalf("unsafe input returned %v", err)
			}
		})
	}
}

// TestFenceMetricMustBeCompleteAndTrue rejects missing, duplicate and failed evidence.
func TestFenceMetricMustBeCompleteAndTrue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		failed, ok bool
	}{
		{"fenced", "HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 1\n", false, true},
		{"failed exec", "HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 1\n", true, false},
		{"not fenced", "HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 0\n", false, false},
		{"duplicate gauge", "HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 1\ncnpg_collector_fencing_on 1\n", false, false},
		{"empty", "", false, false},
		{"non-OK HTTP", "HTTP/1.0 503 Unavailable\r\n\r\ncnpg_collector_fencing_on 1\n", false, false},
		{"truncated body", "HTTP/1.0 200 OK\r\nContent-Length: 100\r\n\r\ncnpg_collector_fencing_on 1\n", false, false},
		{"trailing response", "HTTP/1.0 200 OK\r\nContent-Length: 28\r\n\r\ncnpg_collector_fencing_on 1\nextra", false, false},
		{"oversized", "HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 1\n" + strings.Repeat("#", 1<<20), false, false},
		{"timestamped gauge", "HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 1 123\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[0] == "get" {
					return fakeRead(fixture(), args[1:])
				}
				if tc.failed {
					return []byte(tc.body), errors.New("partial response")
				}
				return []byte(tc.body), nil
			}}
			if got := c.fenced(context.Background(), identity{"wedding-db-1", "target-uid"}); got != tc.ok {
				t.Fatalf("fencing=%v, want %v", got, tc.ok)
			}
		})
	}
}

// TestFenceMetricCannotClearChangedIdentity rejects a replaced, deleting or
// unreadable Pod before or after exec; valid metrics alone do not bind identity.
func TestFenceMetricCannotClearChangedIdentity(t *testing.T) {
	for _, after := range []bool{false, true} {
		for _, failure := range []string{"uid", "deleting", "failed read"} {
			t.Run(fmt.Sprintf("after=%v/%s", after, failure), func(t *testing.T) {
				reads, execs := 0, 0
				c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
					if args[0] == "exec" {
						execs++
						return []byte("HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 1\n"), nil
					}
					reads++
					pod := fixture().pods[0]
					if (reads == 2) == after {
						switch failure {
						case "uid":
							at(pod, "metadata")["uid"] = "replacement"
						case "deleting":
							at(pod, "metadata")["deletionTimestamp"] = "now"
						case "failed read":
							return nil, errors.New("unreadable identity")
						}
					}
					return json.Marshal(pod)
				}}
				if c.fenced(context.Background(), identity{"wedding-db-1", "target-uid"}) {
					t.Fatal("metrics cleared a changed or unreadable target")
				}
				if !after && execs != 0 {
					t.Fatal("executed inside an unbound target")
				}
			})
		}
	}
}

// TestDispatchMustNameTheProtectedRecoveryWorkflow rejects a different dispatch caller.
func TestDispatchMustNameTheProtectedRecoveryWorkflow(t *testing.T) {
	env := map[string]string{"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ATTEMPT": "1", "WEDDING_REPAIR_CONFIRM": "retain-volumes-replace-failed-standby", "GITHUB_SHA": strings.Repeat("a", 40), "GITHUB_WORKFLOW_REF": "devantler-tech/platform/.github/workflows/replace-wedding-standby.yaml@refs/heads/main"}
	get := func(key string) string { return env[key] }
	if !dispatchAllowed(get) {
		t.Fatal("reviewed dispatch refused")
	}
	env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/other.yaml@refs/heads/main"
	if dispatchAllowed(get) {
		t.Fatal("another workflow received recovery authority")
	}
}

// fakeRead returns Kubernetes-shaped responses for the transaction's exact reads.
func fakeRead(s inventory, args []string) ([]byte, error) {
	kind, name := strings.TrimSuffix(args[0], ".postgresql.cnpg.io"), ""
	if len(args) > 1 {
		name = args[1]
	}
	var result any
	switch kind {
	case "cluster":
		result = s.cluster
	case "deployment":
		result = s.operator
	case "pods":
		result = object{"items": s.pods}
	case "pod":
		for _, pod := range s.pods {
			if id(pod).name == name {
				result = pod
			}
		}
	case "pvc":
		if name == "wedding-db-4" {
			claim := fixture().claims[0]
			at(claim, "metadata")["name"] = "wedding-db-4"
			at(claim, "metadata")["uid"] = "new-claim-uid"
			at(claim, "spec")["volumeName"] = "new-pv"
			result = claim
		} else {
			result = object{"items": s.claims}
		}
	case "pv":
		if name == "new-pv" {
			pv := fixture().volumes["old-pv"]
			at(pv, "metadata")["name"] = "new-pv"
			at(pv, "metadata")["uid"] = "new-pv-uid"
			at(pv, "spec", "claimRef")["name"] = "wedding-db-4"
			at(pv, "spec", "claimRef")["uid"] = "new-claim-uid"
			result = pv
		} else {
			result = s.volumes[name]
		}
	case "backups":
		result = object{"items": s.backups}
	case "jobs":
		jobs := s.jobs
		if jobs == nil {
			jobs = []object{}
		}
		result = object{"items": jobs}
	default:
		return nil, errors.New("unexpected read")
	}
	return json.Marshal(result)
}

// TestReadOnlyPlanAndPartialReadFailure rejects plausible output followed by failure.
func TestReadOnlyPlanAndPartialReadFailure(t *testing.T) {
	for _, fail := range []string{"", "cluster", "deployment", "pods", "pvc", "pv", "backups", "jobs"} {
		t.Run("partial "+fail, func(t *testing.T) {
			c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[0] != "get" {
					t.Fatal("plan issued a write")
				}
				data, err := fakeRead(fixture(), args[1:])
				if len(args) > 1 && strings.TrimSuffix(args[1], ".postgresql.cnpg.io") == fail {
					return data, errors.New("plausible partial response followed by failure")
				}
				return data, err
			}}
			err := repair(context.Background(), c, testOptions(), false)
			if (err == nil) != (fail == "") {
				t.Fatalf("error=%v on failed read %q", err, fail)
			}
		})
	}
}

// TestMalformedJobListingCannotClearPlan rejects entries that cannot be examined.
func TestMalformedJobListingCannotClearPlan(t *testing.T) {
	for _, entry := range []any{"not an object", nil} {
		c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
			if args[0] != "get" {
				t.Fatal("plan issued a write")
			}
			if args[1] == "jobs" {
				return json.Marshal(object{"items": []any{entry}})
			}
			return fakeRead(fixture(), args[1:])
		}}
		if err := repair(context.Background(), c, testOptions(), false); err == nil {
			t.Fatalf("unexamined job entry %v cleared the plan", entry)
		}
	}
}

// TestExecutionRefusesFailedSourceBeforeWrites protects against stale-main execution.
func TestExecutionRefusesFailedSourceBeforeWrites(t *testing.T) {
	writes := 0
	c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] != "get" {
			writes++
			return nil, nil
		}
		return fakeRead(fixture(), args[1:])
	}, source: func(context.Context) error { return errors.New("source stale") }}
	if err := repair(context.Background(), c, testOptions(), true); err == nil {
		t.Fatal("stale source accepted")
	}
	if writes != 0 {
		t.Fatal("write before source proof")
	}
}

// TestRepairStopsOnIntermediateDrift prevents later mutations after a safety gate changes.
func TestRepairStopsOnIntermediateDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		after  int
		change func(inventory)
	}{
		{"primary changed before fence", 1, func(s inventory) { at(s.cluster, "status")["currentPrimary"] = "wedding-db-3" }},
		{"healthy peer replaced before detach", 2, func(s inventory) { at(s.pods[2], "metadata")["uid"] = "changed" }},
		{"volume binding changed before detach", 2, func(s inventory) { at(s.volumes["old-pv"], "spec", "claimRef")["uid"] = "changed" }},
		{"target ready before detach", 2, func(s inventory) {
			at(s.pods[0], "status")["conditions"] = []any{object{"type": "Ready", "status": "True"}}
		}},
		{"pod replaced before delete", 3, func(s inventory) { at(s.pods[0], "metadata")["uid"] = "changed" }},
		{"primary changing before delete", 3, func(s inventory) { at(s.cluster, "status")["targetPrimary"] = "wedding-db-1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture()
			writes := 0
			c := client{source: func(context.Context) error { return nil }, wait: func(context.Context) error { return errors.New("unexpected wait") }}
			c.command = func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[0] == "exec" {
					return []byte("HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 1\n"), nil
				}
				if args[0] == "get" {
					if args[1] == "--raw" {
						return []byte("cnpg_collector_fencing_on 1\n"), nil
					}
					return fakeRead(s, args[1:])
				}
				writes++
				if args[0] != "patch" {
					t.Fatal("unsafe pod deletion after drift")
				}
				switch args[1] {
				case "pv":
					at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
				case "cluster.postgresql.cnpg.io":
					at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-1"]`
				case "pvc":
					at(s.claims[0], "metadata")["ownerReferences"] = []any{}
					at(s.claims[0], "metadata", "annotations")["cnpg.io/pvcStatus"] = "detached"
				default:
					t.Fatal("unexpected mutation")
				}
				if writes == tc.after {
					tc.change(s)
				}
				return []byte(`{}`), nil
			}
			if err := repair(context.Background(), c, testOptions(), true); err == nil {
				t.Fatal("drift cleared repair")
			}
			if writes != tc.after {
				t.Fatalf("made %d writes, expected stop at %d", writes, tc.after)
			}
		})
	}
}

// TestFailedConditionalWriteDoesNotRetry keeps a rejected mutation in HOLD.
func TestFailedConditionalWriteDoesNotRetry(t *testing.T) {
	writes := 0
	c := client{source: func(context.Context) error { return nil }}
	c.command = func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] == "get" {
			return fakeRead(fixture(), args[1:])
		}
		writes++
		return []byte(`{"status":"Success"}`), errors.New("CAS rejected after output")
	}
	if err := repair(context.Background(), c, testOptions(), true); err == nil {
		t.Fatal("failed write treated as clean")
	}
	if writes != 1 {
		t.Fatal("write retried or cleanup attempted")
	}
}

// TestUnacknowledgedFenceCannotDetachStorage preserves ownership until fencing is proven.
func TestUnacknowledgedFenceCannotDetachStorage(t *testing.T) {
	s := fixture()
	writes := 0
	c := client{source: func(context.Context) error { return nil }, wait: func(context.Context) error { return context.DeadlineExceeded }}
	c.command = func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] == "exec" {
			return []byte("HTTP/1.0 200 OK\r\n\r\ncnpg_collector_fencing_on 0\n"), nil
		}
		if args[0] == "get" {
			if args[1] == "--raw" {
				return []byte("cnpg_collector_fencing_on 0\n"), nil
			}
			return fakeRead(s, args[1:])
		}
		writes++
		switch args[1] {
		case "pv":
			at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
		case "cluster.postgresql.cnpg.io":
			at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-1"]`
		default:
			t.Fatal("mutated storage without acknowledged fencing")
		}
		return []byte(`{}`), nil
	}
	if err := repair(context.Background(), c, testOptions(), true); err == nil {
		t.Fatal("unfenced instance cleared repair")
	}
	if writes != 2 || !owned(s.claims[0], "cluster-uid") {
		t.Fatal("claim was changed without fencing")
	}
}

// TestWriteSequencePreservesDataAndPinsDeletes checks proof/read/write ordering and retention.
func TestWriteSequencePreservesDataAndPinsDeletes(t *testing.T) {
	testRepairSequence(t, false)
}

// TestResumeOwnFencePreservesTheOriginalData resumes only the pre-detachment
// HOLD without rewriting an already-owned fence or touching protected peers.
func TestResumeOwnFencePreservesTheOriginalData(t *testing.T) {
	testRepairSequence(t, true)
}

// TestResumePlanRequestsOwnershipEvidence models kubectl's default omission of
// managedFields. A read-only continuation plan must request the ownership proof.
func TestResumePlanRequestsOwnershipEvidence(t *testing.T) {
	s, o := fixture(), testOptions()
	prepareFencedHold(s)
	o.fenced = true
	c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] != "get" {
			t.Fatalf("read-only plan issued %v", args)
		}
		if args[1] == "cluster.postgresql.cnpg.io" && !strings.Contains(strings.Join(args, " "), "--show-managed-fields=true") {
			delete(at(s.cluster, "metadata"), "managedFields")
		}
		return fakeRead(s, args[1:])
	}}
	if err := repair(context.Background(), c, o, false); err != nil {
		t.Fatalf("read-only continuation omitted ownership evidence: %v", err)
	}
}

// TestResumeRechecksOwnershipBeforeMutation refuses a fence taken over after
// the initial plan, including before its first conditional storage mutation.
func TestResumeRechecksOwnershipBeforeMutation(t *testing.T) {
	s, o := fixture(), testOptions()
	prepareFencedHold(s)
	o.fenced = true
	writes := 0
	c := client{wait: func(context.Context) error { return context.DeadlineExceeded }, source: func(context.Context) error {
		list(s.cluster, "metadata", "managedFields")[0]["manager"] = "someone-else"
		return nil
	}, command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if args[0] == "get" {
			return fakeRead(s, args[1:])
		}
		writes++
		return nil, errors.New("unexpected mutation")
	}}
	if err := repair(context.Background(), c, o, true); err == nil || writes != 0 {
		t.Fatalf("lost fence ownership issued %d mutations, error %v", writes, err)
	}
}

func testRepairSequence(t *testing.T, resume bool) {
	t.Helper()
	s := fixture()
	o := testOptions()
	if resume {
		prepareFencedHold(s)
		o.fenced = true
	}
	var writes []string
	proofs := 0
	c := client{source: func(context.Context) error {
		// Controllers can publish harmless status changes during the forge read.
		// Every write must observe the versions AFTER that read, not before it.
		proofs++
		for _, o := range append([]object{s.cluster, s.operator, s.claims[0], s.volumes["old-pv"]}, s.pods...) {
			at(o, "metadata")["resourceVersion"] = fmt.Sprint(proofs)
		}
		return nil
	}, wait: func(context.Context) error { return nil }}
	c.command = func(_ context.Context, args []string, data []byte) ([]byte, error) {
		if args[0] == "exec" {
			return []byte("HTTP/1.0 200 OK\r\n\r\n# gauge\ncnpg_collector_fencing_on 1\n"), nil
		}
		if args[0] == "get" {
			if args[1] == "--raw" {
				return []byte("# gauge\ncnpg_collector_fencing_on 1\n"), nil
			}
			return fakeRead(s, args[1:])
		}
		writes = append(writes, strings.Join(args[:2], " "))
		if args[0] == "delete" {
			if args[1] != "--raw" || !strings.Contains(strings.Join(args, " "), "/pods/wedding-db-1") {
				t.Fatal("deletion escaped the single pod")
			}
			var body object
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatal(err)
			}
			if str(body, "preconditions", "uid") != "target-uid" || str(body, "preconditions", "resourceVersion") != str(s.pods[0], "metadata", "resourceVersion") {
				t.Fatal("delete not bound to exact pod")
			}
			s.pods = s.pods[1:]
			newPod := fixture().pods[2]
			at(newPod, "metadata")["name"] = "wedding-db-4"
			at(newPod, "metadata")["uid"] = "new-uid"
			at(newPod, "metadata", "labels")["cnpg.io/instanceName"] = "wedding-db-4"
			at(newPod, "spec")["volumes"] = []any{object{"persistentVolumeClaim": object{"claimName": "wedding-db-4"}}}
			s.pods = append(s.pods, newPod)
			at(s.cluster, "status")["readyInstances"] = float64(3)
			appendCondition(s.cluster, object{"type": "Ready", "status": "True"})
			return []byte(`{}`), nil
		}
		if args[0] != "patch" {
			t.Fatalf("unexpected write %v", args)
		}
		var ops []object
		if err := json.Unmarshal(data, &ops); err != nil {
			t.Fatal(err)
		}
		if len(ops) < 3 || str(ops[0], "op") != "test" || str(ops[0], "path") != "/metadata/uid" || str(ops[1], "op") != "test" || str(ops[1], "path") != "/metadata/resourceVersion" {
			t.Fatal("patch omitted identity preconditions")
		}
		switch strings.TrimSuffix(args[1], ".postgresql.cnpg.io") {
		case "pv":
			if value(ops[1], "value") != str(s.volumes["old-pv"], "metadata", "resourceVersion") {
				t.Fatal("PV snapshot preceded the source proof")
			}
			at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
		case "cluster":
			if value(ops[1], "value") != str(s.cluster, "metadata", "resourceVersion") {
				t.Fatal("Cluster snapshot preceded the source proof")
			}
			last := ops[len(ops)-1]
			if str(last, "op") == "remove" {
				delete(at(s.cluster, "metadata", "annotations"), fenceKey)
			} else {
				at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-1"]`
			}
		case "pvc":
			if value(ops[1], "value") != str(s.claims[0], "metadata", "resourceVersion") {
				t.Fatal("PVC snapshot preceded the source proof")
			}
			at(s.claims[0], "metadata")["ownerReferences"] = []any{}
			at(s.claims[0], "metadata", "annotations")["cnpg.io/pvcStatus"] = "detached"
		default:
			t.Fatal("write outside authorized resources")
		}
		return []byte(`{}`), nil
	}
	if err := repair(context.Background(), c, o, true); err != nil {
		t.Fatal(err)
	}
	want := "patch pv,patch cluster.postgresql.cnpg.io,patch pvc,delete --raw,patch cluster.postgresql.cnpg.io"
	if resume {
		want = "patch pvc,delete --raw,patch cluster.postgresql.cnpg.io"
	}
	if strings.Join(writes, ",") != want {
		t.Fatalf("write ordering %v; want %s", writes, want)
	}
	if proofs < len(writes) {
		t.Fatal("a write reused an old source proof")
	}
	if id(s.claims[0]).uid != "claim-uid" || str(s.volumes["old-pv"], "spec", "claimRef", "uid") != "claim-uid" {
		t.Fatal("old data binding lost")
	}
}

// prepareFencedHold represents the original workflow's persisted pre-detach
// state, including ownership of the exact annotation and the retained volume.
func prepareFencedHold(s inventory) {
	at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-1"]`
	at(s.cluster, "metadata")["managedFields"] = []any{object{"manager": "wedding-standby-repair", "fieldsV1": object{"f:metadata": object{"f:annotations": object{"f:cnpg.io/fencedInstances": object{}}}}}}
	at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Retain"
	at(s.pods[0], "status")["containerStatuses"] = []any{object{"name": "postgres", "ready": false, "state": object{"running": object{}}}}
}

// TestResumeRefusesUnownedOrAdvancedHold cannot infer authority from the mere
// presence of a fence or resume a later partially-completed storage operation.
func TestResumeRefusesUnownedOrAdvancedHold(t *testing.T) {
	for _, failure := range []string{"foreign fence owner", "unretained volume", "detached claim", "wrong fence", "missing fence"} {
		t.Run(failure, func(t *testing.T) {
			s, o := fixture(), testOptions()
			prepareFencedHold(s)
			o.fenced = true
			switch failure {
			case "foreign fence owner":
				list(s.cluster, "metadata", "managedFields")[0]["manager"] = "someone-else"
			case "unretained volume":
				at(s.volumes["old-pv"], "spec")["persistentVolumeReclaimPolicy"] = "Delete"
			case "detached claim":
				at(s.claims[0], "metadata", "annotations")["cnpg.io/pvcStatus"] = "detached"
			case "wrong fence":
				at(s.cluster, "metadata", "annotations")[fenceKey] = `["wedding-db-3"]`
			case "missing fence":
				delete(at(s.cluster, "metadata", "annotations"), fenceKey)
			}
			writes := 0
			c := client{source: func(context.Context) error { return nil }, wait: func(context.Context) error { return context.DeadlineExceeded }}
			c.command = func(_ context.Context, args []string, _ []byte) ([]byte, error) {
				if args[0] == "get" {
					return fakeRead(s, args[1:])
				}
				writes++
				return nil, errors.New("unexpected write")
			}
			if err := repair(context.Background(), c, o, true); err == nil || writes != 0 {
				t.Fatalf("unsafe continuation issued %d writes, error %v", writes, err)
			}
		})
	}
}

var testNow = time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)

// fixture models the one failed standby and its protected peers and bound storage.
func fixture() inventory {
	owner := []any{object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "wedding-db", "uid": "cluster-uid", "controller": true}}
	meta := func(name, uid string) object {
		return object{"name": name, "namespace": "wedding-app", "uid": uid, "resourceVersion": "10", "ownerReferences": owner}
	}
	pod := func(name, uid, role, ready string) object {
		m := meta(name, uid)
		m["labels"] = object{"cnpg.io/cluster": "wedding-db", "cnpg.io/instanceName": name, "cnpg.io/instanceRole": role, "cnpg.io/podRole": "instance"}
		status := object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": ready}}, "containerStatuses": []any{object{"name": "postgres", "ready": ready == "True", "state": object{"waiting": object{"reason": "CrashLoopBackOff"}}}}}
		return object{"metadata": m, "status": status, "spec": object{"volumes": []any{object{"persistentVolumeClaim": object{"claimName": name}}}}}
	}
	c := object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": object{"name": "wedding-db", "namespace": "wedding-app", "uid": "cluster-uid", "resourceVersion": "10", "annotations": object{}}, "spec": object{"instances": float64(3)}, "status": object{"readyInstances": float64(2), "currentPrimary": "wedding-db-2", "targetPrimary": "wedding-db-2", "conditions": []any{object{"type": "ContinuousArchiving", "status": "True"}, object{"type": "LastBackupSucceeded", "status": "True"}}}}
	claim := object{"metadata": meta("wedding-db-1", "claim-uid"), "spec": object{"volumeName": "old-pv"}, "status": object{"phase": "Bound"}}
	at(claim, "metadata")["labels"] = object{"cnpg.io/cluster": "wedding-db", "cnpg.io/instanceName": "wedding-db-1"}
	at(claim, "metadata")["annotations"] = object{"cnpg.io/pvcStatus": "ready"}
	volume := object{"metadata": object{"name": "old-pv", "uid": "pv-uid", "resourceVersion": "10"}, "spec": object{"claimRef": object{"name": "wedding-db-1", "namespace": "wedding-app", "uid": "claim-uid"}, "persistentVolumeReclaimPolicy": "Delete"}, "status": object{"phase": "Bound"}}
	backup := object{"metadata": meta("daily-backup", "backup-uid"), "spec": object{"cluster": object{"name": "wedding-db"}, "method": "plugin", "pluginConfiguration": object{"name": "barman-cloud.cloudnative-pg.io"}}, "status": object{"phase": "completed", "stoppedAt": testNow.Add(-time.Hour).Format(time.RFC3339), "pluginMetadata": object{"clusterUID": "cluster-uid"}}}
	operator := object{"metadata": object{"name": "cloudnative-pg", "namespace": "cnpg-system", "uid": "operator-uid", "resourceVersion": "10", "generation": float64(1)}, "spec": object{"replicas": float64(2), "template": object{"spec": object{"containers": []any{object{"image": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1"}}}}}, "status": object{"observedGeneration": float64(1), "availableReplicas": float64(2), "updatedReplicas": float64(2)}}
	return inventory{cluster: c, operator: operator, pods: []object{pod("wedding-db-1", "target-uid", "replica", "False"), pod("wedding-db-2", "primary-uid", "primary", "True"), pod("wedding-db-3", "healthy-uid", "replica", "True")}, claims: []object{claim}, volumes: map[string]object{"old-pv": volume}, backups: []object{backup}}
}

// at requires the fixture's expected map shape and fails loudly on fixture mistakes.
func at(o object, path ...string) object {
	for _, k := range path {
		next, ok := o[k].(object)
		if !ok {
			panic("fixture map is missing at " + k)
		}
		o = next
	}
	return o
}

// appendCondition extends decoded conditions without unchecked type assertions.
func appendCondition(o object, c object) {
	items := []any{}
	for _, existing := range list(o, "status", "conditions") {
		items = append(items, existing)
	}
	at(o, "status")["conditions"] = append(items, c)
}

// testOptions binds deterministic identities and freshness for the fixture.
func testOptions() options {
	return options{clusterUID: "cluster-uid", podUID: "target-uid", now: testNow}
}

// TestValidateSafeRepair proves complete safe evidence produces a bound plan.
func TestValidateSafeRepair(t *testing.T) {
	p, err := validate(fixture(), testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if p.primary.uid != "primary-uid" || p.healthy.uid != "healthy-uid" || p.target.uid != "target-uid" || len(p.claims) != 1 {
		t.Fatalf("unbound plan: %+v", p)
	}
}

// TestRefuseUnsafeRepair checks invalid identities, storage, backups and roles.
func TestRefuseUnsafeRepair(t *testing.T) {
	cases := []struct {
		name   string
		change func(*inventory, *options)
	}{
		{"unaudited operator", func(s *inventory, _ *options) {
			at(s.operator, "spec", "template", "spec")["containers"] = []any{object{"image": "other:latest"}}
		}},
		{"operator rolling", func(s *inventory, _ *options) { at(s.operator, "status")["updatedReplicas"] = float64(1) }},
		{"stale cluster", func(_ *inventory, o *options) { o.clusterUID = "previous" }},
		{"stale pod", func(_ *inventory, o *options) { o.podUID = "previous" }},
		{"target primary", func(s *inventory, _ *options) { at(s.cluster, "status")["currentPrimary"] = "wedding-db-1" }},
		{"pending switchover", func(s *inventory, _ *options) { at(s.cluster, "status")["targetPrimary"] = "wedding-db-3" }},
		{"target primary label", func(s *inventory, _ *options) {
			at(s.pods[0], "metadata", "labels")["cnpg.io/instanceRole"] = "primary"
		}},
		{"target ready", func(s *inventory, _ *options) {
			at(s.pods[0], "status")["conditions"] = []any{object{"type": "Ready", "status": "True"}}
		}},
		{"unknown failure", func(s *inventory, _ *options) { at(s.pods[0], "status")["containerStatuses"] = []any{} }},
		{"unhealthy peer", func(s *inventory, _ *options) {
			at(s.pods[2], "status")["conditions"] = []any{object{"type": "Ready", "status": "False"}}
		}},
		{"missing primary", func(s *inventory, _ *options) { s.pods = s.pods[:1] }},
		{"foreign pod owner", func(s *inventory, _ *options) { at(s.pods[0], "metadata")["ownerReferences"] = []any{} }},
		{"deleting pod", func(s *inventory, _ *options) { at(s.pods[0], "metadata")["deletionTimestamp"] = "now" }},
		{"missing pod version", func(s *inventory, _ *options) { delete(at(s.pods[0], "metadata"), "resourceVersion") }},
		{"missing claim", func(s *inventory, _ *options) { s.claims = nil }},
		{"extra claim", func(s *inventory, _ *options) { s.claims = append(s.claims, s.claims[0]) }},
		{"foreign claim owner", func(s *inventory, _ *options) { at(s.claims[0], "metadata")["ownerReferences"] = []any{} }},
		{"deleting claim", func(s *inventory, _ *options) { at(s.claims[0], "metadata")["deletionTimestamp"] = "now" }},
		{"unbound claim", func(s *inventory, _ *options) { at(s.claims[0], "status")["phase"] = "Pending" }},
		{"missing volume", func(s *inventory, _ *options) { s.volumes = nil }},
		{"foreign volume binding", func(s *inventory, _ *options) { at(s.volumes["old-pv"], "spec", "claimRef")["uid"] = "foreign" }},
		{"deleting volume", func(s *inventory, _ *options) { at(s.volumes["old-pv"], "metadata")["deletionTimestamp"] = "now" }},
		{"existing fence", func(s *inventory, _ *options) {
			at(s.cluster, "metadata", "annotations")["cnpg.io/fencedInstances"] = `["other"]`
		}},
		{"active job", func(s *inventory, _ *options) { s.jobs = []object{{"status": object{"active": float64(1)}}} }},
		{"no backup", func(s *inventory, _ *options) { s.backups = nil }},
		{"stale backup", func(s *inventory, _ *options) {
			at(s.backups[0], "status")["stoppedAt"] = testNow.Add(-25 * time.Hour).Format(time.RFC3339)
		}},
		{"future backup", func(s *inventory, _ *options) {
			at(s.backups[0], "status")["stoppedAt"] = testNow.Add(time.Hour).Format(time.RFC3339)
		}},
		{"foreign backup", func(s *inventory, _ *options) { at(s.backups[0], "status", "pluginMetadata")["clusterUID"] = "foreign" }},
		{"failed backup", func(s *inventory, _ *options) { at(s.backups[0], "status")["phase"] = "failed" }},
		{"archiving unhealthy", func(s *inventory, _ *options) { at(s.cluster, "status")["conditions"] = []any{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, o := fixture(), testOptions()
			tc.change(&s, &o)
			if _, err := validate(s, o); err == nil {
				t.Fatal("unsafe repair accepted")
			}
		})
	}
}

// TestFixtureDecodesLikeKubernetes exercises kubectl's JSON numeric and list forms.
func TestFixtureDecodesLikeKubernetes(t *testing.T) {
	s := fixture()
	b, err := json.Marshal(s.cluster)
	if err != nil {
		t.Fatal(err)
	}
	var decoded object
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	s.cluster = decoded
	if _, err := validate(s, testOptions()); err != nil {
		t.Fatal(err)
	}
}

// TestPatchUsesOwnFieldManager keeps Flux from reverting the repair's writes.
// kustomize-controller takes over every field whose manager starts with
// "kubectl" and then removes what the Git source does not declare, so a fence
// written by a default kubectl patch disappears on the next reconciliation.
func TestPatchUsesOwnFieldManager(t *testing.T) {
	for _, kind := range []string{"pv", "cluster", "pvc"} {
		var got []string
		c := client{command: func(_ context.Context, args []string, _ []byte) ([]byte, error) {
			got = args
			return nil, nil
		}}
		if err := c.patch(context.Background(), kind, object{"metadata": map[string]any{"name": "n", "uid": "u"}}, nil); err != nil {
			t.Fatal(err)
		}
		managers := 0
		for _, arg := range got {
			if name, ok := strings.CutPrefix(arg, "--field-manager="); ok {
				managers++
				if name == "" || strings.HasPrefix(name, "kubectl") || name == "before-first-apply" || name == "kustomize-controller" {
					t.Fatalf("%s patch uses field manager %q, which Flux reverts", kind, name)
				}
			}
		}
		if managers != 1 {
			t.Fatalf("%s patch names %d field managers, want exactly 1: %v", kind, managers, got)
		}
	}
}
