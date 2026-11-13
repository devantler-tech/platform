package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	oldUID = "11111111-2222-4333-8444-555555555555"
	newUID = "99999999-8888-4777-8666-555555555555"
)

// fake stands in for kubectl. uids is the sequence of UIDs successive reads
// return; the last one repeats.
type fake struct {
	uids       []string
	uidErr     error
	deleted    []string
	deleteErr  error
	states     []syncState
	stateErr   error
	syncErr    error
	syncMarker string
	reads      int
}

func (f *fake) uid(context.Context, string) (string, error) {
	if f.uidErr != nil {
		return "", f.uidErr
	}
	u := f.uids[0]
	if len(f.uids) > 1 {
		f.uids = f.uids[1:]
	}
	return u, nil
}

func (f *fake) deleteIfUID(_ context.Context, name, uid string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, name+"@"+uid)
	return nil
}

func (f *fake) canaryState(context.Context) (syncState, error) {
	f.reads++
	if f.stateErr != nil {
		return syncState{}, f.stateErr
	}
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return s, nil
}

func (f *fake) requestSync(_ context.Context, marker string) error {
	f.syncMarker = marker
	return f.syncErr
}

func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// limitedSleep ends the wait after n polls, standing in for the deadline.
func limitedSleep(n int) func(context.Context, time.Duration) error {
	return func(context.Context, time.Duration) error {
		n--
		if n < 0 {
			return context.DeadlineExceeded
		}
		return nil
	}
}

var clock = time.Date(2026, 11, 13, 14, 0, 0, 500, time.UTC)

func runWith(t *testing.T, f *fake, sleep func(context.Context, time.Duration) error, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut, f, sleep, func() time.Time { return clock })
	return code, strings.TrimSpace(out.String()), errOut.String()
}

func TestDelete(t *testing.T) {
	deleteArgs := []string{"delete", "--server", "openbao-2", "--expect-uid", oldUID, "--timeout", "10m"}
	cases := []struct {
		name    string
		f       *fake
		sleep   func(context.Context, time.Duration) error
		code    int
		line    string
		deleted []string
	}{
		{"replaced once a successor appears", &fake{uids: []string{oldUID, oldUID, "", newUID}}, noSleep, 0,
			"OPENBAO_POD_REPLACEMENT=REPLACED server=openbao-2", []string{"openbao-2@" + oldUID}},
		{"refuses a pod replaced since the capture", &fake{uids: []string{newUID}}, noSleep, 1,
			"OPENBAO_POD_REPLACEMENT=REFUSED server=openbao-2 reason=pod-changed-since-capture", nil},
		{"refuses a pod that is gone", &fake{uids: []string{""}}, noSleep, 1,
			"OPENBAO_POD_REPLACEMENT=REFUSED server=openbao-2 reason=pod-changed-since-capture", nil},
		{"refuses when the API server's precondition fails", &fake{uids: []string{oldUID}, deleteErr: errPreconditionFailed}, noSleep, 1,
			"OPENBAO_POD_REPLACEMENT=REFUSED server=openbao-2 reason=pod-changed-since-capture", nil},
		{"an unreadable pod is unverified", &fake{uidErr: errors.New("forbidden")}, noSleep, 2,
			"OPENBAO_POD_REPLACEMENT=UNVERIFIED server=openbao-2 stage=pod-identity", nil},
		{"a failed delete is unverified", &fake{uids: []string{oldUID}, deleteErr: errors.New("timeout")}, noSleep, 2,
			"OPENBAO_POD_REPLACEMENT=UNVERIFIED server=openbao-2 stage=delete", nil},
		{"no successor before the deadline is unverified", &fake{uids: []string{oldUID, oldUID, ""}}, limitedSleep(3), 2,
			"OPENBAO_POD_REPLACEMENT=UNVERIFIED server=openbao-2 stage=successor", []string{"openbao-2@" + oldUID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, line, _ := runWith(t, c.f, c.sleep, deleteArgs...)
			if code != c.code || line != c.line || !reflect.DeepEqual(c.f.deleted, c.deleted) {
				t.Fatalf("got %d %q deleted=%v", code, line, c.f.deleted)
			}
			if strings.Contains(line, oldUID) || strings.Contains(line, newUID) {
				t.Fatal("a pod UID reached the log")
			}
		})
	}
}

