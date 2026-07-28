package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkFile is the command's own sequence — parse, then render any findings —
// so these tests assert on the message an operator actually sees.
func checkFile(path string, source []byte) error {
	findings, err := validateFile(path, source)
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}

	return errors.New(formatReport(path, findings))
}

// Each case is a whole workflow so the test exercises the same traversal the
// command runs, not a hand-built node tree.

const workflowLevelValid = `name: CD

on: push

concurrency:
  group: prod-deploy
  cancel-in-progress: false
  queue: max

jobs:
  deploy:
    runs-on: ubuntu-latest
`

const workflowLevelTypo = `name: CD

on: push

concurrency:
  group: prod-deploy
  cancel-in-progress: false
  queue: maxx

jobs:
  deploy:
    runs-on: ubuntu-latest
`

const jobLevelTypo = `name: CI

on: push

jobs:
  deploy-prod:
    runs-on: ubuntu-latest
    concurrency:
      group: prod-deploy
      cancel-in-progress: false
      queue: sngle
`

// Both tiers wrong in one file: the reporter must not stop at the first.
const bothTiersInvalid = `name: CI

on: push

concurrency:
  group: outer
  queue: nope

jobs:
  deploy-prod:
    runs-on: ubuntu-latest
    concurrency:
      group: prod-deploy
      queue: alsonope
`

func TestValidateFileAcceptsTheEnum(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"workflow-level max":    workflowLevelValid,
		"workflow-level single": strings.Replace(workflowLevelValid, "queue: max", "queue: single", 1),
		// A quoted scalar is the same value; a line matcher would miss this.
		"quoted value": strings.Replace(workflowLevelValid, "queue: max", `queue: "max"`, 1),
		// An inline comment must not become part of the value.
		"trailing comment": strings.Replace(workflowLevelValid, "queue: max", "queue: max # serialize", 1),
		// concurrency may be a bare group string, with no queue at all.
		"scalar concurrency": "name: CI\non: push\nconcurrency: prod-deploy\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		// No concurrency block anywhere.
		"absent": "name: CI\non: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		// A queue key outside a concurrency block is none of this check's business.
		"unrelated queue key": "name: CI\non: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    env:\n      queue: whatever\n",
	}

	for name, workflow := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := checkFile("w.yaml", []byte(workflow)); err != nil {
				t.Fatalf("expected %s to pass, got: %v", name, err)
			}
		})
	}
}

func TestValidateFileRejectsValuesOutsideTheEnum(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		workflow  string
		wantValue string
	}{
		"workflow-level typo": {workflowLevelTypo, "maxx"},
		"job-level typo":      {jobLevelTypo, "sngle"},
		// GitHub's enum is lower-case; a capitalised value is rejected at run time.
		"wrong case": {strings.Replace(workflowLevelValid, "queue: max", "queue: Max", 1), "Max"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := checkFile("w.yaml", []byte(tc.workflow))
			if err == nil {
				t.Fatalf("expected %s to fail, but it passed", name)
			}
			if !strings.Contains(err.Error(), tc.wantValue) {
				t.Errorf("error should name the offending value %q, got: %v", tc.wantValue, err)
			}
			// Fail with the fix: the message must say what to put there instead.
			if !strings.Contains(err.Error(), "single") || !strings.Contains(err.Error(), "max") {
				t.Errorf("error should name both valid values, got: %v", err)
			}
		})
	}
}

func TestValidateFileReportsEveryInvalidQueueNotJustTheFirst(t *testing.T) {
	t.Parallel()

	err := checkFile("w.yaml", []byte(bothTiersInvalid))
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"nope", "alsonope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected both tiers reported; %q missing from: %v", want, err)
		}
	}
}

func TestValidateFileRejectsNonScalarQueue(t *testing.T) {
	t.Parallel()

	workflow := "name: CI\non: push\nconcurrency:\n  group: g\n  queue:\n    - max\n"
	if err := checkFile("w.yaml", []byte(workflow)); err == nil {
		t.Fatal("a list-valued queue is not a valid enum member, expected failure")
	}
}

func TestRunFailsClosedOnAnUnreadableFile(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	// A workflow that cannot be read has not been validated, so this must fail
	// rather than silently contribute nothing.
	if err := run([]string{filepath.Join(t.TempDir(), "absent.yaml")}, &stderr); err == nil {
		t.Fatal("expected failure on an unreadable workflow")
	}
	if !strings.Contains(stderr.String(), "could not read") {
		t.Errorf("expected a read error on stderr, got: %q", stderr.String())
	}
}

func TestRunRequiresAtLeastOnePath(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	// Called with no paths, the command must not report success having checked
	// nothing — that would pass a CI step whose glob silently matched no files.
	if err := run(nil, &stderr); err == nil {
		t.Fatal("expected failure when no paths are supplied")
	}
}

func TestRunAcceptsRealValidWorkflows(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "cd.yaml")
	if err := os.WriteFile(path, []byte(workflowLevelValid), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var stderr bytes.Buffer
	if err := run([]string{path}, &stderr); err != nil {
		t.Fatalf("expected a valid workflow to pass, got: %v (stderr %q)", err, stderr.String())
	}
}
