// Command judge-openbao-health decides, from one captured observation document,
// whether every expected OpenBao server is proven able to serve: initialized,
// unsealed, on the expected release, in one cluster, with exactly one active
// server. A later protected procedure calls it before each single-server
// replacement (#4756, #4962).
//
// Kubernetes Ready is deliberately not an input: the readiness probe accepts
// sealed and uninitialized servers so that bootstrap can reach them, so Ready
// says nothing about whether a server can serve secrets. A successful HTTP status
// is not enough either; the health body must parse strictly and agree.
//
// The verdict covers server health only. It is not Raft membership or quorum
// proof, which needs an authenticated read this command does not make.
//
// It reads no cluster and makes no request. It prints one JSON line naming each
// failed server and reason, never a health body or cluster identity.
//
// Exit codes: 0 PROVEN, 1 HOLD (a definite finding), 2 UNKNOWN (the evidence is
// missing, stale or unusable, or the arguments are invalid).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const (
	verdictProven  = "PROVEN"
	verdictHold    = "HOLD"
	verdictUnknown = "UNKNOWN"

	// maxCaptureBytes bounds the observation document; three health bodies
	// and their envelope fit in a few kilobytes.
	maxCaptureBytes = 1 << 20

	scope = "server health only; not Raft membership or quorum proof"
)

// canonicalFields lists the member names this command decodes at each path,
// so a case-aliased spelling of one of them is refused rather than read as it.
var canonicalFields = map[string][]string{
	"":                 {"startedAt", "capturedAt", "servers"},
	"servers[]":        {"name", "podUID", "healthStatus", "health"},
	"servers[].health": {"initialized", "sealed", "standby", "version", "cluster_id", "server_time_utc"},
}

type expectation struct {
	servers []string
	version string
	maxAge  time.Duration
}

type finding struct {
	Server string `json:"server,omitempty"`
	Reason string `json:"reason"`
	hold   bool
}

type report struct {
	Verdict  string    `json:"verdict"`
	Servers  int       `json:"servers"`
	Findings []finding `json:"findings"`
	Scope    string    `json:"scope"`
}

type capture struct {
	StartedAt  *string         `json:"startedAt"`
	CapturedAt *string         `json:"capturedAt"`
	Servers    []serverCapture `json:"servers"`
}

type serverCapture struct {
	Name         *string         `json:"name"`
	PodUID       *string         `json:"podUID"`
	HealthStatus *int            `json:"healthStatus"`
	Health       json.RawMessage `json:"health"`
}

type health struct {
	Initialized   *bool   `json:"initialized"`
	Sealed        *bool   `json:"sealed"`
	Standby       *bool   `json:"standby"`
	Version       *string `json:"version"`
	ClusterID     *string `json:"cluster_id"`
	ServerTimeUTC *int64  `json:"server_time_utc"`
}

// clockSkew is how far a server's own clock may differ from the capture's
// window and still count as read during it.
const clockSkew = time.Minute

// window is the interval in which every health body must have been produced.
type window struct{ start, end time.Time }

func unknown(server, reason string) finding { return finding{Server: server, Reason: reason} }
func hold(server, reason string) finding    { return finding{Server: server, Reason: reason, hold: true} }

