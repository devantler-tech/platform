package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// fake stands in for kubectl. uids is the sequence of pod UIDs successive
// reads return; the last one repeats. Every pod mounts mounts; claims holds
// each claim before the delete and claimsAfter, when set, after it.
type fake struct {
	uids        []string
	uidErr      error
	mounts      []string
	claims      map[string]claimInfo
	claimsAfter map[string]claimInfo
	claimErr    error
	release     string
	releaseErr  error
	helm        helmState
	helmErr     error
	deleted     []string
	deleteErr   error
	states      []syncState
	stateErr    error
	syncErr     error
	syncMarker  string
	reads       int
}

// healthy is a fake whose pod passes every pre-delete check.
func healthy(uids ...string) *fake {
	return &fake{
		uids:    uids,
		mounts:  []string{"data-openbao-2", "audit-openbao-2"},
		claims:  map[string]claimInfo{"data-openbao-2": {"c-data", "Bound"}, "audit-openbao-2": {"c-audit", "Bound"}},
		release: "v2.7.2",
		helm:    helmState{chart: "0.30.7", readyAtGeneration: true},
	}
}

func (f *fake) pod(context.Context, string) (podInfo, error) {
	if f.uidErr != nil {
		return podInfo{}, f.uidErr
	}
	u := f.uids[0]
	if len(f.uids) > 1 {
		f.uids = f.uids[1:]
	}
	if u == "" {
		return podInfo{}, nil
	}
	return podInfo{uid: u, claims: f.mounts}, nil
}

func (f *fake) claim(_ context.Context, name string) (claimInfo, error) {
	if f.claimErr != nil {
		return claimInfo{}, f.claimErr
	}
	if f.deleted != nil && f.claimsAfter != nil {
		return f.claimsAfter[name], nil
	}
	return f.claims[name], nil
}

func (f *fake) templateRelease(context.Context) (string, error) { return f.release, f.releaseErr }

