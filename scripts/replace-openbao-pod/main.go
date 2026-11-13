// Command replace-openbao-pod performs the two production changes of the
// single-server OpenBao replacement procedure (#4970). The protected
// Replace OpenBao Server workflow runs it only after both judges returned
// PROVEN for the named server.
//
//	replace-openbao-pod delete --server openbao-2 --expect-uid <uid> --timeout 10m
//	replace-openbao-pod deliver --timeout 5m
//
// delete removes the named server's pod only while it still has the UID the
// judged capture saw: the API server enforces that precondition, so a pod that
// was replaced after the capture is never deleted on another pod's evidence.
// The StatefulSet updates OnDelete, so deleting the pod is what starts the
// server on the reviewed release. The command then waits until a successor
// with a different UID exists. Whether the successor is healthy is the judges'
// call, not this command's.
//
// deliver asks External Secrets to re-read one canary secret from OpenBao and
// waits until that ExternalSecret reports a new, successful sync, which proves
// secrets are delivered again through the replaced cluster.
//
// It prints one closed status line and never a UID, secret or response body.
//
// Exit codes: 0 REPLACED or CONFIRMED, 1 REFUSED or FAILED (a definite
// finding), 2 UNVERIFIED (a read failed, the wait ran out, or the arguments
// are invalid).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	namespace   = "openbao"
	kubeContext = "admin@prod"
	// canary is an OpenBao-backed ExternalSecret in the openbao namespace that
	// nothing reads at startup, so a forced re-sync of it changes no workload.
	canary = "vault-snapshot-r2"
	// pollInterval spaces the reads of a wait; the wait itself is bounded by
	// --timeout.
	pollInterval = 5 * time.Second
)

