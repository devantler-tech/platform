package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

type rejectedOutput struct{}

func (rejectedOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCompleteAnalysisRefusesFailedOutput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.log")
	data := jsonLine("2026-10-02T09:59:59Z", "ready") + jsonLine("2026-10-02T10:01:01Z", "ready")
	if err := os.WriteFile(input, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--experimental", "--from", "2026-10-02T10:00:00Z", "--to", "2026-10-02T10:01:00Z", "--output", filepath.Join(dir, "report.json"), "--input", "node-1=" + input}
	if code := run(args, rejectedOutput{}); code != 2 {
		t.Fatalf("unwritten analysis receipt must refuse: code=%d", code)
	}
}