func TestDeliver(t *testing.T) {
	before := syncState{syncedVersion: "1-a", ready: "True"}
	fresh := syncState{syncedVersion: "1-b", ready: "True"}
	cases := []struct {
		name  string
		f     *fake
		sleep func(context.Context, time.Duration) error
		code  int
		line  string
	}{
		{"confirmed after a new successful sync", &fake{states: []syncState{before, before, fresh}}, noSleep, 0, "OPENBAO_SECRET_DELIVERY=CONFIRMED"},
		{"a failed sync never confirms", &fake{states: []syncState{before, {syncedVersion: "1-a", ready: "False"}}}, limitedSleep(2), 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=sync"},
		{"an unchanged version is not this request", &fake{states: []syncState{before, {syncedVersion: "1-a", ready: "True"}}}, limitedSleep(2), 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=sync"},
		{"an unreadable canary is unverified", &fake{stateErr: errors.New("forbidden")}, noSleep, 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=canary-read"},
		{"a refused sync request is unverified", &fake{states: []syncState{before}, syncErr: errors.New("forbidden")}, noSleep, 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=sync-request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, line, _ := runWith(t, c.f, c.sleep, "deliver", "--timeout", "5m")
			if code != c.code || line != c.line {
				t.Fatalf("got %d %q", code, line)
			}
		})
	}
	f := &fake{states: []syncState{before, fresh}}
	runWith(t, f, noSleep, "deliver", "--timeout", "5m")
	if f.syncMarker != "1794578400" {
		t.Fatalf("sync marker %q is not the request time", f.syncMarker)
	}
}

func TestInvalidArgumentsChangeNothing(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"restart"},
		{"delete", "--server", "openbao-2", "--timeout", "10m"},
		{"delete", "--server", "vault-0", "--expect-uid", oldUID, "--timeout", "10m"},
		{"delete", "--server", "openbao-2", "--expect-uid", "uid-2", "--timeout", "10m"},
		{"delete", "--server", "openbao-2", "--expect-uid", oldUID},
		{"delete", "--server", "openbao-2", "--expect-uid", oldUID, "--timeout", "2h"},
		{"delete", "--server", "openbao-2", "--server", "openbao-1", "--expect-uid", oldUID, "--timeout", "10m"},
		{"delete", "--server", "openbao-2", "--expect-uid", oldUID, "--timeout", "10m", "extra"},
		{"deliver"},
		{"deliver", "--timeout", "5m", "--server", "openbao-2"},
		{"delete", "-h"},
	} {
		f := &fake{uids: []string{oldUID}, states: []syncState{{}}}
		code, line, _ := runWith(t, f, noSleep, args...)
		if code != 2 || line != "" || f.deleted != nil || f.syncMarker != "" {
			t.Fatalf("%v: got %d %q deleted=%v", args, code, line, f.deleted)
		}
	}
}

func TestParseCanary(t *testing.T) {
	s, err := parseCanary([]byte(`{"status":{"syncedResourceVersion":"1-b","refreshTime":"2026-11-13T14:00:00Z","conditions":[{"type":"Ready","status":"True","reason":"SecretSynced"}]}}`))
	if err != nil || s.syncedVersion != "1-b" || s.ready != "True" {
		t.Fatalf("got %+v %v", s, err)
	}
	for _, bad := range []string{
		`not json`,
		`{"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Ready","status":"False"}]}}`,
	} {
		if _, err := parseCanary([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// The command may read pods, delete one pod behind a UID precondition and
// annotate the canary; nothing else reaches kubectl.
func TestKubectlVerbs(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := regexp.MustCompile(`k\.output\(ctx, (?:nil|body), "([a-z-]+)"`).FindAllStringSubmatch(string(b), -1)
	seen := map[string]int{}
	for _, c := range calls {
		seen[c[1]]++
	}
	if !reflect.DeepEqual(seen, map[string]int{"get": 2, "delete": 1, "annotate": 1}) {
		t.Fatalf("kubectl verbs changed: %v", seen)
	}
	src := string(b)
	for _, want := range []string{
		`"preconditions": map[string]string{"uid": uid}`,
		`"annotate", "externalsecret", canary, "force-sync="+marker, "--overwrite"`,
		`canary = "vault-snapshot-r2"`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("missing %s", want)
		}
	}
}

// Both ways the API server reports a UID mismatch are a refusal; anything
// else, including a pod that is already gone, is not.
func TestPreconditionRefusal(t *testing.T) {
	for _, stderr := range []string{
		`Error from server (Conflict): Operation cannot be fulfilled on Pod "openbao-2": the UID in the precondition (` + oldUID + `) does not match the UID in record (` + newUID + `). The object might have been deleted and then recreated`,
		`Error from server (Conflict): Precondition failed: UID in precondition: ` + oldUID + `, UID in object meta: ` + newUID,
	} {
		if !preconditionRefused([]byte(stderr)) {
			t.Fatalf("not recognised as a refusal: %s", stderr)
		}
	}
	for _, stderr := range []string{`Error from server (NotFound): pods "openbao-2" not found`, `Unable to connect to the server: i/o timeout`, ``} {
		if preconditionRefused([]byte(stderr)) {
			t.Fatalf("taken for a refusal: %s", stderr)
		}
	}
}
