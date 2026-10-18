// Command judge-openbao-quorum decides, from one captured Raft configuration
// read, whether every expected OpenBao server is a voting member of one Raft
// cluster with exactly one leader, and whether quorum survives taking the named
// server out for replacement. The protected single-server replacement procedure
// calls it beside judge-openbao-health before each step (#4970, #5033).
//
// The health judge proves that each server answers as initialized and unsealed;
// it cannot see membership. This command covers membership and quorum as the
// leader reports them. It does not prove storage health or secret delivery.
//
// It reads no cluster and makes no request. It prints one JSON line naming each
// failed expected server and reason, never a response body, address or node
// identity the capture supplied.
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

	// maxCaptureBytes bounds the observation document; a configuration of a
	// handful of members and its envelope fit in a few kilobytes.
	maxCaptureBytes = 1 << 20

	scope = "Raft membership and quorum as the leader reports them; not storage health or secret delivery"
)

// canonicalFields lists the member names this command decodes at each path,
// so a case-aliased spelling of one of them is refused rather than read as it.
var canonicalFields = map[string][]string{
	"":                                    {"startedAt", "capturedAt", "status", "configuration"},
	"configuration":                       {"data"},
	"configuration.data":                  {"config"},
	"configuration.data.config":           {"servers"},
	"configuration.data.config.servers[]": {"node_id", "voter", "leader"},
}

type expectation struct {
	servers []string
	replace string
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
	Replace  string    `json:"replace"`
	Findings []finding `json:"findings"`
	Scope    string    `json:"scope"`
}

type capture struct {
	StartedAt     *string         `json:"startedAt"`
	CapturedAt    *string         `json:"capturedAt"`
	Status        *int            `json:"status"`
	Configuration json.RawMessage `json:"configuration"`
}

// configuration mirrors the parts of OpenBao's sys/storage/raft/configuration
// response this command reads; every other native field is ignored.
type configuration struct {
	Data *struct {
		Config *struct {
			Servers []member `json:"servers"`
		} `json:"config"`
	} `json:"data"`
}

type member struct {
	NodeID *string `json:"node_id"`
	Voter  *bool   `json:"voter"`
	Leader *bool   `json:"leader"`
}

func unknown(server, reason string) finding { return finding{Server: server, Reason: reason} }
func hold(server, reason string) finding    { return finding{Server: server, Reason: reason, hold: true} }

