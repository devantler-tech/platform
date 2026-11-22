package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func dispatch() map[string]string {
	return map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_ACTOR": "devantler", "GITHUB_SHA": strings.Repeat("a", 40), "RUNNER_ENVIRONMENT": "github-hosted", "PROD_OPERATION": operation}
}

func toolGone(string) error { return fs.ErrNotExist }

func journal(mutate func(map[string]any)) string {
	record, _ := json.Marshal(map[string]any{"Holder": "38200854113.1", "Phase": "complete", "Source": strings.Repeat("b", 40)})
	o := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "uid": "journal-uid", "resourceVersion": "354671879",
			"annotations": map[string]any{holderKey: "38200854113.1"},
		},
		"data": map[string]any{journalKey: string(record)},
	}
	if mutate != nil {
		mutate(o)
	}
	b, _ := json.Marshal(o)
	return string(b)
}

var getArgs = []string{"--context=admin@prod", "--request-timeout=20s", "get", "configmap", name, "--namespace=" + namespace, "--ignore-not-found", "--output=json"}
var deleteArgs = []string{"--context=admin@prod", "--request-timeout=20s", "delete", "--raw", rawPath, "--filename=-"}

// fakeCluster answers the two fixed reads and the pinned delete like the API server.
type fakeCluster struct {
	object    string // "" when the journal is absent
	readErr   []error
	deleteErr error
	replaced  string // object present after a refused delete
	reads     int
	deletes   [][]byte
}

func (c *fakeCluster) run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	switch {
	case reflect.DeepEqual(args, getArgs):
		c.reads++
		if len(c.readErr) >= c.reads && c.readErr[c.reads-1] != nil {
			return nil, c.readErr[c.reads-1]
		}
		if len(stdin) != 0 {
			return nil, errors.New("a read sent a body")
		}
		return []byte(c.object), nil
	case reflect.DeepEqual(args, deleteArgs):
		c.deletes = append(c.deletes, stdin)
		if c.deleteErr != nil {
			if c.replaced != "" {
				c.object = c.replaced
			}
			return nil, c.deleteErr
		}
		c.object = ""
		return []byte(`{"kind":"Status","status":"Success"}`), nil
	default:
		return nil, errors.New("unreviewed kubectl call")
	}
}

func run(t *testing.T, env map[string]string, stat func(string) error, c *fakeCluster) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := execute(context.Background(), func(k string) string { return env[k] }, stat, c.run, &out)
	return code, out.String()
}

func TestDeletesTheCompletedJournalPinnedToWhatItRead(t *testing.T) {
	c := &fakeCluster{object: journal(nil)}
	code, out := run(t, dispatch(), toolGone, c)
	if code != 0 || out != "LONGHORN_CLIENT_JOURNAL=DELETED\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if len(c.deletes) != 1 || c.reads != 2 {
		t.Fatalf("expected one read, one delete and one confirming read, got reads=%d deletes=%d", c.reads, len(c.deletes))
	}
	var body struct {
		APIVersion, Kind string
		Preconditions    map[string]string
	}
	if err := json.Unmarshal(c.deletes[0], &body); err != nil {
		t.Fatal(err)
	}
	if body.APIVersion != "v1" || body.Kind != "DeleteOptions" ||
		!reflect.DeepEqual(body.Preconditions, map[string]string{"uid": "journal-uid", "resourceVersion": "354671879"}) {
		t.Fatalf("delete is not pinned to the read object: %s", c.deletes[0])
	}
}

func TestAnAbsentJournalIsAlreadyDone(t *testing.T) {
	c := &fakeCluster{}
	code, out := run(t, dispatch(), toolGone, c)
	if code != 0 || out != "LONGHORN_CLIENT_JOURNAL=ABSENT\n" || len(c.deletes) != 0 {
		t.Fatalf("code=%d out=%q deletes=%d", code, out, len(c.deletes))
	}
}

func TestNothingIsReadOutsideTheReviewedDispatch(t *testing.T) {
	for key, value := range map[string]string{
		"GITHUB_ACTIONS": "", "GITHUB_REPOSITORY": "fork/platform", "GITHUB_EVENT_NAME": "push",
		"GITHUB_REF": "refs/heads/feature", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_ACTOR": "someone",
		"RUNNER_ENVIRONMENT": "self-hosted", "PROD_OPERATION": "read-alert-routes", "GITHUB_SHA": "main",
	} {
		t.Run(key, func(t *testing.T) {
			env := dispatch()
			env[key] = value
			c := &fakeCluster{object: journal(nil)}
			code, out := run(t, env, toolGone, c)
			if code != 3 || !strings.HasPrefix(out, "LONGHORN_CLIENT_JOURNAL=UNKNOWN") || c.reads != 0 || len(c.deletes) != 0 {
				t.Fatalf("code=%d out=%q reads=%d deletes=%d", code, out, c.reads, len(c.deletes))
			}
		})
	}
}

func TestHoldsWhileTheRetiredToolIsStillOnMain(t *testing.T) {
	for label, stat := range map[string]func(string) error{
		"present":    func(string) error { return nil },
		"unreadable": func(string) error { return fs.ErrPermission },
	} {
		t.Run(label, func(t *testing.T) {
			c := &fakeCluster{object: journal(nil)}
			code, out := run(t, dispatch(), stat, c)
			if code == 0 || c.reads != 0 || len(c.deletes) != 0 {
				t.Fatalf("code=%d out=%q reads=%d deletes=%d", code, out, c.reads, len(c.deletes))
			}
		})
	}
}

