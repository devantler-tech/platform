// Command guard-grype-source retains complete native SCA while binding its
// source-only adjudications to an immutable checkout and real refusal proofs.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/acrapplicability"
	"github.com/devantler-tech/platform/internal/celapplicability"
	"github.com/devantler-tech/platform/internal/scansnapshot"
	"github.com/devantler-tech/platform/internal/scanvex"
	"gopkg.in/yaml.v3"
)

func main() {
	status, err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guard-grype-source:", err)
		status = 2
	}
	os.Exit(status)
}

func configuration(args []string) ([]byte, []string, error) {
	defaults := []byte("fail-on-severity: high\nignore:\n  - vulnerability: CVE-2018-20225\n")
	if slices.Equal(args, []string{"dir:."}) {
		return defaults, nil, nil
	}
	if len(args) != 7 || args[0] != "--config" || !filepath.IsAbs(args[1]) || !slices.Equal(args[2:], []string{"--exclude", "**/.agents", "--exclude", "**/megalinter-reports", "dir:."}) {
		return nil, nil, fmt.Errorf("require the complete native directory command and unchanged exclusions")
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		return nil, nil, err
	}
	var config struct {
		Severity string `yaml:"fail-on-severity"`
		Ignore   []struct {
			Vulnerability string `yaml:"vulnerability"`
		} `yaml:"ignore"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, nil, fmt.Errorf("unknown native Grype configuration: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || config.Severity != "high" || len(config.Ignore) != 1 || config.Ignore[0].Vulnerability != "CVE-2018-20225" {
		return nil, nil, fmt.Errorf("the reviewed native severity/ignore profile changed")
	}
	return data, slices.Clone(args[2 : len(args)-1]), nil
}

func environment() []string {
	var result []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GRYPE_") && !strings.HasPrefix(entry, "SYFT_") {
			result = append(result, entry)
		}
	}
	return result
}

func receipt(name, head string, value any) error {
	directory := filepath.Join("megalinter-reports", "source-applicability")
	for _, path := range []string{"megalinter-reports", directory} {
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid or linked native report directory")
		}
	}
	file, err := os.CreateTemp(directory, name+"-"+head+"-*.json")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) < 2 || !filepath.IsAbs(args[0]) {
		return 2, fmt.Errorf("an absolute native Grype engine and complete source command are required")
	}
	engine, args := args[0], args[1:]
	metadata := slices.Equal(args, []string{"--version"}) || slices.Equal(args, []string{"--help"})
	if metadata {
		cmd := exec.CommandContext(ctx, engine, args...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.Env = environment()
		if err := cmd.Run(); err != nil {
			return 2, err
		}
		return 0, nil
	}
	config, excludes, err := configuration(args)
	if err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	versionCmd := exec.CommandContext(ctx, engine, "--version")
	versionCmd.Env = environment()
	version, err := versionCmd.Output()
	if err != nil || strings.TrimSpace(string(version)) != "grype 0.118.0" {
		return 2, fmt.Errorf("native Grype 0.118.0 is required")
	}
	snapshot, cleanup, err := scansnapshot.Clone(ctx, ".")
	if err != nil {
		return 2, err
	}
	defer cleanup()
	cel, err := celapplicability.Prove(ctx, snapshot.Root)
	if err != nil {
		return 2, err
	}
	acr, err := acrapplicability.Prove(ctx, snapshot.Root)
	if err != nil {
		return 2, err
	}
	if acr.Status != "not_affected" || acr.Head != snapshot.Head || cel.Head != snapshot.Head {
		return 2, fmt.Errorf("applicability proof does not bind the native source commit")
	}
	paths := make([]string, 0, len(cel.Scopes))
	for _, scope := range cel.Scopes {
		paths = append(paths, scope.GoModPath)
	}
	scopes := []scanvex.Scope{
		{IDs: append([]string{cel.Vulnerability}, cel.Aliases...), Module: "github.com/google/cel-go", Version: "v0.26.0", Paths: paths, Justification: cel.Justification},
		{IDs: append([]string{acr.AdvisoryID}, acr.Aliases...), Module: acr.Module, Version: acr.Version, Paths: []string{acr.Owner}, Justification: acr.Justification},
	}
	if err := receipt("grype-cel", snapshot.Head, cel); err != nil {
		return 2, err
	}
	if err := receipt("grype-acr", snapshot.Head, acr); err != nil {
		return 2, err
	}
	work, err := os.MkdirTemp("", "platform-grype-")
	if err != nil {
		return 2, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	vex := filepath.Join(work, "source-applicability.json")
	if err := scanvex.Write(vex, scopes); err != nil {
		return 2, err
	}
	configPath := filepath.Join(work, "native.grype.yaml")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		return 2, err
	}
	if err := snapshot.Verify(); err != nil {
		return 2, err
	}
	if _, err := fmt.Fprintf(stderr, "guard-grype-source: complete source commit %s; native config SHA256 %x; source-bound CEL and paired ACR refusal proofs\n", snapshot.Head, sha256.Sum256(config)); err != nil {
		return 2, err
	}
	full := append([]string{"--config", configPath}, excludes...)
	full = append(full, "--vex", vex, "--show-suppressed", "--output", "json", "dir:.")
	cmd := exec.CommandContext(ctx, engine, full...)
	cmd.Dir = snapshot.Root
	cmd.Env = environment()
	var out, log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &log
	scanErr := cmd.Run()
	if _, err := stdout.Write(out.Bytes()); err != nil {
		return 2, err
	}
	if _, err := stderr.Write(log.Bytes()); err != nil {
		return 2, err
	}
	if ctx.Err() != nil {
		return 2, fmt.Errorf("native source scan exceeded its bounded command")
	}
	status := 0
	if scanErr != nil {
		exit, ok := scanErr.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 {
			return 2, fmt.Errorf("native Grype scan did not complete: %w", scanErr)
		}
		status = exit.ExitCode()
	}
	active, err := scanvex.ValidateGrype(out.Bytes(), scopes)
	if err != nil {
		return 2, err
	}
	if err := snapshot.Verify(); err != nil {
		return 2, err
	}
	if active > 0 && status == 0 {
		status = 1
	}
	return status, nil
}
