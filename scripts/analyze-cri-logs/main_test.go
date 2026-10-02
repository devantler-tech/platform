package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type testNode struct {
	Label                                                                           string
	Lines, Parsed, Malformed, Unsupported, Starts, Returns, Pairs, FailedPairs      int
	OverlappingStarts, AmbiguousReturns, OrphanReturns, PendingStarts, OrderingGaps int
	OutsideWindow, WindowPairs, SlowPairs                                           int
	Earliest, Latest                                                                string
	WindowBracketed                                                                 bool
	SHA256                                                                          string
}
type testSample struct {
	Node, Method, TargetHash, PodUID, Outcome string
	Milliseconds                              int64
}
type testReport struct {
	Status, Scope, CorootAttribution string
	Nodes                            []testNode
	Samples                          []testSample
	SamplesTruncated                 bool
}

const uid = "11111111-2222-3333-4444-555555555555"
const sandbox = "RunPodSandbox for &PodSandboxMetadata{Name:example,Uid:" + uid + ",Namespace:example,Attempt:0,}"

func jsonLine(stamp, message string) string {
	b, _ := json.Marshal(map[string]string{"time": stamp, "msg": message, "level": "info"})
	return string(b) + "\n"
}
func logfmtLine(stamp, message string) string {
	return fmt.Sprintf("time=%q level=info msg=%q\n", stamp, message)
}

func invoke(t *testing.T, inputs map[string]string, extra ...string) (int, string, testReport, string) {
	t.Helper()
	d := t.TempDir()
	output := filepath.Join(d, "report.json")
	args := []string{"--experimental", "--from", "2026-10-02T10:00:00Z", "--to", "2026-10-02T10:01:00Z", "--output", output}
	for label, data := range inputs {
		path := filepath.Join(d, label+".log")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--input", label+"="+path)
	}
	args = append(args, extra...)
	var stdout bytes.Buffer
	code := run(args, &stdout)
	var report testReport
	if b, err := os.ReadFile(output); err == nil {
		if err = json.Unmarshal(b, &report); err != nil {
			t.Fatalf("invalid report: %v", err)
		}
	}
	return code, stdout.String(), report, output
}

func TestPairBothFormatsAndDoNotLeakTargets(t *testing.T) {
	secret := "https://registry.example/image?token=DO-NOT-EMIT"
	data := "node-address: " + jsonLine("2026-10-02T09:59:59Z", "ready") +
		jsonLine("2026-10-02T10:00:00Z", sandbox) + jsonLine("2026-10-02T10:00:06Z", sandbox+" returns sandbox id abc") +
		logfmtLine("2026-10-02T10:00:07Z", fmt.Sprintf("PullImage %q", secret)) +
		logfmtLine("2026-10-02T10:00:10Z", fmt.Sprintf("PullImage %q returns image reference abc", secret)) +
		jsonLine("2026-10-02T10:01:01Z", "ready")
	code, stdout, r, output := invoke(t, map[string]string{"node-1": data})
	if code != 0 || len(r.Nodes) != 1 {
		t.Fatalf("code=%d stdout=%s report=%+v", code, stdout, r)
	}
	n := r.Nodes[0]
	if n.Pairs != 2 || n.SlowPairs != 2 || n.WindowPairs != 2 || !n.WindowBracketed || n.SHA256 == "" {
		t.Fatalf("wrong evidence: %+v", n)
	}
	if len(r.Samples) != 2 || r.Samples[0].Milliseconds != 6000 || r.Samples[0].PodUID != uid || r.Samples[1].Milliseconds != 3000 {
		t.Fatalf("wrong samples: %+v", r.Samples)
	}
	if r.CorootAttribution != "UNPROVEN" || r.Scope != "finite CRI records; not complete request coverage" {
		t.Fatalf("missing scope: %+v", r)
	}
	b, _ := os.ReadFile(output)
	if strings.Contains(string(b)+stdout, secret) || strings.Contains(string(b)+stdout, "DO-NOT-EMIT") {
		t.Fatal("raw target leaked")
	}
	info, _ := os.Stat(output)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("report permissions %v", info.Mode())
	}
}

func TestOverlapsRemainAmbiguousUntilAllReturnsDrain(t *testing.T) {
	data := jsonLine("2026-10-02T09:59:59Z", "ready")
	for _, item := range []struct{ stamp, msg string }{
		{"10:00:00", sandbox}, {"10:00:01", sandbox}, {"10:00:02", sandbox + " returns sandbox id a"},
		{"10:00:03", sandbox + " returns sandbox id b"}, {"10:00:04", sandbox}, {"10:00:05", sandbox + " returns sandbox id c"},
	} {
		data += jsonLine("2026-10-02T"+item.stamp+"Z", item.msg)
	}
	data += jsonLine("2026-10-02T10:01:01Z", "ready")
	code, _, r, _ := invoke(t, map[string]string{"node-1": data})
	if code != 1 || len(r.Nodes) != 1 {
		t.Fatalf("code=%d report=%+v", code, r)
	}
	n := r.Nodes[0]
	if n.OverlappingStarts != 1 || n.AmbiguousReturns != 2 || n.Pairs != 1 || n.PendingStarts != 0 {
		t.Fatalf("overlap fabricated a pair: %+v", n)
	}
}

