package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
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
}

var historyGood = regexp.MustCompile(`^The sync looks good, all ([0-9]+) identified messages in host1 are on host2\.$`)

func (r *historyReport) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bytes += len(data)
	if r.bytes > 128<<20 {
		r.bad = true
		return 0, errors.New("history-output-bound")
	}
	for _, b := range data {
		if b == '\n' {
			line := strings.TrimSuffix(string(r.line), "\r")
			r.line = r.line[:0]
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
			r.bad = true
			return 0, errors.New("history-line-bound")
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

// historySourceOnlyGrew reports whether the source kept every folder it had,
// with the same UID validity, and only gained messages: no folder lost one or
// had its next UID go back, and no folder gained more messages than new UIDs.
// New mail, and new folders, are expected while the domain's MX still points at
// the source; the copy after MX moves picks them up. A folder that vanished,
// was recreated or shrank means the source changed in a way the copy cannot
// vouch for.
func historySourceOnlyGrew(before, after historySnapshot) bool {
	if len(after) < len(before) {
		return false
	}
	for name, was := range before {
		now, found := after[name]
		if !found || now.uidValidity != was.uidValidity || now.messages < was.messages || now.uidNext < was.uidNext ||
			now.messages-was.messages > now.uidNext-was.uidNext {
			return false
		}
	}
	return true
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
	var total uint64
	for _, box := range source {
		total += uint64(box.messages)
	}
	// The source keeps receiving mail until MX moves, which needs this copy
	// first, so it may grow while the copy runs. The tool must still find at
	// least every message observed at the start, all of them on the destination.
	for _, dry := range []bool{false, true} {
		var result historyReport
		if execute(ctx, historyArgs(v, dry), &result) != nil || result.bad || !result.zero || total > 0 && (!result.good || !result.identified || result.messages < total) {
			if dry {
				return fail("verification")
			}
			return fail("copy")
		}
	}
	currentSource, err := observe(ctx, "source", v)
	if err != nil || !historySourceOnlyGrew(source, currentSource) {
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