// judge returns the verdict for one capture read from input, observed at now.
func judge(input io.Reader, want expectation, now time.Time) report {
	result := report{Verdict: verdictUnknown, Servers: len(want.servers), Replace: want.replace, Findings: []finding{}, Scope: scope}
	members, f, ok := readMembers(input, want, now)
	if !ok {
		result.Findings = append(result.Findings, f)
		return result
	}

	expected := map[string]bool{}
	for _, name := range want.servers {
		expected[name] = true
	}
	byName := map[string]member{}
	unexpected := 0
	voters := 0
	leaders := 0
	for _, m := range members {
		if m.NodeID == nil || *m.NodeID == "" {
			result.Findings = append(result.Findings, unknown("", "a member has no node identity"))
			continue
		}
		if m.Voter == nil || m.Leader == nil {
			// The node identity is captured data, so only an expected one is named.
			name := ""
			if expected[*m.NodeID] {
				name = *m.NodeID
			}
			result.Findings = append(result.Findings, unknown(name, "member entry is incomplete"))
			continue
		}
		name := *m.NodeID
		if _, seen := byName[name]; seen {
			reported := ""
			if expected[name] {
				reported = name
			}
			result.Findings = append(result.Findings, unknown(reported, "member listed more than once"))
			continue
		}
		byName[name] = m
		if *m.Voter {
			voters++
		}
		if *m.Leader {
			leaders++
		}
		if !expected[name] {
			unexpected++
		}
	}
	if unexpected > 0 {
		// Counted, never printed: the identities are captured data.
		result.Findings = append(result.Findings, hold("", fmt.Sprintf("configuration has %d unexpected member(s)", unexpected)))
	}
	for _, name := range want.servers {
		m, ok := byName[name]
		switch {
		case !ok:
			result.Findings = append(result.Findings, hold(name, "server is not a Raft member"))
		case !*m.Voter:
			result.Findings = append(result.Findings, hold(name, "server is not a voter"))
		}
	}
	switch {
	case leaders == 0:
		result.Findings = append(result.Findings, hold("", "no member is leader"))
	case leaders > 1:
		result.Findings = append(result.Findings, hold("", "more than one member is leader"))
	}

	// Taking a voter out leaves voters-1 of the configured voters available;
	// the cluster stays writable only while those are still a majority.
	if target, ok := byName[want.replace]; ok && *target.Voter {
		quorum := voters/2 + 1
		if voters-1 < quorum {
			result.Findings = append(result.Findings, hold(want.replace, fmt.Sprintf("replacing it leaves %d of %d voters, below quorum %d", voters-1, voters, quorum)))
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

// readMembers validates the envelope and the configuration body and returns
// the reported members. Every check that can make the evidence UNKNOWN runs
// here, before any membership finding, so incomplete evidence is never
// reported as a narrower HOLD.
func readMembers(input io.Reader, want expectation, now time.Time) ([]member, finding, bool) {
	data, err := io.ReadAll(io.LimitReader(input, maxCaptureBytes+1))
	if err != nil {
		return nil, unknown("", "capture is unreadable"), false
	}
	if len(data) > maxCaptureBytes {
		return nil, unknown("", "capture exceeds its size bound"), false
	}
	if err := strictjson.Validate(data, canonicalFields); err != nil {
		return nil, unknown("", "capture is ambiguous or malformed: "+err.Error()), false
	}
	var observed capture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observed); err != nil {
		return nil, unknown("", "capture does not have the expected shape"), false
	}
	if observed.StartedAt == nil || observed.CapturedAt == nil {
		return nil, unknown("", "capture has no start or completion time"), false
	}
	startedAt, startErr := time.Parse(time.RFC3339, *observed.StartedAt)
	capturedAt, endErr := time.Parse(time.RFC3339, *observed.CapturedAt)
	// Freshness is measured from the start, so the read is no older than the
	// bound. The configuration body carries no time of its own.
	switch {
	case startErr != nil || endErr != nil:
		return nil, unknown("", "capture times are not RFC 3339"), false
	case capturedAt.Before(startedAt):
		return nil, unknown("", "capture completed before it started"), false
	case capturedAt.After(now):
		return nil, unknown("", "capture is from the future"), false
	case now.Sub(startedAt) > want.maxAge:
		return nil, unknown("", "capture is stale; recapture"), false
	}
	trimmed := bytes.TrimSpace(observed.Configuration)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, unknown("", "no configuration evidence"), false
	}
	if observed.Status == nil {
		return nil, unknown("", "configuration response has no HTTP status"), false
	}
	if *observed.Status != 200 {
		// A refused or failed read decides nothing about membership.
		return nil, unknown("", "configuration read did not succeed"), false
	}
	var body configuration
	if err := json.Unmarshal(trimmed, &body); err != nil {
		return nil, unknown("", "configuration response is not an object of the expected shape"), false
	}
	if body.Data == nil || body.Data.Config == nil || body.Data.Config.Servers == nil {
		return nil, unknown("", "configuration response is incomplete"), false
	}
	return body.Data.Config.Servers, finding{}, true
}

func parseExpectation(servers, replace string, maxAge time.Duration) (expectation, error) {
	want := expectation{replace: replace, maxAge: maxAge}
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
	if !seen[replace] {
		return want, errors.New("--replace must name one of --expect-servers")
	}
	return want, nil
}

// onceValue refuses a second Set, so a flag given twice is an error.
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

// parseArgs parses args without ever exiting 0 on its own: -h and every parse
// error become errors, and a flag given twice is refused, so a trailing
// duplicate cannot quietly narrow the expected servers or swap the target.
// The refusal happens in the flag values themselves, so it holds wherever the
// parser assigns a value, including after an argument that is itself "--".
func parseArgs(set *flag.FlagSet, args []string) error {
	set.Init(set.Name(), flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.VisitAll(func(f *flag.Flag) {
		if _, wrapped := f.Value.(*onceValue); !wrapped {
			f.Value = &onceValue{Value: f.Value, name: f.Name}
		}
	})
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func main() {
	input := flag.String("input", "", "captured configuration document, or - for stdin")
	servers := flag.String("expect-servers", "", "comma-separated server names the reviewed render declares")
	replace := flag.String("replace", "", "the one expected server the next step would replace")
	maxAge := flag.Duration("max-age", 5*time.Minute, "oldest acceptable capture")
	err := parseArgs(flag.CommandLine, os.Args[1:])
	var want expectation
	if err == nil {
		want, err = parseExpectation(*servers, *replace, *maxAge)
	}
	if err == nil && (*input == "" || flag.NArg() != 0) {
		err = errors.New("--input is required and no positional arguments are accepted")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "judge-openbao-quorum:", err)
		os.Exit(2)
	}
	reader := io.Reader(os.Stdin)
	if *input != "-" {
		file, err := os.Open(*input)
		if err != nil {
			fmt.Fprintln(os.Stderr, "judge-openbao-quorum: capture cannot be opened")
			os.Exit(2)
		}
		reader = file
	}
	result := judge(reader, want, time.Now().UTC())
	if file, ok := reader.(*os.File); ok && file != os.Stdin {
		_ = file.Close() // Read-only; judge has already read everything it will use.
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "judge-openbao-quorum:", err)
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
