// Package main runs the pinned Revive engine with complete package context.
// Explicit file arguments make Revive treat each file as a separate package.
// Directory targets preserve package context; fallback files retain coverage of
// sources excluded by the native resolver's platform or build-tag selection.
package main

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

func main() {
	if err := run(); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run() error {
	selected := []string(nil)
	if len(os.Args) > 1 {
		if len(os.Args) < 3 || os.Args[1] != "--from-megalinter" {
			return errors.New("usage: check-go-package-style [--from-megalinter tracked.go ...]")
		}
		selected = os.Args[2:]
	}
	binary, err := exec.LookPath("revive")
	if err != nil {
		return fmt.Errorf("find pinned revive: %w", err)
	}
	version, err := exec.Command(binary, "-version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "version 1.16.0" {
		return errors.New("revive 1.16.0 is required")
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil || goSeries(info.GoVersion) == "" || goSeries(info.GoVersion) != goSeries(runtime.Version()) {
		return errors.New("revive and the coverage resolver must use the same Go release series")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	gitRoot, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("discover repository root: %w", err)
	}
	physicalGitRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(gitRoot)))
	if err != nil || physicalRoot != physicalGitRoot {
		return errors.New("complete package-style lint must run from the repository root")
	}
	root = physicalRoot
	command := exec.Command("git", "ls-files", "-z", "--", "*.go")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("discover tracked Go sources: %w", err)
	}
	paths := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	for _, path := range selected {
		if !slices.Contains(paths, path) {
			return errors.New("the caller selected an unknown or untracked Go source")
		}
	}
	args, directories, fallbacks, err := targets(root, paths, build.Default)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(os.Stdout, "REVIVE tracked=%d directories=%d fallback-files=%d\n", len(paths), directories, fallbacks); err != nil {
		return err
	}
	command = exec.Command(binary, append([]string{"-set_exit_status", "--"}, args...)...)
	command.Dir = root
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("complete package-style lint failed: %w", err)
	}
	return nil
}

func goSeries(version string) string {
	if !strings.HasPrefix(version, "go1.") {
		return ""
	}
	minor := strings.Split(strings.TrimPrefix(version, "go1."), ".")[0]
	if _, err := strconv.Atoi(minor); err != nil {
		return ""
	}
	return "go1." + minor
}

func targets(root string, paths []string, context build.Context) ([]string, int, int, error) {
	if len(paths) == 0 {
		return nil, 0, 0, errors.New("tracked Go inventory is empty")
	}
	tracked, dirs := map[string]bool{}, map[string]bool{}
	for _, path := range paths {
		if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, "..") || strings.ContainsAny(path, "\r\n\t") || !strings.HasSuffix(path, ".go") || tracked[path] {
			return nil, 0, 0, errors.New("invalid or duplicate tracked Go path")
		}
		absolute := filepath.Join(root, path)
		physical, err := filepath.EvalSymlinks(absolute)
		if err != nil || physical != absolute {
			return nil, 0, 0, fmt.Errorf("missing or linked Go source %s", path)
		}
		stat, err := os.Stat(absolute)
		if err != nil || !stat.Mode().IsRegular() {
			return nil, 0, 0, fmt.Errorf("non-regular Go source %s", path)
		}
		tracked[path], dirs[filepath.Dir(path)] = true, true
	}
	args := make([]string, 0, len(dirs))
	for dir := range dirs {
		args = append(args, dir)
	}
	slices.Sort(args)
	directories := len(args)
	resolved := map[string]bool{}
	for _, dir := range args {
		// This matches revive v1.16.0's dots v1.0.0 directory resolver.
		pkg, err := context.ImportDir(filepath.Join(root, dir), 0)
		var noGo *build.NoGoError
		if err != nil && !errors.As(err, &noGo) {
			return nil, 0, 0, fmt.Errorf("resolve package %s: %w", dir, err)
		}
		if pkg == nil {
			continue
		}
		for _, group := range [][]string{pkg.GoFiles, pkg.CgoFiles, pkg.TestGoFiles, pkg.XTestGoFiles} {
			for _, file := range group {
				path := filepath.Join(dir, file)
				if !tracked[path] {
					return nil, 0, 0, fmt.Errorf("native package resolver includes untracked source %s", path)
				}
				resolved[path] = true
			}
		}
	}
	fallbacks := []string{}
	for path := range tracked {
		if !resolved[path] {
			fallbacks = append(fallbacks, path)
		}
	}
	slices.Sort(fallbacks)
	return append(args, fallbacks...), directories, len(fallbacks), nil
}
