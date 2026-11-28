package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type historyFolder struct {
	messages, uidNext, uidValidity uint32
}

type historySnapshot map[string]historyFolder
type historyObserver func(context.Context, string, historyConfig) (historySnapshot, error)
type historyExecutor func(context.Context, []string, io.Writer) error

func historyArgs(v historyConfig, dry bool) []string {
	args := []string{
		"--host1", "imap.mail.me.com", "--port1", "993", "--ssl1",
		"--user1", v.username, "--passfile1", filepath.Join(v.dir, "source-password"),
		"--host2", "mail.devantler.tech", "--port2", "993", "--ssl2",
		"--user2", historyDestination, "--passfile2", filepath.Join(v.dir, "destination-password"),
		"--sep1", "/", "--sep2", "/", "--prefix1", "", "--prefix2", "",
		"--noexpunge1", "--noexpunge2", "--nouidexpunge2", "--noexpungeaftereach",
		"--noresyncflags", "--nosyncacls", "--syncduplicates", "--noskipemptyfolders",
		// Identify a message by its Message-Id alone, not imapsync's default
		// Message-Id and Received. The destination returned a few messages'
		// Received headers differently from the source (#5481, attempt 12: the
		// same 2 messages copied again on every run), and a copy whose identity
		// does not round-trip is appended again each run.
		//
		// No fallback header is added on purpose. A message without a
		// Message-Id is then unidentified, and the pass refuses it (h1_noheader
		// above 0) instead of matching it by a weaker header: a Date-only
		// identity could match an unrelated destination message with the same
		// Date, leave the source message uncopied and still let the counts
		// reconcile to COMPLETE.
		"--useheader", "Message-Id",
		"--noreleasecheck", "--nolog", "--noemailreport1", "--noemailreport2", "--noid",
		"--timeout1", "60", "--timeout2", "60", "--errorsmax", "1",
		"--pidfile", "/tmp/email-history-import.pid", "--tmpdir", "/tmp",
		// Preserve message contents without marking a newly copied message for deletion.
		"--regexflag", `s/\\Deleted//g`,
	}
	for _, peer := range []struct{ side, host string }{{"1", "imap.mail.me.com"}, {"2", "mail.devantler.tech"}} {
		for _, option := range []string{"SSL_verify_mode=1", "SSL_verifycn_scheme=imap", "SSL_verifycn_name=" + peer.host, "SSL_ca_file=/etc/ssl/certs/ca-certificates.crt", "SSL_version=SSLv23:!SSLv2:!SSLv3:!TLSv1:!TLSv1_1"} {
			args = append(args, "--sslargs"+peer.side, option)
		}
	}
	if dry {
		args = append(args, "--dry")
	}
	return args
}

// historyReport discards the tool's private output. Only its final public result
// is retained in memory, with fixed bounds even if an upstream line is malformed.
type historyReport struct {
	mu                     sync.Mutex
	line                   []byte
	bytes                  int
	good, identified, zero bool
	bad                    bool
	messages               uint64
	// counts holds the integers of the tool's closing statistics, keyed by a
	// fixed name, for the per-pass diagnostic line. It never decides success.
	counts map[string]uint64
	// inStatistics is set once the tool prints its closing statistics header;
	// the labelled counts are read only after it.
	inStatistics bool
	// overlong is set while the rest of an over-bound line is being dropped.
	overlong bool
}

var historyGood = regexp.MustCompile(`^The sync looks good, all ([0-9]+) identified messages in host1 are on host2\.$`)

// historyStatisticsHeader starts imapsync's closing statistics, printed once
// at the end of a pass.
const historyStatisticsHeader = "++++ Statistics"

