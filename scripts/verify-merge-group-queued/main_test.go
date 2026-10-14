package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const groupSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const baseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const sourceSHA = "cccccccccccccccccccccccccccccccccccccccc"
const headRef = "refs/heads/gh-readonly-queue/main/pr-4891-" + baseSHA
const validEvent = `{"action":"checks_requested","repository":{"full_name":"devantler-tech/platform"},"merge_group":{"base_ref":"refs/heads/main","base_sha":"` + baseSHA + `","head_ref":"` + headRef + `","head_sha":"` + groupSHA + `","head_commit":{"id":"` + groupSHA + `","timestamp":"2026-10-14T16:44:00+00:00"}}}`
const validResponse = `{"data":{"repository":{"nameWithOwner":"devantler-tech/platform","ref":{"target":{"oid":"` + groupSHA + `"}},"mergeQueue":{"id":"MQ_actual_queue"},"pullRequest":{"number":4891,"state":"OPEN","baseRefName":"main","headRefOid":"` + sourceSHA + `","isInMergeQueue":true,"mergeQueueEntry":{"id":"MQE_original_entry","enqueuedAt":"2026-10-14T16:43:58Z","headCommit":{"oid":"` + groupSHA + `"},"baseCommit":{"oid":"` + baseSHA + `"},"mergeQueue":{"id":"MQ_actual_queue"},"pullRequest":{"number":4891,"headRefOid":"` + sourceSHA + `"}}}}}}`

func TestSquashQueueWitness(t *testing.T) {
	e, n, err := parseEvent([]byte(validEvent), repository, groupSHA, "merge_group")
	if err != nil {
		t.Fatal(err)
	}
	got, err := evaluate([]byte(validResponse), e, n)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != sourceSHA || got.Base != baseSHA || got.Group != groupSHA || got.PR != 4891 {
		t.Fatalf("bad receipt: %+v", got)
	}
	// The ref suffix is the BASE, not the source. No parent/ancestry witness exists
	// for a squash group; only the real queue's exact synthetic revision is used.
	if strings.HasSuffix(got.Ref, got.Source) {
		t.Fatal("fixture must distinguish source and base")
	}
}

func TestEventRefusalsBeforeNetwork(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"eviction action", "checks_requested", "destroyed"},
		{"wrong repository", repository, "someone/platform"},
		{"wrong base", "refs/heads/main", "refs/heads/other"},
		{"wrong group", groupSHA, sourceSHA},
		{"short base", baseSHA, "1234"},
		{"not a queue ref", headRef, "refs/heads/main"},
		{"zero PR", "pr-4891-", "pr-0-"},
		{"PR overflow", "pr-4891-", "pr-999999999999999999999999999999-"},
		{"bad date", "2026-10-14T16:44:00+00:00", "2026-02-30T16:44:00Z"},
		{"non UTC", "+00:00", "+02:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.ReplaceAll(validEvent, tc.from, tc.to)
			if mutated == validEvent {
				t.Fatal("mutation changed nothing")
			}
			if _, _, err := parseEvent([]byte(mutated), repository, groupSHA, "merge_group"); err == nil {
				t.Fatal("accepted invalid event")
			}
		})
	}
	for _, input := range []string{"", "{", "null", "{}"} {
		if _, _, err := parseEvent([]byte(input), repository, groupSHA, "merge_group"); err == nil {
			t.Fatal("accepted partial event")
		}
	}
	for _, name := range []string{"pull_request", "workflow_dispatch", ""} {
		if _, _, err := parseEvent([]byte(validEvent), repository, groupSHA, name); err == nil {
			t.Fatal("accepted wrong event name")
		}
	}
}

func TestQueueRefusals(t *testing.T) {
	e, number, err := parseEvent([]byte(validEvent), repository, groupSHA, "merge_group")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"evicted", `"isInMergeQueue":true`, `"isInMergeQueue":false`},
		{"merged", `"state":"OPEN"`, `"state":"MERGED"`},
		{"closed", `"state":"OPEN"`, `"state":"CLOSED"`},
		{"wrong repo", repository, "other/platform"},
		{"wrong PR", `"number":4891`, `"number":4892`},
		{"wrong base ref", `"baseRefName":"main"`, `"baseRefName":"other"`},
		{"wrong group", groupSHA, sourceSHA},
		{"wrong base", baseSHA, sourceSHA},
		{"missing source", sourceSHA, ""},
		{"missing entry", "MQE_original_entry", ""},
		{"other queue", `"mergeQueue":{"id":"MQ_actual_queue"},"pullRequest":{"number":4891,"headRefOid"`, `"mergeQueue":{"id":"MQ_other_queue"},"pullRequest":{"number":4891,"headRefOid"`},
		{"reenqueued", "2026-10-14T16:43:58Z", "2026-10-14T16:45:00Z"},
		{"malformed time", "2026-10-14T16:43:58Z", "bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := strings.ReplaceAll(validResponse, tc.from, tc.to)
			if data == validResponse {
				t.Fatal("mutation changed nothing")
			}
			if _, err := evaluate([]byte(data), e, number); err == nil {
				t.Fatal("accepted broken queue witness")
			}
		})
	}
	for _, input := range []string{"", "{", "null", "{}", `{"errors":[{"message":"unreadable"}]}`, strings.Replace(validResponse, `{"data":`, `{"errors":[{"message":"partial failure"}],"data":`, 1)} {
		if _, err := evaluate([]byte(input), e, number); err == nil {
			t.Fatal("accepted missing or errored witness")
		}
	}
}