// judge returns the verdict for one capture read from input, observed at now.
func judge(input io.Reader, want expectation, now time.Time) report {
	result := report{Verdict: verdictUnknown, Servers: len(want.servers), Findings: []finding{}, Scope: scope}
	data, err := io.ReadAll(io.LimitReader(input, maxCaptureBytes+1))
	if err != nil {
		result.Findings = append(result.Findings, unknown("", "capture is unreadable"))
		return result
	}
	if len(data) > maxCaptureBytes {
		result.Findings = append(result.Findings, unknown("", "capture exceeds its size bound"))
		return result
	}
	if err := strictjson.Validate(data, canonicalFields); err != nil {
		result.Findings = append(result.Findings, unknown("", "capture is ambiguous or malformed: "+err.Error()))
		return result
	}
	var observed capture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observed); err != nil {
		result.Findings = append(result.Findings, unknown("", "capture does not have the expected shape"))
		return result
	}
	if observed.StartedAt == nil || observed.CapturedAt == nil {
		result.Findings = append(result.Findings, unknown("", "capture has no start or completion time"))
		return result
	}
	startedAt, startErr := time.Parse(time.RFC3339, *observed.StartedAt)
	capturedAt, endErr := time.Parse(time.RFC3339, *observed.CapturedAt)
	// Freshness is measured from the start, so the earliest read in the
	// capture is no older than the bound.
	switch {
	case startErr != nil || endErr != nil:
		result.Findings = append(result.Findings, unknown("", "capture times are not RFC 3339"))
		return result
	case capturedAt.Before(startedAt):
		result.Findings = append(result.Findings, unknown("", "capture completed before it started"))
		return result
	case capturedAt.After(now):
		result.Findings = append(result.Findings, unknown("", "capture is from the future"))
		return result
	case now.Sub(startedAt) > want.maxAge:
		result.Findings = append(result.Findings, unknown("", "capture is stale; recapture"))
		return result
	}

	expected := map[string]bool{}
	for _, name := range want.servers {
		expected[name] = true
	}
	byName := map[string]serverCapture{}
	podUIDs := map[string]bool{}
	for _, server := range observed.Servers {
		if server.Name == nil || *server.Name == "" {
			result.Findings = append(result.Findings, unknown("", "a server observation has no name"))
			continue
		}
		name := *server.Name
		if !expected[name] {
			// The name is captured data, so it is counted, never printed.
			result.Findings = append(result.Findings, unknown("", "capture contains an unexpected server"))
			continue
		}
		if _, seen := byName[name]; seen {
			result.Findings = append(result.Findings, unknown(name, "server observed more than once"))
			continue
		}
		byName[name] = server
		if server.PodUID == nil || *server.PodUID == "" {
			result.Findings = append(result.Findings, unknown(name, "observation is not bound to a pod identity"))
		} else if podUIDs[*server.PodUID] {
			result.Findings = append(result.Findings, unknown(name, "pod identity is shared with another server"))
		} else {
			podUIDs[*server.PodUID] = true
		}
	}

	healthy := map[string]health{}
	for _, name := range want.servers {
		server, ok := byName[name]
		if !ok {
			result.Findings = append(result.Findings, unknown(name, "server was not observed"))
			continue
		}
		if f, ok := judgeServer(name, server, want.version, window{startedAt, capturedAt}); !ok {
			result.Findings = append(result.Findings, f)
			continue
		}
		var body health
		_ = json.Unmarshal(server.Health, &body) // judgeServer proved it decodes.
		healthy[name] = body
	}

	if len(healthy) == len(want.servers) {
		clusters := map[string]bool{}
		active := 0
		for _, body := range healthy {
			clusters[*body.ClusterID] = true
			if !*body.Standby {
				active++
			}
		}
		if len(clusters) != 1 {
			result.Findings = append(result.Findings, hold("", "servers report different cluster identities"))
		}
		switch {
		case active == 0:
			result.Findings = append(result.Findings, hold("", "no server is active"))
		case active > 1:
			result.Findings = append(result.Findings, hold("", "more than one server is active"))
		}
	}

	sort.SliceStable(result.Findings, func(i, j int) bool { return result.Findings[i].Server < result.Findings[j].Server })
	result.Verdict = verdictProven
	for _, f := range result.Findings {
		if !f.hold {
			result.Verdict = verdictUnknown
			break
		}
		result.Verdict = verdictHold
	}
	return result
}