// The closing statistics the per-pass diagnostic line reports. Only these
// labels are read, only after the statistics header, and only the integer
// after each, so no folder name, header, subject or server reply can reach the
// receipt. Within the block the first value of a label is kept, so a later line
// with the same label cannot replace what the closing summary printed.
// errors and identified are different: they come from the result lines the
// success check already reads ("Detected N errors", "The sync looks good" and
// "The sync is not finished"), wherever those appear, and the last one wins.
var (
	historyStatisticKeys = map[string]string{
		"Messages transferred":                   "transferred",
		"Messages skipped":                       "skipped",
		"Messages found duplicate on host1":      "h1_duplicates",
		"Messages found duplicate on host2":      "h2_duplicates",
		"Messages found crossduplicate on host2": "h2_crossduplicates",
		"Messages void (noheader) on host1":      "h1_noheader",
		"Messages void (noheader) on host2":      "h2_noheader",
		"Messages found in host1 not in host2":   "in1_not_in2",
		"Messages found in host2 not in host1":   "in2_not_in1",
	}
	historyStatisticOrder = []string{"identified", "errors", "transferred", "would_transfer", "skipped", "h1_duplicates", "h2_duplicates", "h2_crossduplicates", "h1_noheader", "h2_noheader", "in1_not_in2", "in2_not_in1"}
	historyStatistic      = regexp.MustCompile(`^(Messages [a-z0-9() ]+?) +: ([0-9]+)\b`)
	historyDryTransfers   = regexp.MustCompile(`\(could be ([0-9]+) without --dry mode\)`)
	historyErrorCount     = regexp.MustCompile(`^Detected ([0-9]+) errors$`)
	historyNotFinished    = regexp.MustCompile(`^The sync is not finished, there are [0-9]+ among ([0-9]+) identified messages in host1 that are not on host2\.$`)
)

// count records one parsed integer for the diagnostic line; a value that does
// not parse is left out rather than guessed. With first set, a value already
// recorded is kept.
func (r *historyReport) count(key, digits string, first bool) {
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return
	}
	if r.counts == nil {
		r.counts = map[string]uint64{}
	}
	if _, seen := r.counts[key]; first && seen {
		return
	}
	r.counts[key] = n
}

// statistics reads the fixed-vocabulary counts from one complete line.
func (r *historyReport) statistics(line string) {
	if line == historyStatisticsHeader {
		r.inStatistics = true
	}
	if m := historyStatistic.FindStringSubmatch(line); m != nil && r.inStatistics {
		if key, known := historyStatisticKeys[m[1]]; known {
			r.count(key, m[2], true)
			if key == "transferred" {
				if dry := historyDryTransfers.FindStringSubmatch(line); dry != nil {
					r.count("would_transfer", dry[1], true)
				}
			}
		}
	}
	if m := historyErrorCount.FindStringSubmatch(line); m != nil {
		// The largest count is kept: any nonzero one already fails the pass,
		// so a later "Detected 0 errors" must not make the line read errors=0.
		if n, err := strconv.ParseUint(m[1], 10, 64); err == nil && n >= r.counts["errors"] {
			r.count("errors", m[1], false)
		}
	}
	if m := historyGood.FindStringSubmatch(line); m != nil {
		r.count("identified", m[1], false)
	}
	if m := historyNotFinished.FindStringSubmatch(line); m != nil {
		r.count("identified", m[1], false)
	}
}

// historyPassLine is the per-pass diagnostic line: the pass, the tool's exit
// status and the fixed counts, each a number or "?" when the tool printed none.
// observed is the source total the pass is judged against. It is written for
// every pass, successful or not, and carries numbers only.
func historyPassLine(pass string, runErr error, r *historyReport, observed uint64) string {
	exit := "?"
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		exit = "0"
	case errors.As(runErr, &exitErr) && exitErr.ExitCode() < 0:
		// The process was killed, for example by the import's deadline.
		exit = "signal"
	case errors.As(runErr, &exitErr):
		exit = strconv.Itoa(exitErr.ExitCode())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "MAIL_HISTORY_IMPORT_PASS pass=%s exit=%s observed=%d", pass, exit, observed)
	for _, key := range historyStatisticOrder {
		value := "?"
		if n, ok := r.counts[key]; ok {
			value = strconv.FormatUint(n, 10)
		}
		fmt.Fprintf(&b, " %s=%s", key, value)
	}
	b.WriteString("\n")
	return b.String()
}

