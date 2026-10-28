package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"time"
)

func runStorageSeed(env func(string) string, out io.Writer) int {
	fail := func(stage string) int {
		_, _ = fmt.Fprintf(out, "EMAIL_STORAGE_KEY=UNVERIFIED stage=%s\n", stage)
		return 2
	}
	if env("GITHUB_REPOSITORY") != "devantler-tech/platform" || env("GITHUB_EVENT_NAME") != "workflow_dispatch" || env("GITHUB_REF") != "refs/heads/main" || env("GITHUB_RUN_ATTEMPT") != "1" || env("EMAIL_STORAGE_CONFIRM") != "prepare-email-storage-key" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(env("GITHUB_SHA")) {
		return fail("guard")
	}
	dir := env("OPENBAO_RECOVERY_DIR")
	if !recoveryDirectory(dir) {
		return fail("inputs")
	}
	root, err := privateInput(filepath.Join(dir, "root-token"), 16384)
	if err != nil {
		return fail("inputs")
	}
	defer clear(root)
	raw, err := privateInput(filepath.Join(dir, "expected.json"), 1<<20)
	if err != nil {
		return fail("inputs")
	}
	defer clear(raw)
	var expected map[string]json.RawMessage
	if json.Unmarshal(raw, &expected) != nil || !recordsEqual(expected, expected) {
		return fail("inputs")
	}
	api, err := newAPI("http://127.0.0.1:38200")
	if err != nil {
		return fail("preparation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	state, err := api.prepareStorage(ctx, string(root), expected, rand.Reader)
	if err != nil || state != "CREATED" && state != "EXISTING" {
		return fail("preparation")
	}
	_, _ = fmt.Fprintf(out, "EMAIL_STORAGE_KEY=PREPARED state=%s recovered=false\n", state)
	return 0
}