func (f *fake) releaseState(context.Context) (helmState, error) { return f.helm, f.helmErr }

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
	deleteArgs := []string{"delete", "--server", "openbao-2", "--expect-uid", oldUID, "--expect-chart", "0.30.7", "--expect-release", "2.7.2", "--timeout", "10m"}
	with := func(f *fake, change func(*fake)) *fake { change(f); return f }
	deleted := []string{"openbao-2@" + oldUID}
	refused := func(reason string) string {
		return "OPENBAO_POD_REPLACEMENT=REFUSED server=openbao-2 reason=" + reason
	}
	unverified := func(stage string) string {
		return "OPENBAO_POD_REPLACEMENT=UNVERIFIED server=openbao-2 stage=" + stage
	}
	cases := []struct {
		name    string
		f       *fake
		sleep   func(context.Context, time.Duration) error
		code    int
		line    string
		deleted []string
	}{
		{"replaced once a successor mounts the same storage", healthy(oldUID, oldUID, "", newUID), noSleep, 0,
			"OPENBAO_POD_REPLACEMENT=REPLACED server=openbao-2", deleted},
		{"an unprefixed tag is the same release", with(healthy(oldUID, newUID), func(f *fake) { f.release = "2.7.2" }), noSleep, 0,
			"OPENBAO_POD_REPLACEMENT=REPLACED server=openbao-2", deleted},
		{"refuses a pod replaced since the capture", healthy(newUID), noSleep, 1, refused("pod-changed-since-capture"), nil},
		{"refuses a pod that is gone", healthy(""), noSleep, 1, refused("pod-changed-since-capture"), nil},
		{"refuses when the API server's precondition fails", with(healthy(oldUID), func(f *fake) { f.deleteErr = errPreconditionFailed }), noSleep, 1,
			refused("pod-changed-since-capture"), nil},
		{"refuses a template still on the old release", with(healthy(oldUID), func(f *fake) { f.release = "v2.6.3" }), noSleep, 1,
			refused("template-not-on-reviewed-release"), nil},
		{"refuses a pod with no claims", with(healthy(oldUID), func(f *fake) { f.mounts = nil }), noSleep, 1, refused("storage-not-mounted"), nil},
		{"refuses a pod without its data claim", with(healthy(oldUID), func(f *fake) { f.mounts = []string{"audit-openbao-2"} }), noSleep, 1, refused("storage-not-mounted"), nil},
		{"refuses another server's claim", with(healthy(oldUID), func(f *fake) { f.mounts = []string{"audit-openbao-2", "data-openbao-1"} }), noSleep, 1, refused("storage-not-mounted"), nil},
		{"refuses a release on another chart", with(healthy(oldUID), func(f *fake) { f.helm.chart = "0.30.6" }), noSleep, 1, refused("template-not-on-reviewed-release"), nil},
		{"refuses a release not Ready at its generation", with(healthy(oldUID), func(f *fake) { f.helm.readyAtGeneration = false }), noSleep, 1, refused("template-not-on-reviewed-release"), nil},
		{"an unreadable release is unverified", with(healthy(oldUID), func(f *fake) { f.helmErr = errors.New("forbidden") }), noSleep, 2, unverified("deployment-binding"), nil},
		{"refuses a claim that is not Bound", with(healthy(oldUID), func(f *fake) { f.claims["audit-openbao-2"] = claimInfo{"c-audit", "Lost"} }), noSleep, 1,
			refused("storage-not-bound"), nil},
		{"refuses a missing claim", with(healthy(oldUID), func(f *fake) { delete(f.claims, "data-openbao-2") }), noSleep, 1, refused("storage-not-bound"), nil},
		{"an unreadable pod is unverified", with(healthy(oldUID), func(f *fake) { f.uidErr = errors.New("forbidden") }), noSleep, 2, unverified("pod-identity"), nil},
		{"an unreadable template is unverified", with(healthy(oldUID), func(f *fake) { f.releaseErr = errors.New("forbidden") }), noSleep, 2, unverified("deployment-binding"), nil},
		{"an unreadable claim is unverified", with(healthy(oldUID), func(f *fake) { f.claimErr = errors.New("forbidden") }), noSleep, 2, unverified("storage"), nil},
		{"a failed delete is unverified", with(healthy(oldUID), func(f *fake) { f.deleteErr = errors.New("timeout") }), noSleep, 2, unverified("delete"), nil},
		{"no successor before the deadline is unverified", healthy(oldUID, oldUID, ""), limitedSleep(3), 2, unverified("successor"), deleted},
		{"a successor on a new claim is unverified", with(healthy(oldUID, newUID), func(f *fake) {
			f.claimsAfter = map[string]claimInfo{"data-openbao-2": {"c-new", "Bound"}, "audit-openbao-2": {"c-audit", "Bound"}}
		}), limitedSleep(3), 2, unverified("storage-retained"), deleted},
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
		{"delete", "--server", "openbao-2", "--expect-uid", oldUID, "--expect-release", "v2.7.2", "--timeout", "10m"},
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
	if !reflect.DeepEqual(seen, map[string]int{"get": 5, "delete": 1, "annotate": 1}) {
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

func TestParsePodAndTemplate(t *testing.T) {
	volumes := `"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-openbao-2"}},{"name":"unseal-keys","secret":{"secretName":"openbao-unseal"}},{"name":"audit","persistentVolumeClaim":{"claimName":"audit-openbao-2"}},{"name":"scratch","persistentVolumeClaim":{"claimName":"scratch-openbao-2"}}]`
	// Only claims the openbao container mounts count; the sidecar's does not.
	p, err := parsePod([]byte(`{"metadata":{"uid":"` + oldUID + `"},"spec":{"containers":[{"name":"openbao","volumeMounts":[{"name":"audit"},{"name":"unseal-keys"},{"name":"data"},{"name":"data"}]},{"name":"sidecar","volumeMounts":[{"name":"scratch"}]}],` + volumes + `}}`))
	if err != nil || p.uid != oldUID || !reflect.DeepEqual(p.claims, []string{"audit-openbao-2", "data-openbao-2"}) {
		t.Fatalf("got %+v %v", p, err)
	}
	for _, bad := range []string{`not json`, `{"metadata":{}}`, `{"metadata":{"uid":"x"},"spec":{"volumes":[{"persistentVolumeClaim":{}}]}}`, `{"metadata":{"uid":"x"},"spec":{"containers":[{"name":"sidecar"}]}}`, `{"metadata":{"uid":"x"},"spec":{"containers":[{"name":"openbao"},{"name":"openbao"}]}}`} {
		if _, err := parsePod([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	sts := func(images ...string) []byte {
		var cs []string
		for i, image := range images {
			name := "openbao"
			if i > 0 && image == "sidecar:1" {
				name = "sidecar"
			}
			cs = append(cs, `{"name":"`+name+`","image":"`+image+`"}`)
		}
		return []byte(`{"spec":{"template":{"spec":{"containers":[` + strings.Join(cs, ",") + `]}}}}`)
	}
	for image, want := range map[string]string{
		"quay.io/openbao/openbao:2.7.2":                   "2.7.2",
		"registry:5000/openbao/openbao:v2.7.2@sha256:abc": "v2.7.2",
	} {
		if got, err := parseTemplateRelease(sts(image, "sidecar:1")); err != nil || got != want {
			t.Fatalf("%s: got %q %v", image, got, err)
		}
	}
	for _, bad := range [][]byte{sts("registry:5000/openbao/openbao"), sts("openbao/openbao@sha256:abc"), sts("openbao:"), sts(), sts("a:1", "b:2"), []byte(`nope`)} {
		if got, err := parseTemplateRelease(bad); err == nil {
			t.Fatalf("accepted %s as %q", bad, got)
		}
	}
}

func TestParseHelmRelease(t *testing.T) {
	hr := func(gen, observed, condGen int64, status string) []byte {
		return []byte(fmt.Sprintf(`{"metadata":{"generation":%d},"status":{"observedGeneration":%d,"lastAttemptedRevision":"0.30.7","conditions":[{"type":"Ready","status":%q,"observedGeneration":%d}]}}`, gen, observed, status, condGen))
	}
	for _, c := range []struct {
		doc   []byte
		ready bool
	}{
		{hr(4, 4, 4, "True"), true},
		{hr(5, 4, 4, "True"), false},
		{hr(5, 5, 4, "True"), false},
		{hr(4, 4, 4, "False"), false},
	} {
		s, err := parseHelmRelease(c.doc)
		if err != nil || s.chart != "0.30.7" || s.readyAtGeneration != c.ready {
			t.Fatalf("%s: got %+v %v", c.doc, s, err)
		}
	}
	for _, bad := range []string{`nope`, `{"metadata":{"generation":1},"status":{}}`, `{"metadata":{"generation":1},"status":{"lastAttemptedRevision":"0.30.7"}}`,
		`{"metadata":{"generation":1},"status":{"lastAttemptedRevision":"0.30.7","conditions":[{"type":"Ready","status":"True"},{"type":"Ready","status":"True"}]}}`} {
		if _, err := parseHelmRelease([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
