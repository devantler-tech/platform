// Command production-inputs answers one question about a queued change: could
// it alter what a production deploy does?
//
// The production inputs are declared in inputs.txt beside this file: one
// repository path per line, a directory ending in "/". The merge-queue deploy
// runs from a working tree that holds only those paths (restrict), so a file
// outside the list cannot influence a deploy, and a dependency the list misses
// fails that deploy instead of passing unnoticed. A queued change therefore
// alters production only when it changes a listed path (changed).
//
// Usage:
//
//	go run ./scripts/production-inputs changed --base <sha> --head <sha>
//	go run ./scripts/production-inputs restrict
//	go run ./scripts/production-inputs reachable
//
// changed prints "production=true" or "production=false" and appends the same
// line to $GITHUB_OUTPUT. It answers true whenever it cannot decide: an
// unreadable commit, a list it cannot parse at either revision, or a failed
// comparison. A path counts when the list at the base or at the head covers
// it, so a change cannot take a file off the list and alter it in one step.
//
// restrict removes every tracked file the list does not cover from the working
// tree with git sparse-checkout, then verifies the result file by file. It
// refuses a list entry that matches nothing.
//
// reachable prints the tracked files a static trace reaches from the deploy
// action, and exits 1 when the list does not cover one of them. The trace is a
// second check, not the proof: it reads every branch of every script, which a
// single deploy does not exercise.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// listPath is the declared list, relative to the repository root.
const listPath = "scripts/production-inputs/inputs.txt"

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// inputs is a parsed list: directories keep their trailing slash.
type inputs struct {
	dirs  []string
	files map[string]bool
}

// parseInputs reads the list format. It refuses anything that is not a plain
// repository-relative path, because the same lines become sparse-checkout
// patterns, where a wildcard or a negation would mean something else.
func parseInputs(text string) (inputs, error) {
	list := inputs{files: map[string]bool{}}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	for number := 1; scanner.Scan(); number++ {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line != strings.TrimSpace(line) {
			return inputs{}, fmt.Errorf("line %d: surrounding whitespace", number)
		}
		if strings.ContainsAny(line, "*?[]!\\\"' \t") {
			return inputs{}, fmt.Errorf("line %d: %q is not a plain path", number, line)
		}
		directory := strings.HasSuffix(line, "/")
		clean := path.Clean(line)
		if directory {
			clean += "/"
		}
		if clean != line || strings.HasPrefix(line, "/") || strings.HasPrefix(line, "../") || line == "../" || line == "./" {
			return inputs{}, fmt.Errorf("line %d: %q is not a normalized repository path", number, line)
		}
		if seen[line] {
			return inputs{}, fmt.Errorf("line %d: %q is listed twice", number, line)
		}
		seen[line] = true
		if directory {
			list.dirs = append(list.dirs, line)
		} else {
			list.files[line] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return inputs{}, err
	}
	if len(seen) == 0 {
		return inputs{}, errors.New("the list is empty")
	}
	sort.Strings(list.dirs)
	return list, nil
}

// covers reports whether the list holds the path.
func (list inputs) covers(file string) bool {
	if list.files[file] {
		return true
	}
	for _, directory := range list.dirs {
		if strings.HasPrefix(file, directory) {
			return true
		}
	}
	return false
}

// entries returns every list entry, sorted.
func (list inputs) entries() []string {
	all := append([]string{}, list.dirs...)
	for file := range list.files {
		all = append(all, file)
	}
	sort.Strings(all)
	return all
}

// git runs one git command in the repository and returns its standard output.
func git(repository string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", repository, "--no-replace-objects"}, arguments...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// splitNUL splits git's NUL-terminated output into its records.
func splitNUL(output []byte) []string {
	var records []string
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) > 0 {
			records = append(records, string(record))
		}
	}
	return records
}

// listAt reads and parses the list as one commit holds it.
func listAt(repository, commit string) (inputs, error) {
	text, err := git(repository, "show", commit+":"+listPath)
	if err != nil {
		return inputs{}, err
	}
	return parseInputs(string(text))
}

// decision is the answer for one queued change and the evidence behind it.
type decision struct {
	production bool
	reason     string
	paths      []string
}

