package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	dockerfileAddInstruction = regexp.MustCompile(`(?i)^\s*ADD\s`)
	dockerfilePinnedChecksum = regexp.MustCompile(`(^|\s)--checksum=sha256:[0-9a-f]{64}(\s|$)`)
)

// dockerfileInstructions joins backslash-continued lines, so a flag on a
// continuation line still belongs to its instruction.
func dockerfileInstructions(data string) []string {
	var instructions []string
	current := ""
	for _, line := range strings.Split(data, "\n") {
		if current == "" && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, "\\") {
			current += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}
		instructions = append(instructions, current+trimmed)
		current = ""
	}
	if current != "" {
		instructions = append(instructions, current)
	}
	return instructions
}

// Checkov cannot scope CKV_DOCKER_4 to one instruction: the skip on the caddy
// LICENSE fetch covers the whole Dockerfile. This test is what still refuses an
// ADD that is not pinned by digest.
func TestEveryDockerfileAddIsChecksumPinned(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	adds := 0
	for _, instruction := range dockerfileInstructions(string(data)) {
		if !dockerfileAddInstruction.MatchString(instruction) {
			continue
		}
		adds++
		if !dockerfilePinnedChecksum.MatchString(instruction) {
			t.Errorf("ADD without a pinned --checksum=sha256: digest: %s", strings.TrimSpace(instruction))
		}
	}
	if adds == 0 {
		t.Fatal("found no ADD instruction; if the caddy LICENSE fetch was removed, remove its CKV_DOCKER_4 skip and this test")
	}
}

func TestDockerfileInstructionsKeepContinuedFlags(t *testing.T) {
	got := dockerfileInstructions("# comment\nADD \\\n  --checksum=sha256:" + strings.Repeat("a", 64) + " \\\n  https://x /y\nADD https://z /w\n")
	if len(got) != 3 || !dockerfilePinnedChecksum.MatchString(got[0]) || dockerfilePinnedChecksum.MatchString(got[1]) {
		t.Fatalf("unexpected instructions: %q", got)
	}
}