func (r *historyReport) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Past either bound the report can no longer be judged, so the pass fails.
	// The rest is still read and discarded, never refused: a refusal would stop
	// the pipe from draining, block the tool until the import's deadline and
	// lose its exit status.
	r.bytes += len(data)
	if r.bytes > 128<<20 {
		r.bad = true
		return len(data), nil
	}
	for _, b := range data {
		if b == '\n' && r.overlong {
			r.overlong = false
			continue
		}
		if r.overlong {
			continue
		}
		if b == '\n' {
			line := strings.TrimSuffix(string(r.line), "\r")
			r.line = r.line[:0]
			r.statistics(line)
			switch {
			case historyGood.MatchString(line):
				r.good = true
				var err error
				r.messages, err = strconv.ParseUint(historyGood.FindStringSubmatch(line)[1], 10, 64)
				r.bad = r.bad || err != nil
			case line == "There is no unidentified message on host1.":
				r.identified = true
			case line == "Detected 0 errors":
				r.zero = true
			case strings.HasPrefix(line, "Detected ") && strings.HasSuffix(line, " errors"):
				r.bad = true
			case strings.HasPrefix(line, "The sync is not finished,") || strings.Contains(line, " unidentified messages "):
				r.bad = true
			}
			continue
		}
		if len(r.line) >= 64<<10 {
			// An overlong line is dropped up to its newline; it is never judged.
			r.bad = true
			r.overlong = true
			r.line = r.line[:0]
			continue
		}
		r.line = append(r.line, b)
	}
	return len(data), nil
}

func executeHistory(ctx context.Context, args []string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, "/usr/local/bin/imapsync", args...)
	cmd.Env = []string{"HOME=/tmp", "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.Dir = "/tmp"
	cmd.Stdout, cmd.Stderr = output, output
	return cmd.Run()
}

func importHistory(ctx context.Context, v historyConfig, observe historyObserver, execute historyExecutor, receipt io.Writer) error {
	fail := func(stage string) error { return errors.New(stage) }
	// An observation names its failed step only from a fixed list, so no server
	// reply or private value can reach the receipt.
	observed := func(side string, err error) error {
		if err != nil && historyObservationSteps[err.Error()] {
			return fail(side + "-" + err.Error())
		}
		return fail(side)
	}
	source, err := observe(ctx, "source", v)
	if err != nil || len(source) == 0 || len(source) > 1000 {
		return observed("source", err)
	}
	destination, err := observe(ctx, "destination", v)
	if err != nil {
		return observed("destination", err)
	}
	// identified counts distinct identities across all folders, so it lies
	// between the largest folder and the total: within one folder every message
	// has its own identity (--syncduplicates keys each repeat separately), while
	// the same message filed in several folders counts once. Below the largest
	// folder the tool examined only part of the mailbox; above the total the
	// source changed during the pass. Each folder's completeness is then proved
	// by the reconciliation below.
	var total, largest uint64
	for _, box := range source {
		total += uint64(box.messages)
		largest = max(largest, uint64(box.messages))
	}
	for _, dry := range []bool{false, true} {
		var result historyReport
		runErr := execute(ctx, historyArgs(v, dry), &result)
		// The diagnostic line is written for every pass, before it is judged, so
		// a failed import still shows what the tool counted. It is best effort:
		// the completion line below remains the only success signal.
		pass := "copy"
		if dry {
			pass = "verify"
		}
		_, _ = io.WriteString(receipt, historyPassLine(pass, runErr, &result, total))
		if runErr != nil || result.bad || !result.zero || total > 0 && (!result.good || !result.identified || result.messages < largest || result.messages > total) {
			if dry {
				return fail("verification")
			}
			return fail("copy")
		}
	}
	currentSource, err := observe(ctx, "source", v)
	if err != nil || !reflect.DeepEqual(source, currentSource) {
		return fail("source-change")
	}
	currentDestination, err := observe(ctx, "destination", v)
	if err != nil {
		return fail("readback")
	}
	for name, before := range source {
		after, found := currentDestination[name]
		if !found || after.messages < before.messages {
			return fail("reconciliation")
		}
	}
	for name, before := range destination {
		after, found := currentDestination[name]
		if !found || after.messages < before.messages {
			return fail("reconciliation")
		}
	}
	_, err = fmt.Fprintf(receipt, "MAIL_HISTORY_IMPORT=COMPLETE folders=%d messages=%d source=PRESERVED routing=UNCHANGED\n", len(source), total)
	return err
}
