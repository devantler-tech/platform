package main

import (
	"errors"
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

// deployEntrypoints is everything the merge-queue deploy job runs itself: the
// two commands in its own steps and the action it calls. The tests keep this
// equal to the job in .github/workflows/ci.yaml, so the command needs no YAML
// parser when it runs inside that job.
var deployEntrypoints = []string{
	"scripts/verify-merge-group-queued",
	"scripts/production-inputs",
	".github/actions/deploy-prod",
}

// implicitInputs are files a tool the deploy runs loads without being told to, so no
// script names them. KSail reads ksail.yaml from the working directory on every command.
var implicitInputs = []string{
	"ksail.yaml",
}

// A repository path written out in a script, an action or a Go source file.
// docs/ is left out on purpose: scripts name runbooks in their messages and
// never read them, and the restricted working tree is what proves that.
var literalPath = regexp.MustCompile(`(?:\./)?((?:\.github|scripts|internal|third_party|k8s|talos|tests|schemas)/[A-Za-z0-9_./-]*[A-Za-z0-9_-])`)

// A path built from a variable or a command substitution, as in
// "${script_dir}/lib.sh", "$GUARD_DIR/approved.tsv" or "$(dirname "$0")/lib.sh". It is
// tried against the naming file's directory and the repository root.
var siblingPath = regexp.MustCompile(`(?:\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|[)"])/((?:\.\./)*[A-Za-z0-9_][A-Za-z0-9_./-]*[A-Za-z0-9_-])`)

// A module a JavaScript file imports by a relative path, as in
// `import {x} from './lib.mjs'`. It is resolved against the importing file.
var moduleImport = regexp.MustCompile("(?:\\bfrom|\\bimport|\\brequire)\\s*\\(?\\s*['\"`](\\.{1,2}/[A-Za-z0-9_./-]*[A-Za-z0-9_-])['\"`]")

// A directory named up to a template, as in "schemas/{{.Group}}/x.json": anything below
// it can be read.
var templatedPath = regexp.MustCompile(`(?:\./)?((?:\.github|scripts|internal|third_party|k8s|talos|tests|schemas)(?:/[A-Za-z0-9_.-]+)*)/\{\{`)

// tree is the tracked content the trace walks.
type tree struct {
	repository string
	files      map[string]bool
	// rootFiles matches each top-level file's name where it stands alone, as
	// "go.mod" does in a command but not inside "scripts/x/go.mod".
	rootFiles map[string]*regexp.Regexp
}

func newTree(repository string) (*tree, error) {
	files, err := tracked(repository)
	if err != nil {
		return nil, err
	}
	walked := &tree{repository: repository, files: map[string]bool{}, rootFiles: map[string]*regexp.Regexp{}}
	for _, file := range files {
		walked.files[file] = true
		if !strings.Contains(file, "/") {
			walked.rootFiles[file] = regexp.MustCompile(`(^|[^A-Za-z0-9_./-])` + regexp.QuoteMeta(file) + `($|[^A-Za-z0-9_/-])`)
		}
	}
	return walked, nil
}

// under returns the tracked files below a directory.
func (walked *tree) under(directory string) []string {
	prefix := strings.TrimSuffix(directory, "/") + "/"
	var files []string
	for file := range walked.files {
		if strings.HasPrefix(file, prefix) {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	return files
}

// followed reports whether a reached file can itself name further files.
func followed(file string) bool {
	if strings.HasSuffix(file, "_test.go") {
		return false
	}
	switch path.Ext(file) {
	case ".sh", ".bash", ".go", ".jq", ".mjs", ".js":
		return true
	case ".yaml", ".yml":
		// A tool's configuration at the repository root names files that tool reads.
		if !strings.Contains(file, "/") {
			return true
		}
	}
	return strings.HasPrefix(file, ".github/actions/") && path.Base(file) == "action.yml"
}

// code returns a file's lines without those that are only a comment.
func code(file, text string) string {
	marker := "#"
	if path.Ext(file) == ".go" || path.Ext(file) == ".mjs" || path.Ext(file) == ".js" {
		marker = "//"
	}
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		// A "shellcheck source=" comment declares a sourced file exactly.
		if !strings.HasPrefix(strings.TrimSpace(line), marker) || strings.Contains(line, "shellcheck source=") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// goPackages returns the repository directories a Go package is built from.
func (walked *tree) goPackages(directory string) ([]string, error) {
	command := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}{{end}}", "./"+directory)
	command.Dir = walked.repository
	// Read-only: listing packages must never rewrite go.mod or go.sum.
	command.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "GOWORK=off")
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("go list ./%s: %w: %s", directory, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, err
	}
	root, err := filepath.EvalSymlinks(walked.repository)
	if err != nil {
		return nil, err
	}
	var directories []string
	for _, line := range strings.Fields(string(output)) {
		resolved, err := filepath.EvalSymlinks(line)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || strings.HasPrefix(relative, "..") {
			continue
		}
		directories = append(directories, filepath.ToSlash(relative))
	}
	return directories, nil
}

// trace returns every tracked file the deploy job can name, directly or
// through a file it reaches.
func trace(repository string) ([]string, error) {
	walked, err := newTree(repository)
	if err != nil {
		return nil, err
	}
	reached := map[string]bool{}
	packages := map[string]bool{}
	type pending struct{ file, text string }
	queue := []pending{{file: listPath, text: strings.Join(append(append([]string{listPath}, deployEntrypoints...), implicitInputs...), "\n")}}

	var reach func(candidate string) error
	reach = func(candidate string) error {
		candidate = path.Clean(candidate)
		if walked.files[candidate] {
			if reached[candidate] {
				return nil
			}
			reached[candidate] = true
			if followed(candidate) {
				text, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(candidate)))
				if err != nil {
					return err
				}
				queue = append(queue, pending{file: candidate, text: code(candidate, string(text))})
			}
			return nil
		}
		below := walked.under(candidate)
		if len(below) == 0 {
			return nil
		}
		isPackage := false
		for _, file := range below {
			if path.Dir(file) == candidate && strings.HasSuffix(file, ".go") && !strings.HasSuffix(file, "_test.go") {
				isPackage = true
				break
			}
		}
		if !isPackage {
			for _, file := range below {
				if err := reach(file); err != nil {
					return err
				}
			}
			return nil
		}
		if packages[candidate] {
			return nil
		}
		packages[candidate] = true
		directories, err := walked.goPackages(candidate)
		if err != nil {
			return err
		}
		for _, directory := range directories {
			for _, file := range walked.under(directory) {
				// A package is its own directory: nested directories are other
				// packages, reached only if they are imported or named.
				if path.Dir(file) != directory || strings.HasSuffix(file, "_test.go") {
					continue
				}
				if err := reach(file); err != nil {
					return err
				}
			}
		}
		return nil
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, match := range literalPath.FindAllStringSubmatch(current.text, -1) {
			if err := reach(match[1]); err != nil {
				return nil, err
			}
		}
		for _, match := range templatedPath.FindAllStringSubmatch(current.text, -1) {
			if err := reach(match[1]); err != nil {
				return nil, err
			}
		}
		if extension := path.Ext(current.file); extension == ".mjs" || extension == ".js" {
			for _, match := range moduleImport.FindAllStringSubmatch(current.text, -1) {
				candidate := path.Join(path.Dir(current.file), match[1])
				if strings.HasPrefix(candidate, "..") {
					continue
				}
				// An import that names no tracked file (one written without its extension,
				// say) is one the trace cannot follow, so it must not pass as followed.
				if !walked.files[candidate] && len(walked.under(candidate)) == 0 {
					return nil, fmt.Errorf("%s imports %s, which is not a tracked file", current.file, match[1])
				}
				if err := reach(candidate); err != nil {
					return nil, err
				}
			}
		}
		for root, named := range walked.rootFiles {
			if named.MatchString(current.text) {
				if err := reach(root); err != nil {
					return nil, err
				}
			}
		}
		for _, match := range siblingPath.FindAllStringSubmatch(current.text, -1) {
			for _, base := range []string{path.Dir(current.file), "."} {
				candidate := path.Join(base, match[1])
				// A bare top-level directory ("$root/scripts") names no file to
				// read; whatever is read below it is named further on.
				topLevelDirectory := !strings.Contains(candidate, "/") && !walked.files[candidate]
				if strings.HasPrefix(candidate, "..") || strings.HasPrefix(candidate, "docs/") || topLevelDirectory {
					continue
				}
				if err := reach(candidate); err != nil {
					return nil, err
				}
			}
		}
	}

	files := make([]string, 0, len(reached))
	for file := range reached {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
}

// uncovered returns the traced files the list does not hold.
func uncovered(repository string) (traced, missing []string, err error) {
	text, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(listPath)))
	if err != nil {
		return nil, nil, err
	}
	list, err := parseInputs(string(text))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", listPath, err)
	}
	traced, err = trace(repository)
	if err != nil {
		return nil, nil, err
	}
	for _, file := range traced {
		if !list.covers(file) {
			missing = append(missing, file)
		}
	}
	return traced, missing, nil
}

// reachable prints the trace and fails when the list misses part of it.
func reachable(repository string, stdout io.Writer) int {
	traced, missing, err := uncovered(repository)
	if err != nil {
		fmt.Fprintln(os.Stderr, "production-inputs:", err)
		return 2
	}
	if _, err := fmt.Fprintln(stdout, strings.Join(traced, "\n")); err != nil {
		fmt.Fprintln(os.Stderr, "production-inputs:", err)
		return 2
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "production-inputs: the deploy can reach %d files that %s does not list:\n", len(missing), listPath)
		for _, file := range missing {
			fmt.Fprintln(os.Stderr, "  "+file)
		}
		return 1
	}
	return 0
}
