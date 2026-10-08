package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	fetchAttempts = 3
	fetchTimeout  = time.Minute
)

// ksailRepository is the public source the pinned KSail release is built from.
// It and fetchBackoff are variables so tests can stand in a local source and
// avoid waiting.
var (
	ksailRepository = "https://github.com/devantler-tech/ksail.git"
	fetchBackoff    = 5 * time.Second
)

var (
	releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	gitObjectID    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// foldInputs are the parts of the KSail source the mirrored fold depends on,
// with the git object type each must have. A tree's object ID covers every
// file below it, so equal IDs mean byte-identical content.
var foldInputs = []struct{ path, kind string }{
	{"pkg/fsutil/configmanager", "tree"},
	{"pkg/fsutil/generator/talos", "tree"},
	{"pkg/apis", "tree"},
	{"charts", "tree"},
	{"go.mod", "blob"},
	{"go.sum", "blob"},
}

// auditedFoldInputs records every set of fold inputs whose fold was reviewed
// against this mirror. A release passes when all its inputs equal one set.
// Add a set only with the source audit README.md describes.
var auditedFoldInputs = []struct {
	audit string
	ids   map[string]string
}{
	{"7.193.6, 7.193.8 and 7.194.0", map[string]string{
		"pkg/fsutil/configmanager":   "d34f5f8c4f6467f6fc3f11f8bd791c904a5e50b6",
		"pkg/fsutil/generator/talos": "8959ca398398f7bde09c312c362de7fb80936073",
		"pkg/apis":                   "a23c242005321f3a03aa39ac026122ac0cabab4d",
		"charts":                     "1ddd64ee9b29fcd14402838ec9bba7795edd0fa9",
		"go.mod":                     "a77ef7b4247c1735ba84705c6570427a3c681ec1",
		"go.sum":                     "12ab4ed14cc66ea4f526188dd1c4dbc3d30e74a0",
	}},
	{"7.194.3 and 7.194.4", map[string]string{
		"pkg/fsutil/configmanager":   "d34f5f8c4f6467f6fc3f11f8bd791c904a5e50b6",
		"pkg/fsutil/generator/talos": "8959ca398398f7bde09c312c362de7fb80936073",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "e7d6122e499761d5a12948422176f33c2f1a853a",
		"go.sum":                     "c43cb045162b328ad0dd7f9311fb63133336841b",
	}},
	{"7.194.5", map[string]string{
		"pkg/fsutil/configmanager":   "d34f5f8c4f6467f6fc3f11f8bd791c904a5e50b6",
		"pkg/fsutil/generator/talos": "8959ca398398f7bde09c312c362de7fb80936073",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "b41d0e9d2219f2b1a497d122a4c8926e53d60aa8",
		"go.sum":                     "c43cb045162b328ad0dd7f9311fb63133336841b",
	}},
	{"7.194.7 and 7.194.8", map[string]string{
		"pkg/fsutil/configmanager":   "439ee0a33cf6ca597f5773bcb6db316900cb8149",
		"pkg/fsutil/generator/talos": "8959ca398398f7bde09c312c362de7fb80936073",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "b41d0e9d2219f2b1a497d122a4c8926e53d60aa8",
		"go.sum":                     "c43cb045162b328ad0dd7f9311fb63133336841b",
	}},
	{"7.194.9, 7.194.10, 7.195.0 and 7.195.1", map[string]string{
		"pkg/fsutil/configmanager":   "439ee0a33cf6ca597f5773bcb6db316900cb8149",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "b41d0e9d2219f2b1a497d122a4c8926e53d60aa8",
		"go.sum":                     "c43cb045162b328ad0dd7f9311fb63133336841b",
	}},
	{"7.197.1", map[string]string{
		"pkg/fsutil/configmanager":   "439ee0a33cf6ca597f5773bcb6db316900cb8149",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "285b0d20d7d8dffdfd7fb003be65c65d6c08c71f",
		"charts":                     "59bae68ff67c9d02e171cdfe123ec1ac82fccc49",
		"go.mod":                     "e7265f2eee822033c9047eb50e7720872756aa03",
		"go.sum":                     "3e87842cfa29b03933d8a57c23204b340cf3c2b7",
	}},
	{"7.202.3", map[string]string{
		"pkg/fsutil/configmanager":   "ac334b6ba5b7e474bfa88e13486a1ab0d1d6feea",
		"pkg/fsutil/generator/talos": "25c9c416e06b77937e68e71a213c1b2e9ed61205",
		"pkg/apis":                   "83982b42d9ec0b34d159783969c39ff8832e6714",
		"charts":                     "c4ed7098f7dce6dca0c65649cd8ab3aaeccdf6f4",
		"go.mod":                     "3ef4674d6c9d1507ec198883c78712ce74819866",
		"go.sum":                     "c780001120d55f3eb85838940f5f149e62202fab",
	}},
}

// verifyFoldInputs passes only when every fold input of the pinned release was
// read and all of them equal one audited set. It returns the releases that set
// was audited at. A failed or incomplete read is an error, never a pass.
func verifyFoldInputs(version string, resolve func(version string) (map[string]string, error)) (string, error) {
	got, err := resolve(version)
	if err != nil {
		return "", fmt.Errorf("KSail %s fold inputs could not be read, so the fold is unverified: %w", version, err)
	}
	for _, input := range foldInputs {
		if !gitObjectID.MatchString(got[input.path]) {
			return "", fmt.Errorf("KSail %s fold input %s was not read, so the fold is unverified", version, input.path)
		}
	}
	// Report the difference against the nearest audited set, preferring the
	// most recent one, so the message names what a new audit has to cover.
	var nearest []string
	nearestAudit := ""
	for i := len(auditedFoldInputs) - 1; i >= 0; i-- {
		set := auditedFoldInputs[i]
		var changed []string
		for _, input := range foldInputs {
			if got[input.path] != set.ids[input.path] {
				changed = append(changed, input.path)
			}
		}
		if len(changed) == 0 {
			return set.audit, nil
		}
		if nearestAudit == "" || len(changed) < len(nearest) {
			nearest, nearestAudit = changed, set.audit
		}
	}
	if nearestAudit == "" {
		return "", fmt.Errorf("no audited KSail fold inputs are recorded")
	}
	sort.Strings(nearest)
	return "", fmt.Errorf("KSail %s changes fold input(s) %s since the set audited at %s; this requires a new source audit", version, strings.Join(nearest, ", "), nearestAudit)
}

// resolveFoldInputs reads the object ID of every fold input at the release tag
// of version from repository. It fetches the tagged commit's trees only.
func resolveFoldInputs(repository, version string) (map[string]string, error) {
	if !releaseVersion.MatchString(version) {
		return nil, fmt.Errorf("%q is not a release version", version)
	}
	dir, err := os.MkdirTemp("", "ksail-fold-inputs-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	// Each command gets its own deadline, so one hung fetch cannot use up the
	// time the retries need.
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		// Ignore user, system and inherited configuration: a URL rewrite or a
		// GIT_DIR from the caller would make this read a different source than
		// the one named above, or write into the caller's repository.
		cmd.Env = append(withoutGitVariables(os.Environ()), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.Output()
		if err != nil {
			detail := ""
			if exit, ok := err.(*exec.ExitError); ok {
				detail = ": " + strings.TrimSpace(string(exit.Stderr))
			}
			return "", fmt.Errorf("git %s: %w%s", strings.Join(args, " "), err, detail)
		}
		return string(out), nil
	}
	if _, err := git("init", "--quiet"); err != nil {
		return nil, err
	}
	tag := "refs/tags/v" + version
	// One transient failure must not fail every pull request and deploy, so the
	// fetch is retried. An exhausted retry is still an error, never a pass.
	var fetchErrs []error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(fetchBackoff * time.Duration(attempt))
		}
		_, err := git("fetch", "--quiet", "--no-tags", "--depth=1", "--filter=blob:none", repository, tag)
		if err == nil {
			fetchErrs = nil
			break
		}
		fetchErrs = append(fetchErrs, err)
	}
	if len(fetchErrs) > 0 {
		return nil, errors.Join(fetchErrs...)
	}
	ids := map[string]string{}
	for _, input := range foldInputs {
		out, err := git("ls-tree", "FETCH_HEAD", "--", input.path)
		if err != nil {
			return nil, err
		}
		// One entry: "<mode> <type> <id>\t<path>".
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		meta, path, found := strings.Cut(lines[0], "\t")
		fields := strings.Fields(meta)
		if len(lines) != 1 || !found || path != input.path || len(fields) != 3 {
			return nil, fmt.Errorf("%s has no %s at %s", repository, input.path, tag)
		}
		if fields[1] != input.kind {
			return nil, fmt.Errorf("%s at %s is a %s, expected a %s", input.path, tag, fields[1], input.kind)
		}
		if !gitObjectID.MatchString(fields[2]) {
			return nil, fmt.Errorf("%s at %s has an unrecognised object ID %q", input.path, tag, fields[2])
		}
		ids[input.path] = fields[2]
	}
	return ids, nil
}

func withoutGitVariables(environment []string) []string {
	var kept []string
	for _, variable := range environment {
		if !strings.HasPrefix(variable, "GIT_") {
			kept = append(kept, variable)
		}
	}
	return kept
}
