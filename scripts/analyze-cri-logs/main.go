package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxFileBytes  = 8 << 20
	maxTotalBytes = 64 << 20
	maxLineBytes  = 1 << 20
	maxPending    = 10000
	maxSamples    = 1000
)

type inputs []string

func (v *inputs) String() string     { return "private node-label=path inputs" }
func (v *inputs) Set(s string) error { *v = append(*v, s); return nil }

type nodeStats struct {
	Label                                                                           string
	Lines, Parsed, Malformed, Unsupported, Starts, Returns, Pairs, FailedPairs      int
	OverlappingStarts, AmbiguousReturns, OrphanReturns, PendingStarts, OrderingGaps int
	QuarantinedStarts                                                               int
	OutsideWindow, WindowPairs, SlowPairs                                           int
	Earliest, Latest                                                                string
	WindowBracketed                                                                 bool
	SHA256                                                                          string
}
type sample struct {
	Node, Method, TargetHash, PodUID, Outcome string
	Start, End                                time.Time
	Milliseconds                              int64
}
type report struct {
	Status, Scope, CorootAttribution string
	From, To                         time.Time
	SlowThreshold                    string
	Nodes                            []nodeStats
	Samples                          []sample
	SamplesTruncated                 bool
}
type operation struct{ method, key, uid, stage string }
type pendingCall struct {
	start     time.Time
	count     int
	ambiguous bool
	op        operation
}

var (
	labelPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)
	logTime      = regexp.MustCompile(`^time=("(?:\\.|[^"\\])*"|[^\s]+)`)
	logMessage   = regexp.MustCompile(`(?:^|\s)msg=("(?:\\.|[^"\\])*")`)
	pullPattern  = regexp.MustCompile(`^PullImage ("(?:\\.|[^"\\])*")(.*)$`)
	uidPattern   = regexp.MustCompile(`(?:\buid:"|\bUid:)([a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12})`)
	criPattern   = regexp.MustCompile(`^(?:Version|RunPodSandbox|StopPodSandbox|RemovePodSandbox|PodSandboxStatus|ListPodSandbox|CreateContainer|StartContainer|StopContainer|RemoveContainer|ListContainers|ContainerStatus|UpdateContainerResources|ReopenContainerLog|ExecSync|Exec|Attach|PortForward|ContainerStats|ListContainerStats|PodSandboxStats|ListPodSandboxStats|UpdateRuntimeConfig|Status|CheckpointContainer|GetContainerEvents|ListMetricDescriptors|ListPodSandboxMetrics|RuntimeConfig|UpdatePodSandboxResources|ListImages|ImageStatus|PullImage|RemoveImage|ImageFsInfo)\b`)
)

func envelope(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "time=") {
		return "", raw
	}
	if prefix, rest, ok := strings.Cut(raw, ": "); ok && (strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, "time=")) {
		return prefix, rest
	}
	return "", raw
}

// Only structured envelope timestamps count. A timestamp in a message cannot
// turn a malformed record into timing evidence.
func parseLine(raw string) (time.Time, string, error) {
	_, raw = envelope(raw)
	var stamp, msg string
	if strings.HasPrefix(raw, "{") {
		var fields struct{ Time, Msg string }
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			return time.Time{}, "", errors.New("malformed JSON")
		}
		stamp, msg = fields.Time, fields.Msg
	} else {
		t, m := logTime.FindStringSubmatch(raw), logMessage.FindStringSubmatch(raw)
		if len(t) != 2 || len(m) != 2 {
			return time.Time{}, "", errors.New("unrecognized structured envelope")
		}
		stamp = t[1]
		var err error
		if strings.HasPrefix(stamp, `"`) {
			stamp, err = strconv.Unquote(stamp)
			if err != nil {
				return time.Time{}, "", err
			}
		}
		msg, err = strconv.Unquote(m[1])
		if err != nil {
			return time.Time{}, "", err
		}
	}
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || msg == "" {
		return time.Time{}, "", errors.New("missing or invalid envelope fields")
	}
	return t, msg, nil
}