// judgeServer checks one server's own evidence. It returns ok only when the
// server is proven initialized, unsealed and on the expected release with a
// complete, successful health response read inside the capture window.
//
// Every check that can make the evidence UNKNOWN runs before the check it
// guards, so incomplete evidence is never reported as a narrower HOLD.
func judgeServer(name string, server serverCapture, version string, captured window) (finding, bool) {
	trimmed := bytes.TrimSpace(server.Health)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return unknown(name, "no health evidence"), false
	}
	if server.HealthStatus == nil {
		return unknown(name, "health response has no HTTP status"), false
	}
	var body health
	if err := json.Unmarshal(trimmed, &body); err != nil {
		return unknown(name, "health response is not an object of the expected shape"), false
	}
	// OpenBao always reports these, so a sealed or uninitialized server is a
	// definite finding even though its body omits the cluster identity.
	if body.Initialized == nil || body.Sealed == nil || body.ServerTimeUTC == nil {
		return unknown(name, "health response is incomplete"), false
	}
	served := time.Unix(*body.ServerTimeUTC, 0).UTC()
	if served.Before(captured.start.Add(-clockSkew)) || served.After(captured.end.Add(clockSkew)) {
		return unknown(name, "health response was not produced during the capture"), false
	}
	switch {
	case !*body.Initialized:
		return hold(name, "server is not initialized"), false
	case *body.Sealed:
		return hold(name, "server is sealed"), false
	}
	if body.Standby == nil || body.Version == nil || body.ClusterID == nil {
		return unknown(name, "health response is incomplete"), false
	}
	if *body.ClusterID == "" {
		return unknown(name, "health response names no cluster"), false
	}
	if *server.HealthStatus != 200 {
		return unknown(name, "health HTTP status disagrees with its body"), false
	}
	if *body.Version != version {
		return hold(name, "server is not on the expected release"), false
	}
	return finding{}, true
}

func parseExpectation(servers, version string, maxAge time.Duration) (expectation, error) {
	want := expectation{version: version, maxAge: maxAge}
	if version == "" {
		return want, errors.New("--expect-version is required")
	}
	if maxAge <= 0 || maxAge > time.Hour {
		return want, errors.New("--max-age must be positive and at most one hour")
	}
	seen := map[string]bool{}
	for _, name := range strings.Split(servers, ",") {
		if name == "" || seen[name] {
			return want, errors.New("--expect-servers must list distinct, non-empty server names")
		}
		seen[name] = true
		want.servers = append(want.servers, name)
	}
	return want, nil
}

// parseArgs parses args without ever exiting 0 on its own: -h and every parse
// error become errors, and a flag given twice is refused, so a trailing
// duplicate cannot quietly narrow the expected servers or swap the release.
func parseArgs(set *flag.FlagSet, args []string) error {
	set.Init(set.Name(), flag.ContinueOnError)
	set.SetOutput(io.Discard)
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	seen := map[string]int{}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if before, _, found := strings.Cut(name, "="); found {
			name = before
		}
		if set.Lookup(name) != nil {
			seen[name]++
			if seen[name] > 1 {
				return fmt.Errorf("--%s was given more than once", name)
			}
		}
	}
	return nil
}

func main() {
	input := flag.String("input", "", "captured observation document, or - for stdin")
	servers := flag.String("expect-servers", "", "comma-separated server names the reviewed render declares")
	version := flag.String("expect-version", "", "OpenBao release every server must run")
	maxAge := flag.Duration("max-age", 5*time.Minute, "oldest acceptable capture")
	err := parseArgs(flag.CommandLine, os.Args[1:])
	var want expectation
	if err == nil {
		want, err = parseExpectation(*servers, *version, *maxAge)
	}
	if err == nil && (*input == "" || flag.NArg() != 0) {
		err = errors.New("--input is required and no positional arguments are accepted")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge-openbao-health:", err)
		os.Exit(2)
	}
	reader := io.Reader(os.Stdin)
	if *input != "-" {
		file, err := os.Open(*input)
		if err != nil {
			fmt.Fprintln(os.Stderr, "judge-openbao-health: capture cannot be opened")
			os.Exit(2)
		}
		reader = file
	}
	result := judge(reader, want, time.Now().UTC())
	if file, ok := reader.(*os.File); ok && file != os.Stdin {
		_ = file.Close() // Read-only; judge has already read everything it will use.
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "judge-openbao-health:", err)
		os.Exit(2)
	}
	switch result.Verdict {
	case verdictProven:
		return
	case verdictHold:
		os.Exit(1)
	default:
		os.Exit(2)
	}
}