var (
	serverName = regexp.MustCompile(`^openbao-[0-9]{1,2}$`)
	// podUID is the shape the API server gives every object UID.
	podUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// syncState is the part of the canary ExternalSecret the delivery check reads.
type syncState struct {
	syncedVersion string
	refreshed     time.Time
	ready         string // "True", "False" or "" when there is no Ready condition
}

// cluster is everything the command asks of production, so the regressions
// can stand in for kubectl.
type cluster interface {
	// uid returns the pod's UID, or "" when the pod does not exist.
	uid(ctx context.Context, name string) (string, error)
	// deleteIfUID deletes the pod only while it still has uid.
	deleteIfUID(ctx context.Context, name, uid string) error
	canaryState(ctx context.Context) (syncState, error)
	requestSync(ctx context.Context, marker string) error
}

// errPreconditionFailed is a delete the API server refused because the pod no
// longer has the expected UID.
var errPreconditionFailed = errors.New("precondition failed")

// wait calls check until it reports done, an error, or the context ends.
func wait(ctx context.Context, sleep func(context.Context, time.Duration) error, check func() (bool, error)) error {
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func replace(ctx context.Context, k cluster, sleep func(context.Context, time.Duration) error, server, expect string) (int, string) {
	line := func(verdict, detail string) string {
		return fmt.Sprintf("OPENBAO_POD_REPLACEMENT=%s server=%s%s", verdict, server, detail)
	}
	current, err := k.uid(ctx, server)
	if err != nil {
		return 2, line("UNVERIFIED", " stage=pod-identity")
	}
	if current != expect {
		return 1, line("REFUSED", " reason=pod-changed-since-capture")
	}
	switch err := k.deleteIfUID(ctx, server, expect); {
	case errors.Is(err, errPreconditionFailed):
		return 1, line("REFUSED", " reason=pod-changed-since-capture")
	case err != nil:
		return 2, line("UNVERIFIED", " stage=delete")
	}
	err = wait(ctx, sleep, func() (bool, error) {
		next, err := k.uid(ctx, server)
		// A read can fail while the pod is being replaced; keep waiting and
		// let the deadline decide.
		return err == nil && next != "" && next != expect, nil
	})
	if err != nil {
		return 2, line("UNVERIFIED", " stage=successor")
	}
	return 0, line("REPLACED", "")
}

func deliver(ctx context.Context, k cluster, sleep func(context.Context, time.Duration) error, now func() time.Time) (int, string) {
	before, err := k.canaryState(ctx)
	if err != nil {
		return 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=canary-read"
	}
	// Whole seconds, because the status reports its refresh time in seconds.
	requested := now().UTC().Truncate(time.Second)
	if err := k.requestSync(ctx, strconv.FormatInt(requested.Unix(), 10)); err != nil {
		return 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=sync-request"
	}
	var after syncState
	err = wait(ctx, sleep, func() (bool, error) {
		s, err := k.canaryState(ctx)
		if err != nil {
			return false, nil
		}
		after = s
		// A new synced version proves the controller acted on this request;
		// the refresh time proves it read OpenBao after the request.
		return s.syncedVersion != before.syncedVersion && !s.refreshed.Before(requested) && s.ready != "", nil
	})
	if err != nil {
		return 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=sync"
	}
	if after.ready != "True" {
		return 1, "OPENBAO_SECRET_DELIVERY=FAILED"
	}
	return 0, "OPENBAO_SECRET_DELIVERY=CONFIRMED"
}

// onceValue refuses a second Set, so a flag given twice is an error rather
// than a quiet swap of the target.
type onceValue struct {
	flag.Value
	name string
	set  bool
}

func (v *onceValue) Set(value string) error {
	if v.set {
		return fmt.Errorf("--%s was given more than once", v.name)
	}
	v.set = true
	return v.Value.Set(value)
}

func parse(set *flag.FlagSet, args []string) error {
	set.SetOutput(io.Discard)
	set.VisitAll(func(f *flag.Flag) { f.Value = &onceValue{Value: f.Value, name: f.Name} })
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("no positional arguments are accepted")
	}
	return nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, k cluster, sleep func(context.Context, time.Duration) error, now func() time.Time) int {
	invalid := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "replace-openbao-pod:", err)
		return 2
	}
	if len(args) == 0 {
		return invalid(errors.New("usage: replace-openbao-pod delete|deliver [flags]"))
	}
	set := flag.NewFlagSet(args[0], flag.ContinueOnError)
	timeout := set.Duration("timeout", 0, "longest wait for the change to take effect")
	var code int
	var line string
	switch args[0] {
	case "delete":
		server := set.String("server", "", "the one server whose pod to replace")
		expect := set.String("expect-uid", "", "the pod UID the judged capture saw")
		if err := parse(set, args[1:]); err != nil {
			return invalid(err)
		}
		if !serverName.MatchString(*server) || !podUID.MatchString(*expect) || *timeout <= 0 || *timeout > 30*time.Minute {
			return invalid(errors.New("--server, --expect-uid and a --timeout of at most 30m are required"))
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		code, line = replace(ctx, k, sleep, *server, *expect)
	case "deliver":
		if err := parse(set, args[1:]); err != nil {
			return invalid(err)
		}
		if *timeout <= 0 || *timeout > 30*time.Minute {
			return invalid(errors.New("a --timeout of at most 30m is required"))
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		code, line = deliver(ctx, k, sleep, now)
	default:
		return invalid(fmt.Errorf("unknown step %q", args[0]))
	}
	_, _ = fmt.Fprintln(stdout, line)
	return code
}

// kubectl is the production cluster, reached with the protected kubeconfig.
type kubectl struct{}

func (kubectl) args(rest ...string) []string {
	return append([]string{"--context=" + kubeContext, "--request-timeout=20s", "--namespace=" + namespace}, rest...)
}

// output runs kubectl and returns stdout, plus stderr only for the caller to
// classify; neither is ever printed.
func (k kubectl) output(ctx context.Context, stdin []byte, rest ...string) ([]byte, []byte, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "kubectl", k.args(rest...)...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

func (k kubectl) uid(ctx context.Context, name string) (string, error) {
	b, _, err := k.output(ctx, nil, "get", "pod", name, "--ignore-not-found", "--output=jsonpath={.metadata.uid}")
	return strings.TrimSpace(string(b)), err
}

// deleteIfUID sends the delete with a UID precondition. kubectl has no flag
// for one, so the request goes through --raw with the DeleteOptions body.
func (k kubectl) deleteIfUID(ctx context.Context, name, uid string) error {
	body, err := json.Marshal(map[string]any{
		"kind":          "DeleteOptions",
		"apiVersion":    "v1",
		"preconditions": map[string]string{"uid": uid},
	})
	if err != nil {
		return err
	}
	path := "/api/v1/namespaces/" + namespace + "/pods/" + name
	_, errOut, err := k.output(ctx, body, "delete", "--raw="+path, "--filename=-")
	if err != nil && bytes.Contains(errOut, []byte("Precondition failed")) {
		return errPreconditionFailed
	}
	return err
}

func (k kubectl) canaryState(ctx context.Context) (syncState, error) {
	b, _, err := k.output(ctx, nil, "get", "externalsecret", canary, "--output=json")
	if err != nil {
		return syncState{}, err
	}
	return parseCanary(b)
}

func parseCanary(b []byte) (syncState, error) {
	var es struct {
		Status struct {
			SyncedResourceVersion string `json:"syncedResourceVersion"`
			RefreshTime           string `json:"refreshTime"`
			Conditions            []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &es); err != nil {
		return syncState{}, err
	}
	s := syncState{syncedVersion: es.Status.SyncedResourceVersion}
	if es.Status.RefreshTime != "" {
		t, err := time.Parse(time.RFC3339, es.Status.RefreshTime)
		if err != nil {
			return syncState{}, err
		}
		s.refreshed = t
	}
	for _, c := range es.Status.Conditions {
		if c.Type == "Ready" {
			if s.ready != "" {
				return syncState{}, errors.New("more than one Ready condition")
			}
			s.ready = c.Status
		}
	}
	return s, nil
}

// requestSync changes the force-sync annotation, which External Secrets reads
// as a request to sync now.
func (k kubectl) requestSync(ctx context.Context, marker string) error {
	_, _, err := k.output(ctx, nil, "annotate", "externalsecret", canary, "force-sync="+marker, "--overwrite")
	return err
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, kubectl{}, sleepCtx, time.Now))
}