func parseOperation(msg string) (operation, bool) {
	var op operation
	var suffix string
	if m := pullPattern.FindStringSubmatch(msg); len(m) == 3 {
		op.method, op.key, suffix = "PullImage", "PullImage "+m[1], m[2]
	} else if strings.HasPrefix(msg, "RunPodSandbox for ") {
		op.method = "RunPodSandbox"
		key := msg
		for _, marker := range []string{" returns", " failed", " error"} {
			if i := strings.Index(key, marker); i >= 0 {
				suffix, key = key[i:], key[:i]
			}
		}
		uid := uidPattern.FindStringSubmatch(key)
		if len(uid) != 2 {
			return operation{}, false
		}
		op.key, op.uid = key, uid[1]
	} else {
		return operation{}, false
	}
	switch {
	case suffix == "":
		op.stage = "start"
	case strings.HasPrefix(suffix, " failed"), strings.HasPrefix(suffix, " error"), strings.HasPrefix(suffix, " returns error"):
		op.stage = "failure"
	case op.method == "RunPodSandbox" && strings.HasPrefix(suffix, " returns sandbox id "),
		op.method == "PullImage" && strings.HasPrefix(suffix, " returns image reference "):
		op.stage = "success"
	default:
		return operation{}, false
	}
	return op, true
}