func TestNeverPairAcrossNodesAndReportOrphans(t *testing.T) {
	code, _, r, _ := invoke(t, map[string]string{
		"node-1": jsonLine("2026-10-02T10:00:00Z", sandbox),
		"node-2": jsonLine("2026-10-02T10:00:05Z", sandbox+" returns sandbox id a"),
	})
	if code != 1 || len(r.Nodes) != 2 {
		t.Fatalf("code=%d report=%+v", code, r)
	}
	if r.Nodes[0].PendingStarts != 1 || r.Nodes[1].OrphanReturns != 1 || len(r.Samples) != 0 {
		t.Fatalf("cross-node pairing: %+v", r)
	}
}

func TestReportMalformedUnsupportedFailureAndRetention(t *testing.T) {
	data := "not a structured record\n" + jsonLine("2026-10-02T10:00:01Z", "CreateContainer for unknown") +
		jsonLine("2026-10-02T10:00:02Z", sandbox) + jsonLine("2026-10-02T10:00:04Z", sandbox+" failed: rpc error")
	code, _, r, _ := invoke(t, map[string]string{"node-1": data})
	if code != 1 || len(r.Nodes) != 1 {
		t.Fatalf("code=%d report=%+v", code, r)
	}
	n := r.Nodes[0]
	if n.Malformed != 1 || n.Unsupported != 1 || n.FailedPairs != 1 || n.Pairs != 0 || n.WindowBracketed {
		t.Fatalf("missing gap: %+v", n)
	}
	if len(r.Samples) != 1 || r.Samples[0].Outcome != "failure" {
		t.Fatalf("failure hidden: %+v", r.Samples)
	}
}

func TestClockRegressionInvalidatesOutstandingStarts(t *testing.T) {
	data := jsonLine("2026-10-02T10:00:02Z", sandbox) + jsonLine("2026-10-02T10:00:01Z", "ready") +
		jsonLine("2026-10-02T10:00:03Z", sandbox+" returns sandbox id a")
	code, _, r, _ := invoke(t, map[string]string{"node-1": data})
	if code != 1 || len(r.Nodes) != 1 || r.Nodes[0].OrderingGaps != 1 || r.Nodes[0].Pairs != 0 || r.Nodes[0].PendingStarts != 1 {
		t.Fatalf("clock regression hidden: code=%d %+v", code, r)
	}
}

func TestPairStartsBeforeWindowButCountOnlyReturnsInside(t *testing.T) {
	data := jsonLine("2026-10-02T09:59:58Z", sandbox) + jsonLine("2026-10-02T10:00:01Z", sandbox+" returns sandbox id a") +
		jsonLine("2026-10-02T10:01:02Z", "ready")
	code, _, r, _ := invoke(t, map[string]string{"node-1": data})
	if code != 0 || len(r.Nodes) != 1 || r.Nodes[0].WindowPairs != 1 || r.Nodes[0].OutsideWindow != 2 || r.Samples[0].Milliseconds != 3000 {
		t.Fatalf("window incorrectly dropped start: code=%d %+v", code, r)
	}
}

func TestDefaultOffDoesNotOpenInputsOrWriteReport(t *testing.T) {
	d := t.TempDir()
	output := filepath.Join(d, "report.json")
	var stdout bytes.Buffer
	code := run([]string{"--input", "node-1=/missing", "--output", output}, &stdout)
	if code != 2 || !strings.Contains(stdout.String(), "experimental opt-in required") {
		t.Fatalf("default not off: %d %s", code, stdout.String())
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("default-off wrote report")
	}
}