// decide compares two commits. Every failure answers production with the
// reason named, so no error can stand in for "nothing changed".
func decide(repository, base, head string) decision {
	deploy := func(format string, values ...any) decision {
		return decision{production: true, reason: fmt.Sprintf(format, values...)}
	}
	if !shaPattern.MatchString(base) || !shaPattern.MatchString(head) {
		return deploy("the base and head must be full commit ids")
	}
	for _, commit := range []string{base, head} {
		if _, err := git(repository, "cat-file", "-e", commit+"^{commit}"); err != nil {
			return deploy("commit %s cannot be read", commit)
		}
	}
	headList, err := listAt(repository, head)
	if err != nil {
		return deploy("the list at the head cannot be used: %v", err)
	}
	baseList, err := listAt(repository, base)
	if err != nil {
		return deploy("the list at the base cannot be used: %v", err)
	}
	output, err := git(repository, "diff", "--name-only", "-z", "--no-renames", base, head, "--")
	if err != nil {
		return deploy("the comparison failed: %v", err)
	}
	changed := splitNUL(output)
	if len(changed) == 0 {
		return deploy("the base and head hold the same files, which a queued change never does")
	}
	var touched []string
	for _, file := range changed {
		if baseList.covers(file) || headList.covers(file) {
			touched = append(touched, file)
		}
	}
	if len(touched) > 0 {
		return decision{production: true, reason: fmt.Sprintf("%d of %d changed files are production inputs", len(touched), len(changed)), paths: touched}
	}
	return decision{reason: fmt.Sprintf("none of the %d changed files is a production input", len(changed)), paths: changed}
}

// report writes the decision for people and for the workflow.
func report(result decision, stdout io.Writer) error {
	answer := fmt.Sprintf("production=%t", result.production)
	if _, err := fmt.Fprintln(stdout, answer); err != nil {
		return err
	}
	summary := fmt.Sprintf("- Production inputs: `%s` (%s).", answer, result.reason)
	for index, file := range result.paths {
		if index == 10 {
			summary += fmt.Sprintf("\n  - and %d more", len(result.paths)-index)
			break
		}
		summary += "\n  - `" + file + "`"
	}
	fmt.Fprintln(os.Stderr, summary)
	for variable, text := range map[string]string{"GITHUB_OUTPUT": answer, "GITHUB_STEP_SUMMARY": summary} {
		target := os.Getenv(variable)
		if target == "" {
			continue
		}
		file, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(file, text); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

// tracked returns every file the checked-out commit tracks.
func tracked(repository string) ([]string, error) {
	output, err := git(repository, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	return splitNUL(output), nil
}

// staleEntries returns the list entries that match no tracked file.
func staleEntries(list inputs, files []string) []string {
	var stale []string
	for _, entry := range list.entries() {
		matched := false
		for _, file := range files {
			if file == entry || (strings.HasSuffix(entry, "/") && strings.HasPrefix(file, entry)) {
				matched = true
				break
			}
		}
		if !matched {
			stale = append(stale, entry)
		}
	}
	return stale
}

// restrict leaves only the listed paths in the working tree and proves it.
func restrict(repository string, stdout io.Writer) error {
	text, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(listPath)))
	if err != nil {
		return err
	}
	list, err := parseInputs(string(text))
	if err != nil {
		return fmt.Errorf("%s: %w", listPath, err)
	}
	files, err := tracked(repository)
	if err != nil {
		return err
	}
	if stale := staleEntries(list, files); len(stale) > 0 {
		return fmt.Errorf("%s lists paths the repository does not track: %s", listPath, strings.Join(stale, ", "))
	}

	// An anchored pattern per entry: "/k8s/" keeps that directory and nothing
	// named k8s deeper in the tree.
	var patterns strings.Builder
	for _, entry := range list.entries() {
		patterns.WriteString("/" + entry + "\n")
	}
	command := exec.Command("git", "-C", repository, "sparse-checkout", "set", "--no-cone", "--stdin")
	command.Stdin = strings.NewReader(patterns.String())
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git sparse-checkout: %w: %s", err, strings.TrimSpace(string(output)))
	}

	kept := 0
	for _, file := range files {
		_, statErr := os.Lstat(filepath.Join(repository, filepath.FromSlash(file)))
		present := statErr == nil
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		switch covered := list.covers(file); {
		case covered && !present:
			return fmt.Errorf("production input %s is missing from the working tree", file)
		case !covered && present:
			return fmt.Errorf("%s is not a production input but is still in the working tree", file)
		case covered:
			kept++
		}
	}
	_, err = fmt.Fprintf(stdout, "The deploy working tree holds %d of %d tracked files: the declared production inputs.\n", kept, len(files))
	return err
}

func run(arguments []string, stdout io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(os.Stderr, "usage: production-inputs changed --base <sha> --head <sha> | restrict | reachable")
		return 2
	}
	repository, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	switch arguments[0] {
	case "changed":
		flags := flag.NewFlagSet("changed", flag.ContinueOnError)
		base := flags.String("base", "", "commit the queued change is built on")
		head := flags.String("head", "", "commit that holds the queued change")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return 2
		}
		if err := report(decide(repository, *base, *head), stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	case "restrict":
		if len(arguments) != 1 {
			return 2
		}
		if err := restrict(repository, stdout); err != nil {
			fmt.Fprintln(os.Stderr, "production-inputs:", err)
			return 1
		}
		return 0
	case "reachable":
		if len(arguments) != 1 {
			return 2
		}
		return reachable(repository, stdout)
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", arguments[0])
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}