// This exercises the actual bounded process transport with a native Git
// checkout and a fixture gh executable. No live credentials or APIs are used.
func TestNativeCaptureAndPreflight(t *testing.T) {
	dir := t.TempDir()
	workspace, bin := filepath.Join(dir, "checkout"), filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			t.Setenv(strings.SplitN(kv, "=", 2)[0], "")
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch=main", "--object-format=sha1", workspace)
	git("-C", workspace, "-c", "user.name=Queue Fixture", "-c", "user.email=queue@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "queue")
	head := git("-C", workspace, "rev-parse", "HEAD")
	eventPath, responsePath, logPath, output := filepath.Join(dir, "event.json"), filepath.Join(dir, "response.json"), filepath.Join(dir, "calls"), filepath.Join(dir, "output")
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(eventPath, strings.ReplaceAll(validEvent, groupSHA, head))
	write(responsePath, strings.ReplaceAll(validResponse, groupSHA, head))
	write(output, "")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$QUEUE_TEST_LOG\"\n[ \"$QUEUE_TEST_FAIL\" != 1 ] || exit 1\ncat \"$QUEUE_TEST_RESPONSE\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("QUEUE_TEST_LOG", logPath)
	t.Setenv("QUEUE_TEST_RESPONSE", responsePath)
	t.Setenv("GITHUB_EVENT_PATH", eventPath)
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("GITHUB_REPOSITORY", repository)
	t.Setenv("GITHUB_SHA", head)
	t.Setenv("GITHUB_EVENT_NAME", "merge_group")
	t.Setenv("GITHUB_WORKSPACE", workspace)
	t.Setenv("GH_TOKEN", "offline-queue-fixture-not-a-credential")
	t.Setenv("QUEUE_TEST_FAIL", "")
	var stdout bytes.Buffer
	if err := verify("capture", &stdout); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	initial := strings.TrimSpace(strings.TrimPrefix(string(data), "receipt="))
	t.Setenv("QUEUE_RECEIPT", initial)
	if err := verify("check", &stdout); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(log), "api --hostname github.com graphql") || !strings.Contains(string(log), "-F number=4891") || !strings.Contains(string(log), "-f ref="+headRef) {
		t.Fatalf("wrong native read: %s, %v", log, err)
	}

	for _, tc := range []struct{ name, from, to string }{
		{"new entry", "MQE_original_entry", "MQE_replaced_entry"},
		{"new source", sourceSHA, baseSHA},
		{"evicted", `"isInMergeQueue":true`, `"isInMergeQueue":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(responsePath, strings.ReplaceAll(strings.ReplaceAll(validResponse, groupSHA, head), tc.from, tc.to))
			if err := verify("check", &stdout); err == nil {
				t.Fatal("accepted changed receipt")
			}
		})
	}
	write(responsePath, strings.ReplaceAll(validResponse, groupSHA, head))
	t.Setenv("QUEUE_TEST_FAIL", "1")
	if err := verify("check", &stdout); err == nil {
		t.Fatal("accepted failed API read")
	}
	t.Setenv("QUEUE_TEST_FAIL", "")
	t.Setenv("QUEUE_RECEIPT", "")
	if err := verify("check", &stdout); err == nil {
		t.Fatal("accepted missing receipt")
	}
	t.Setenv("QUEUE_RECEIPT", initial)
	t.Setenv("GITHUB_SHA", groupSHA)
	if err := verify("check", &stdout); err == nil {
		t.Fatal("accepted mismatched checkout")
	}
	t.Setenv("GITHUB_SHA", head)
	write(filepath.Join(workspace, "dirty"), "untracked")
	if err := verify("check", &stdout); err == nil {
		t.Fatal("accepted dirty checkout")
	}
	// Invalid events must stop before any API read, even when plausible state exists.
	before, _ := os.ReadFile(logPath)
	write(eventPath, `{}`)
	if err := verify("capture", &stdout); err == nil {
		t.Fatal("accepted malformed event")
	}
	after, _ := os.ReadFile(logPath)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid event reached network transport")
	}
}

func TestNativeReadTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := command(ctx, t.TempDir(), "sleep", "5"); err == nil {
		t.Fatal("accepted timed-out read")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("read exceeded native deadline")
	}
}

func TestReceiptJSON(t *testing.T) {
	e, number, err := parseEvent([]byte(validEvent), repository, groupSHA, "merge_group")
	if err != nil {
		t.Fatal(err)
	}
	r, err := evaluate([]byte(validResponse), e, number)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded receipt
	if err := json.Unmarshal(data, &decoded); err != nil || decoded != r {
		t.Fatal("receipt is not lossless")
	}
}