func TestRefuseUnsafeInputsAndExistingOutput(t *testing.T) {
	for _, extra := range [][]string{{"--from", "bad"}, {"--to", "2026-10-02T09:00:00Z"}, {"--slow", "0s"}, {"--input", "node-1=/another"}} {
		code, _, _, output := invoke(t, map[string]string{"node-1": ""}, extra...)
		if code != 2 {
			t.Fatalf("invalid args accepted: %v code=%d", extra, code)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("invalid input wrote report: %v", extra)
		}
	}
	d := t.TempDir()
	path := filepath.Join(d, "log")
	output := filepath.Join(d, "out")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--experimental", "--from", "2026-10-02T10:00:00Z", "--to", "2026-10-02T10:01:00Z", "--input", "node-1=" + path, "--output", output}
	var stdout bytes.Buffer
	if run(args, &stdout) != 2 {
		t.Fatal("public input accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if run(args, &stdout) != 2 {
		t.Fatal("existing output overwritten")
	}
	b, _ := os.ReadFile(output)
	if string(b) != "preserve" {
		t.Fatal("foreign report changed")
	}
}

func TestRejectOversizeLineAndDoNotEchoIt(t *testing.T) {
	code, stdout, _, output := invoke(t, map[string]string{"node-1": strings.Repeat("SECRET", 200000)})
	if code != 2 || strings.Contains(stdout, "SECRET") {
		t.Fatalf("oversize input mishandled: %d", code)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("oversize analysis wrote success report")
	}
}

func TestObservedProtobufFormatPreservesNanoSeconds(t *testing.T) {
	metadata := `RunPodSandbox for name:"example-job"  uid:"` + uid + `"  namespace:"example"`
	data := jsonLine("2026-10-02T09:59:59Z", "ready") +
		"192.0.2.10: " + jsonLine("2026-10-02T10:00:00.100000001Z", metadata) +
		"192.0.2.10: " + jsonLine("2026-10-02T10:00:12.200000001Z", metadata+` returns sandbox id "abc"`) +
		jsonLine("2026-10-02T10:01:01Z", "ready")
	code, _, r, _ := invoke(t, map[string]string{"node-1": data})
	if code != 0 || len(r.Samples) != 1 || r.Samples[0].Milliseconds != 12100 || r.Samples[0].PodUID != uid {
		t.Fatalf("observed protobuf format lost: %d %+v", code, r)
	}
}

func TestTimestampInsideMessageIsNotAnEnvelope(t *testing.T) {
	data := fmt.Sprintf("level=info msg=%q\n", "time=2026-10-02T10:00:00Z "+sandbox)
	code, _, r, _ := invoke(t, map[string]string{"node-1": data})
	if code != 1 || len(r.Nodes) != 1 || r.Nodes[0].Malformed != 1 || r.Nodes[0].Starts != 0 {
		t.Fatalf("message manufactured evidence: %d %+v", code, r)
	}
}

func TestConcurrentPullsAreNotMatchedFIFO(t *testing.T) {
	key := `PullImage "registry.example/image:tag"`
	data := jsonLine("2026-10-02T10:00:00Z", key) + jsonLine("2026-10-02T10:00:01Z", key) +
		jsonLine("2026-10-02T10:00:02Z", key+" returns image reference a") +
		jsonLine("2026-10-02T10:00:03Z", key+" returns image reference b")
	code, _, r, _ := invoke(t, map[string]string{"node-1": data})
	if code != 1 || len(r.Nodes) != 1 || r.Nodes[0].AmbiguousReturns != 2 || r.Nodes[0].Pairs != 0 {
		t.Fatalf("concurrent pull mispaired: %d %+v", code, r)
	}
}

func TestExplicitSampleCapPreservesPairCounts(t *testing.T) {
	var data strings.Builder
	data.WriteString(jsonLine("2026-10-02T09:59:59Z", "ready"))
	base := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 1002; i++ {
		data.WriteString(jsonLine(base.Add(time.Duration(i*2)*time.Microsecond).Format(time.RFC3339Nano), sandbox))
		data.WriteString(jsonLine(base.Add(time.Duration(i*2+1)*time.Microsecond).Format(time.RFC3339Nano), sandbox+" returns sandbox id a"))
	}
	data.WriteString(jsonLine("2026-10-02T10:01:01Z", "ready"))
	code, _, r, _ := invoke(t, map[string]string{"node-1": data.String()}, "--slow", "1ns")
	if code != 1 || !r.SamplesTruncated || len(r.Samples) != 1000 || len(r.Nodes) != 1 || r.Nodes[0].Pairs != 1002 {
		t.Fatalf("sample cap silently dropped evidence: %d %+v", code, r.Nodes)
	}
}

func TestBoundedRegularInputAndNoSymlinkFollowing(t *testing.T) {
	d := t.TempDir()
	input := filepath.Join(d, "input")
	output := filepath.Join(d, "report")
	args := []string{"--experimental", "--from", "2026-10-02T10:00:00Z", "--to", "2026-10-02T10:01:00Z", "--input", "node-1=" + input, "--output", output}
	var stdout bytes.Buffer
	if err := syscall.Mkfifo(input, 0600); err != nil {
		t.Fatal(err)
	}
	if code := run(args, &stdout); code != 2 {
		t.Fatalf("FIFO accepted: %d", code)
	}
	if err := os.Remove(input); err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(d, "actual")
	if err := os.WriteFile(actual, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, input); err != nil {
		t.Fatal(err)
	}
	if code := run(args, &stdout); code != 2 {
		t.Fatalf("symlink followed: %d", code)
	}
	if err := os.Remove(input); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(input, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(8<<20 + 1); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if code := run(args, &stdout); code != 2 {
		t.Fatalf("oversize file accepted: %d", code)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("refused file wrote report")
	}
}