func analyze(label string, data io.Reader, from, to time.Time, slow time.Duration, r *report) (nodeStats, error) {
	n := nodeStats{Label: label}
	pending := map[string]pendingCall{}
	quarantined := map[string]bool{}
	invalidate := func() {
		for key, p := range pending {
			n.PendingStarts += p.count
			quarantined[key] = true
		}
		clear(pending)
	}
	var earliest, latest, previous time.Time
	var nodePrefix string
	var havePrefix bool
	scanner := bufio.NewScanner(data)
	scanner.Buffer(make([]byte, 65536), maxLineBytes)
	for scanner.Scan() {
		n.Lines++
		when, msg, err := parseLine(scanner.Text())
		if err != nil {
			n.Malformed++
			invalidate()
			continue
		}
		prefix, _ := envelope(scanner.Text())
		if havePrefix && prefix != nodePrefix {
			return n, errors.New("mixed node prefixes or envelope styles")
		}
		nodePrefix, havePrefix = prefix, true
		n.Parsed++
		if earliest.IsZero() || when.Before(earliest) {
			earliest = when
		}
		if latest.IsZero() || when.After(latest) {
			latest = when
		}
		if !previous.IsZero() && when.Before(previous) {
			n.OrderingGaps++
			invalidate()
		}
		previous = when
		inWindow := !when.Before(from) && !when.After(to)
		if !inWindow {
			n.OutsideWindow++
		}
		op, ok := parseOperation(msg)
		if !ok {
			if criPattern.MatchString(msg) {
				n.Unsupported++
			}
			// An unrecognized record for a pairable method may be a lost
			// terminal or retry start; do not pair across that uncertainty.
			if strings.HasPrefix(msg, "RunPodSandbox ") || strings.HasPrefix(msg, "PullImage ") {
				invalidate()
			}
			continue
		}
		p := pending[op.key]
		if op.stage == "start" {
			n.Starts++
			if quarantined[op.key] {
				n.QuarantinedStarts++
				continue
			}
			if p.count == 0 {
				p = pendingCall{start: when, op: op}
			} else {
				n.OverlappingStarts++
				p.ambiguous = true
			}
			p.count++
			pending[op.key] = p
			if len(pending)+len(quarantined) > maxPending || p.count > maxPending {
				return n, errors.New("pending-operation limit exceeded")
			}
			continue
		}
		n.Returns++
		if quarantined[op.key] {
			n.AmbiguousReturns++
			continue
		}
		if p.count == 0 {
			n.OrphanReturns++
			continue
		}
		if p.ambiguous {
			n.AmbiguousReturns++
			p.count--
			if p.count == 0 {
				delete(pending, op.key)
			} else {
				pending[op.key] = p
			}
			continue
		}
		delete(pending, op.key)
		if op.stage == "failure" {
			n.FailedPairs++
		} else {
			n.Pairs++
		}
		if inWindow {
			n.WindowPairs++
			d := when.Sub(p.start)
			if d >= slow {
				n.SlowPairs++
			}
			if d >= slow || op.stage == "failure" {
				if len(r.Samples) < maxSamples {
					h := sha256.Sum256([]byte(op.key))
					outcome := "success"
					if op.stage == "failure" {
						outcome = "failure"
					}
					r.Samples = append(r.Samples, sample{Node: label, Method: op.method, TargetHash: hex.EncodeToString(h[:]), PodUID: op.uid, Outcome: outcome, Start: p.start.UTC(), End: when.UTC(), Milliseconds: d.Milliseconds()})
				} else {
					r.SamplesTruncated = true
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return n, errors.New("log line or read limit exceeded")
	}
	for _, p := range pending {
		n.PendingStarts += p.count
	}
	if !earliest.IsZero() {
		n.Earliest, n.Latest = earliest.UTC().Format(time.RFC3339Nano), latest.UTC().Format(time.RFC3339Nano)
		n.WindowBracketed = !earliest.After(from) && !latest.Before(to)
	}
	return n, nil
}

func hasGaps(n nodeStats) bool {
	return n.Malformed+n.Unsupported+n.OverlappingStarts+n.AmbiguousReturns+n.OrphanReturns+n.PendingStarts+n.OrderingGaps+n.QuarantinedStarts > 0 || !n.WindowBracketed
}

func run(args []string, stdout io.Writer) int {
	fail := func(message string) int { fmt.Fprintln(stdout, "ANALYSIS=REFUSED "+message); return 2 }
	fs := flag.NewFlagSet("analyze-cri-logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // Flag errors can contain raw paths or values.
	var logs inputs
	var experimental bool
	var start, end, output string
	var slow time.Duration
	fs.BoolVar(&experimental, "experimental", false, "explicitly enable this offline diagnostic")
	fs.StringVar(&start, "from", "", "window start RFC3339")
	fs.StringVar(&end, "to", "", "window end RFC3339")
	fs.StringVar(&output, "output", "", "new private report file")
	fs.DurationVar(&slow, "slow", 500*time.Millisecond, "sample threshold, not an SLO")
	fs.Var(&logs, "input", "repeat node-label=private-log-path")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fail("invalid arguments")
	}
	if !experimental {
		return fail("experimental opt-in required")
	}
	from, err1 := time.Parse(time.RFC3339Nano, start)
	to, err2 := time.Parse(time.RFC3339Nano, end)
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > 7*24*time.Hour || slow <= 0 || output == "" || len(logs) == 0 || len(logs) > 20 {
		return fail("invalid window, threshold, output or input count")
	}
	targets := map[string]string{}
	for _, entry := range logs {
		label, path, ok := strings.Cut(entry, "=")
		if !ok || !labelPattern.MatchString(label) || path == "" || targets[label] != "" {
			return fail("invalid or duplicate node label")
		}
		targets[label] = path
	}
	labels := make([]string, 0, len(targets))
	for label := range targets {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	r := report{Status: "COMPLETE", Scope: "finite CRI records; not complete request coverage", CorootAttribution: "UNPROVEN", From: from.UTC(), To: to.UTC(), SlowThreshold: slow.String(), Nodes: []nodeStats{}, Samples: []sample{}}
	var total int64
	for _, label := range labels {
		// Nonblocking/no-follow avoids hanging on a FIFO or following an input
		// symlink before we can verify the opened file. Operators use macOS/Linux.
		f, err := os.OpenFile(targets[label], os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fail("cannot open private input " + label)
		}
		before, err := f.Stat()
		if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > maxFileBytes {
			f.Close()
			return fail("input must be a private bounded regular file: " + label)
		}
		total += before.Size()
		if total > maxTotalBytes {
			f.Close()
			return fail("total input limit exceeded")
		}
		h := sha256.New()
		limited := &io.LimitedReader{R: f, N: maxFileBytes + 1}
		n, analyzeErr := analyze(label, io.TeeReader(limited, h), from, to, slow, &r)
		after, statErr := f.Stat()
		closeErr := f.Close()
		if analyzeErr != nil {
			return fail("input analysis refused: " + label + " (" + analyzeErr.Error() + ")")
		}
		if statErr != nil || closeErr != nil || limited.N == 0 {
			return fail("input analysis failed or exceeded its limit: " + label)
		}
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return fail("input changed during analysis: " + label)
		}
		n.SHA256 = hex.EncodeToString(h.Sum(nil))
		if hasGaps(n) {
			r.Status = "GAPS"
		}
		r.Nodes = append(r.Nodes, n)
	}
	if r.SamplesTruncated {
		r.Status = "GAPS"
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fail("report encoding failed")
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail("output must be a new private file")
	}
	_, writeErr := f.Write(append(b, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(output)
		return fail("report write failed")
	}
	var pairs, slowPairs, failures int
	for _, n := range r.Nodes {
		pairs += n.Pairs
		slowPairs += n.SlowPairs
		failures += n.FailedPairs
	}
	fmt.Fprintf(stdout, "ANALYSIS=%s nodes=%d successful_pairs=%d slow_window_pairs=%d failed_pairs=%d coroot_attribution=UNPROVEN\n", r.Status, len(r.Nodes), pairs, slowPairs, failures)
	if r.Status == "GAPS" {
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }
