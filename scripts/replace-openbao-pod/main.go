// Command replace-openbao-pod performs the two production changes of the
// single-server OpenBao replacement procedure (#4970). The protected
// Replace OpenBao Server workflow runs it only after both judges returned
// PROVEN for the named server.
//
//	replace-openbao-pod delete --server openbao-2 --expect-uid <uid> --expect-chart 0.30.7 \
//	  --expect-release 2.7.2 --timeout 10m
//	replace-openbao-pod deliver --timeout 5m
//
// delete removes the named server's pod only while it still has the UID the
// judged capture saw: the API server enforces that precondition, so a pod that
// was replaced after the capture is never deleted on another pod's evidence.
// The StatefulSet updates OnDelete, so deleting the pod is what starts the
// server from the live template; delete first proves that template runs the
// reviewed release, and that every claim the pod mounts is Bound. It then waits
// until a successor with a different UID mounts the same claims, unchanged.
// Whether the successor is healthy is the judges' call, not this command's.
//
// deliver asks External Secrets to re-read one canary secret from OpenBao and
// waits until that ExternalSecret reports a new, successful sync, which proves
// secrets are delivered again through the replaced cluster.
//
// It prints one closed status line and never a UID, secret or response body.
//
// Exit codes: 0 REPLACED or CONFIRMED, 1 REFUSED (nothing was deleted: the pod
// changed since the capture, the live template is not on the reviewed release,
// or its storage is not Bound), 2 UNVERIFIED (a read failed, the wait ran out,
// or the arguments are invalid).
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
	"slices"
	"sort"
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
	// statefulSet is the OpenBao StatefulSet, and the name of its server container.
	statefulSet = "openbao"
	// pollInterval spaces the reads of a wait; the wait itself is bounded by
	// --timeout.
	pollInterval = 5 * time.Second
)

var (
	serverName = regexp.MustCompile(`^openbao-[0-9]{1,2}$`)
	// podUID is the shape the API server gives every object UID.
	podUID  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	version = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

type podInfo struct {
	uid    string
	claims []string
}

type claimInfo struct {
	uid, phase string
}

// helmState is the part of the openbao HelmRelease the binding check reads.
type helmState struct {
	chart             string // the chart version Flux last attempted
	readyAtGeneration bool   // Ready=True for the current generation
}

// syncState is the part of the canary ExternalSecret the delivery check reads.
type syncState struct {
	syncedVersion string
	ready         string // "True", "False" or "" when there is no Ready condition
}

// cluster is everything the command asks of production, so the regressions
// can stand in for kubectl.
type cluster interface {
	// pod returns the pod's UID and the claims it mounts; a zero podInfo when
	// the pod does not exist.
	pod(ctx context.Context, name string) (podInfo, error)
	// claim returns a PersistentVolumeClaim's UID and phase.
	claim(ctx context.Context, name string) (claimInfo, error)
	// releaseState returns what Flux last applied for the openbao HelmRelease.
	releaseState(ctx context.Context) (helmState, error)
	// templateRelease returns the release the StatefulSet's live pod template
	// runs, from its openbao container image tag.
	templateRelease(ctx context.Context) (string, error)
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

// release normalises an image tag or a chart appVersion to a bare version.
func release(tag string) string { return strings.TrimPrefix(tag, "v") }

// reviewed is what the reviewed render pins: the chart version and the release
// it ships.
type reviewed struct{ chart, release string }

// retainedClaims are the claims a server must mount from the StatefulSet's
// claim templates: its Raft data and its audit log. Nothing else counts.
func retainedClaims(server string) []string {
	return []string{"audit-" + server, "data-" + server}
}

// sameClaims reports whether got names exactly the claims in want.
func sameClaims(got, want []string) bool {
	got = append([]string(nil), got...)
	sort.Strings(got)
	return slices.Equal(got, want)
}

func replace(ctx context.Context, k cluster, sleep func(context.Context, time.Duration) error, server, expect string, want reviewed) (int, string) {
	line := func(verdict, detail string) string {
		return fmt.Sprintf("OPENBAO_POD_REPLACEMENT=%s server=%s%s", verdict, server, detail)
	}
	current, err := k.pod(ctx, server)
	if err != nil {
		return 2, line("UNVERIFIED", " stage=pod-identity")
	}
	if current.uid != expect {
		return 1, line("REFUSED", " reason=pod-changed-since-capture")
	}
	// The pod restarts from the live template. Only a release Flux applied
	// from the reviewed chart, at its current generation and without failure,
	// binds that template to the reviewed values; the image tag then shows the
	// template carries the reviewed release.
	state, err := k.releaseState(ctx)
	if err != nil {
		return 2, line("UNVERIFIED", " stage=deployment-binding")
	}
	tag, err := k.templateRelease(ctx)
	if err != nil {
		return 2, line("UNVERIFIED", " stage=deployment-binding")
	}
	if state.chart != want.chart || !state.readyAtGeneration || release(tag) != want.release {
		return 1, line("REFUSED", " reason=template-not-on-reviewed-release")
	}
	// Deleting the pod keeps its claims; record them so the successor can be
	// shown to mount the same ones.
	expected := retainedClaims(server)
	if !sameClaims(current.claims, expected) {
		return 1, line("REFUSED", " reason=storage-not-mounted")
	}
	retained := make(map[string]string, len(expected))
	for _, name := range expected {
		c, err := k.claim(ctx, name)
		if err != nil {
			return 2, line("UNVERIFIED", " stage=storage")
		}
		if c.uid == "" || c.phase != "Bound" {
			return 1, line("REFUSED", " reason=storage-not-bound")
		}
		retained[name] = c.uid
	}
	switch err := k.deleteIfUID(ctx, server, expect); {
	case errors.Is(err, errPreconditionFailed):
		return 1, line("REFUSED", " reason=pod-changed-since-capture")
	case err != nil:
		return 2, line("UNVERIFIED", " stage=delete")
	}
	// A read can fail while the pod is being replaced; keep waiting and let
	// the deadline decide.
	err = wait(ctx, sleep, func() (bool, error) {
		next, err := k.pod(ctx, server)
		return err == nil && next.uid != "" && next.uid != expect, nil
	})
	if err != nil {
		return 2, line("UNVERIFIED", " stage=successor")
	}
	err = wait(ctx, sleep, func() (bool, error) {
		next, err := k.pod(ctx, server)
		if err != nil || next.uid == expect || !sameClaims(next.claims, expected) {
			return false, nil
		}
		for _, name := range expected {
			c, err := k.claim(ctx, name)
			if err != nil || c.uid != retained[name] || c.phase != "Bound" {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return 2, line("UNVERIFIED", " stage=storage-retained")
	}
	return 0, line("REPLACED", "")
}

func deliver(ctx context.Context, k cluster, sleep func(context.Context, time.Duration) error, now func() time.Time) (int, string) {
	before, err := k.canaryState(ctx)
	if err != nil {
		return 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=canary-read"
	}
	if err := k.requestSync(ctx, strconv.FormatInt(now().Unix(), 10)); err != nil {
		return 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=sync-request"
	}
	err = wait(ctx, sleep, func() (bool, error) {
		s, err := k.canaryState(ctx)
		if err != nil {
			return false, nil
		}
		// External Secrets records a new synced version only after a successful
		// sync with the changed annotation, so it proves this request was
		// answered from OpenBao. A failed read never records one.
		return s.syncedVersion != before.syncedVersion && s.ready == "True", nil
	})
	if err != nil {
		return 2, "OPENBAO_SECRET_DELIVERY=UNVERIFIED stage=sync"
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
		want := set.String("expect-release", "", "the release the reviewed chart pins")
		chart := set.String("expect-chart", "", "the reviewed chart version")
		if err := parse(set, args[1:]); err != nil {
			return invalid(err)
		}
		if !serverName.MatchString(*server) || !podUID.MatchString(*expect) || !version.MatchString(*want) || !version.MatchString(*chart) || *timeout <= 0 || *timeout > 30*time.Minute {
			return invalid(errors.New("--server, --expect-uid, --expect-release, --expect-chart and a --timeout of at most 30m are required"))
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		code, line = replace(ctx, k, sleep, *server, *expect, reviewed{chart: *chart, release: *want})
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

func (k kubectl) pod(ctx context.Context, name string) (podInfo, error) {
	b, _, err := k.output(ctx, nil, "get", "pod", name, "--ignore-not-found", "--output=json")
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return podInfo{}, err
	}
	return parsePod(b)
}

// parsePod returns the pod's UID and the claims mounted into its openbao
// container, the one container that keeps Raft data and the audit log. A claim
// in the pod spec that this container does not mount is not retained storage.
func parsePod(b []byte) (podInfo, error) {
	var p struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name         string `json:"name"`
				VolumeMounts []struct {
					Name string `json:"name"`
				} `json:"volumeMounts"`
			} `json:"containers"`
			Volumes []struct {
				Name                  string `json:"name"`
				PersistentVolumeClaim *struct {
					ClaimName string `json:"claimName"`
				} `json:"persistentVolumeClaim"`
			} `json:"volumes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return podInfo{}, err
	}
	if p.Metadata.UID == "" {
		return podInfo{}, errors.New("pod without a UID")
	}
	claimOf := map[string]string{}
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			if v.PersistentVolumeClaim.ClaimName == "" {
				return podInfo{}, errors.New("claim volume without a claim name")
			}
			claimOf[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
	}
	info := podInfo{uid: p.Metadata.UID}
	found := 0
	for _, c := range p.Spec.Containers {
		if c.Name != statefulSet {
			continue
		}
		found++
		seen := map[string]bool{}
		for _, m := range c.VolumeMounts {
			if claim, ok := claimOf[m.Name]; ok && !seen[claim] {
				seen[claim] = true
				info.claims = append(info.claims, claim)
			}
		}
	}
	if found != 1 {
		return podInfo{}, errors.New("not exactly one openbao container")
	}
	return info, nil
}

func (k kubectl) claim(ctx context.Context, name string) (claimInfo, error) {
	b, _, err := k.output(ctx, nil, "get", "persistentvolumeclaim", name, "--output=json")
	if err != nil {
		return claimInfo{}, err
	}
	var c struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return claimInfo{}, err
	}
	return claimInfo{uid: c.Metadata.UID, phase: c.Status.Phase}, nil
}

func (k kubectl) templateRelease(ctx context.Context) (string, error) {
	b, _, err := k.output(ctx, nil, "get", "statefulset", statefulSet, "--output=json")
	if err != nil {
		return "", err
	}
	return parseTemplateRelease(b)
}

// parseTemplateRelease reads the tag of the one container named openbao. An
// image pinned only by digest, or without a tag, is not evidence of a release.
func parseTemplateRelease(b []byte) (string, error) {
	var s struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return "", err
	}
	image := ""
	for _, c := range s.Spec.Template.Spec.Containers {
		if c.Name == statefulSet {
			if image != "" {
				return "", errors.New("more than one openbao container")
			}
			image = c.Image
		}
	}
	image, _, _ = strings.Cut(image, "@")
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash || colon == len(image)-1 {
		return "", errors.New("openbao image has no tag")
	}
	return image[colon+1:], nil
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
	if err != nil && preconditionRefused(errOut) {
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

// preconditionRefused tells a delete the API server refused for a UID
// mismatch from any other failure. The registry reports the mismatch as a
// Conflict naming the UID in the record; the storage layer as "Precondition
// failed".
func preconditionRefused(stderr []byte) bool {
	return bytes.Contains(stderr, []byte("does not match the UID in record")) || bytes.Contains(stderr, []byte("Precondition failed"))
}

func (k kubectl) releaseState(ctx context.Context) (helmState, error) {
	b, _, err := k.output(ctx, nil, "get", "helmrelease", statefulSet, "--output=json")
	if err != nil {
		return helmState{}, err
	}
	return parseHelmRelease(b)
}

// parseHelmRelease reads the chart version Flux last attempted and whether the
// release is Ready for its current generation; a stale or failed reconcile is
// not ready.
func parseHelmRelease(b []byte) (helmState, error) {
	var hr struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Status struct {
			ObservedGeneration    int64  `json:"observedGeneration"`
			LastAttemptedRevision string `json:"lastAttemptedRevision"`
			Conditions            []struct {
				Type               string `json:"type"`
				Status             string `json:"status"`
				ObservedGeneration int64  `json:"observedGeneration"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &hr); err != nil {
		return helmState{}, err
	}
	if hr.Metadata.Generation == 0 || hr.Status.LastAttemptedRevision == "" {
		return helmState{}, errors.New("helm release without a generation or attempted revision")
	}
	s := helmState{chart: hr.Status.LastAttemptedRevision}
	ready := 0
	for _, c := range hr.Status.Conditions {
		if c.Type == "Ready" {
			ready++
			s.readyAtGeneration = c.Status == "True" && c.ObservedGeneration == hr.Metadata.Generation
		}
	}
	if ready != 1 {
		return helmState{}, errors.New("not exactly one Ready condition")
	}
	s.readyAtGeneration = s.readyAtGeneration && hr.Status.ObservedGeneration == hr.Metadata.Generation
	return s, nil
}