func TestOnlyTheToolsOwnCompletedJournalIsDeleted(t *testing.T) {
	for label, mutate := range map[string]func(map[string]any){
		"unfinished": func(o map[string]any) {
			o["data"] = map[string]any{journalKey: `{"Holder":"38200854113.1","Phase":"restored"}`}
		},
		"malformed record": func(o map[string]any) { o["data"] = map[string]any{journalKey: "{"} },
		"missing record":   func(o map[string]any) { o["data"] = map[string]any{"other": "x"} },
		"extra record":     func(o map[string]any) { o["data"].(map[string]any)["extra"] = "x" },
		"binary data":      func(o map[string]any) { o["binaryData"] = map[string]any{"x": "eA=="} },
		"foreign holder": func(o map[string]any) {
			o["metadata"].(map[string]any)["annotations"] = map[string]any{holderKey: "1.1"}
		},
		"no holder": func(o map[string]any) { o["metadata"].(map[string]any)["annotations"] = map[string]any{} },
		"Flux-managed": func(o map[string]any) {
			o["metadata"].(map[string]any)["labels"] = map[string]any{"kustomize.toolkit.fluxcd.io/name": "infrastructure"}
		},
		"owned": func(o map[string]any) {
			o["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"kind": "X"}}
		},
		"finalized":         func(o map[string]any) { o["metadata"].(map[string]any)["finalizers"] = []any{"x"} },
		"another object":    func(o map[string]any) { o["metadata"].(map[string]any)["name"] = "other" },
		"another namespace": func(o map[string]any) { o["metadata"].(map[string]any)["namespace"] = "default" },
		"another kind":      func(o map[string]any) { o["kind"] = "Secret" },
		"no uid":            func(o map[string]any) { delete(o["metadata"].(map[string]any), "uid") },
		"no version":        func(o map[string]any) { delete(o["metadata"].(map[string]any), "resourceVersion") },
	} {
		t.Run(label, func(t *testing.T) {
			c := &fakeCluster{object: journal(mutate)}
			code, out := run(t, dispatch(), toolGone, c)
			if code != 1 || !strings.HasPrefix(out, "LONGHORN_CLIENT_JOURNAL=HOLD") || len(c.deletes) != 0 {
				t.Fatalf("code=%d out=%q deletes=%d", code, out, len(c.deletes))
			}
		})
	}
}

func TestAnUnreadableJournalIsUnknownAndUntouched(t *testing.T) {
	for label, c := range map[string]*fakeCluster{
		"read fails":    {object: journal(nil), readErr: []error{errors.New("timeout")}},
		"not JSON":      {object: "not json"},
		"confirm fails": {object: journal(nil), readErr: []error{nil, errors.New("timeout")}},
	} {
		t.Run(label, func(t *testing.T) {
			code, out := run(t, dispatch(), toolGone, c)
			if code != 3 || !strings.HasPrefix(out, "LONGHORN_CLIENT_JOURNAL=UNKNOWN") {
				t.Fatalf("code=%d out=%q", code, out)
			}
			if label != "confirm fails" && len(c.deletes) != 0 {
				t.Fatalf("deleted after an unreadable journal")
			}
		})
	}
}

func TestARefusedOrRacedDeleteIsNeverReportedAsDeleted(t *testing.T) {
	refused := &fakeCluster{object: journal(nil), deleteErr: errors.New("Conflict")}
	if code, out := run(t, dispatch(), toolGone, refused); code != 1 || !strings.Contains(out, "refused the pinned delete") {
		t.Fatalf("refused: code=%d out=%q", code, out)
	}
	replaced := &fakeCluster{object: journal(nil), deleteErr: errors.New("Conflict"), replaced: journal(func(o map[string]any) {
		o["metadata"].(map[string]any)["uid"] = "new-uid"
	})}
	if code, out := run(t, dispatch(), toolGone, replaced); code != 1 || !strings.Contains(out, "replaced") {
		t.Fatalf("replaced: code=%d out=%q", code, out)
	}
	// A lost response to a delete that did happen is still a confirmed absence.
	lost := &fakeCluster{object: journal(nil), deleteErr: errors.New("connection reset")}
	lost.replaced = ""
	lostRun := func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		out, err := lost.run(ctx, stdin, args...)
		if reflect.DeepEqual(args, deleteArgs) {
			lost.object = ""
		}
		return out, err
	}
	var out bytes.Buffer
	if code := execute(context.Background(), func(k string) string { return dispatch()[k] }, toolGone, lostRun, &out); code != 0 || out.String() != "LONGHORN_CLIENT_JOURNAL=DELETED\n" {
		t.Fatalf("lost response: code=%d out=%q", code, out.String())
	}
}

// The wrapper is the only way this program reaches production.
func TestTheWrapperNamesThisProgram(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", operation+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"operation: " + operation, "program: scripts/" + operation, `description: "Type exactly: ` + operation + `"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("wrapper lacks %q", want)
		}
	}
}
