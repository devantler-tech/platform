// Command guard-checkov-source binds native resource dispositions to the same
// immutable source commit without changing native check or framework selection.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/scansnapshot"
	"gopkg.in/yaml.v3"
)

func main() {
	status, err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guard-checkov-source:", err)
		status = 2
	}
	os.Exit(status)
}

func arguments(args []string) (string, error) {
	if len(args) != 12 || !filepath.IsAbs(args[3]) || !slices.Equal(args[:3], []string{"--skip-path", "tests/", "--config-file"}) || !slices.Equal(args[4:], []string{"--skip-path", ".agents", "--skip-path", "megalinter-reports", "--directory", ".", "--skip-framework", "secrets"}) {
		return "", fmt.Errorf("require the unchanged complete native directory, framework and source command")
	}
	return args[3], nil
}

func configuration(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config struct {
		Quiet bool     `yaml:"quiet"`
		Skip  []string `yaml:"skip-check"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || !config.Quiet || !slices.Equal(config.Skip, []string{"CKV_DOCKER_2"}) {
		return nil, fmt.Errorf("the reviewed native Checkov profile changed")
	}
	return data, nil
}

func validateReport(data, log []byte) error {
	var reports []struct {
		Type    string `json:"check_type"`
		Summary struct {
			Passed, Failed, Skipped int
			Errors                  int    `json:"parsing_errors"`
			Version                 string `json:"checkov_version"`
		} `json:"summary"`
	}
	if json.Unmarshal(data, &reports) != nil || len(reports) == 0 {
		return fmt.Errorf("native Checkov returned no complete framework report")
	}
	seen := map[string]bool{}
	errors := 0
	for _, report := range reports {
		if report.Type == "" || seen[report.Type] || (report.Summary.Version != "" && report.Summary.Version != "3.3.15") {
			return fmt.Errorf("native framework identity is missing, duplicated or changed")
		}
		seen[report.Type] = true
		errors += report.Summary.Errors
		if report.Type != "cloudformation" && report.Summary.Passed+report.Summary.Failed+report.Summary.Skipped == 0 {
			return fmt.Errorf("native framework examined no resources")
		}
	}
	for _, framework := range []string{"kubernetes", "dockerfile", "github_actions"} {
		if !seen[framework] {
			return fmt.Errorf("native Checkov did not examine required framework %s", framework)
		}
	}
	// The pre-existing CloudFormation classifier rejects this multi-document
	// Kubernetes bundle, which is independently examined by its Kubernetes runner.
	// Preserve that reviewed baseline and refuse any new or swapped omission.
	parse := regexp.MustCompile(`YAML error parsing ([^:\n]+):`).FindAllStringSubmatch(string(log), -1)
	if errors > 1 || len(parse) != errors {
		return fmt.Errorf("native parser omission differs from the reviewed baseline")
	}
	for _, entry := range parse {
		if strings.TrimPrefix(entry[1], "./") != "k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml" {
			return fmt.Errorf("an unreviewed source could not be parsed")
		}
	}
	return nil
}

func runtimeGuard(ctx context.Context, source, root string, stderr io.Writer) error {
	// The native scanner keeps its original PATH/framework discovery. Only this
	// offline renderer subprocess receives the private approved kubectl.
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(source, "scripts/with-runtime-disposition-tools.sh"), "--kubectl-only", "--", "go", "run", "./scripts/guard-trivy-runtime-dispositions", "--root", root)
	cmd.Dir = source
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("native resource disposition guard refused: %w", err)
	}
	return nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) < 2 || !filepath.IsAbs(args[0]) {
		return 2, fmt.Errorf("an absolute native Checkov executable is required")
	}
	engine, args := args[0], args[1:]
	if slices.Equal(args, []string{"--version"}) || slices.Equal(args, []string{"--help"}) {
		cmd := exec.CommandContext(ctx, engine, args...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			return 2, err
		}
		return 0, nil
	}
	configPath, err := arguments(args)
	if err != nil {
		return 2, err
	}
	config, err := configuration(configPath)
	if err != nil {
		return 2, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	version, err := exec.CommandContext(ctx, engine, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "3.3.15" {
		return 2, fmt.Errorf("native Checkov 3.3.15 is required")
	}
	source, err := os.Getwd()
	if err != nil {
		return 2, err
	}
	snapshot, cleanup, err := scansnapshot.Clone(ctx, source)
	if err != nil {
		return 2, err
	}
	defer cleanup()
	if err := runtimeGuard(ctx, source, snapshot.Root, stderr); err != nil {
		return 2, err
	}
	guard := exec.CommandContext(ctx, "bash", filepath.Join(source, "scripts/guard-checkov-operational-inputs.sh"), snapshot.Root)
	guard.Stderr = stderr
	if err := guard.Run(); err != nil {
		return 2, err
	}
	work, err := os.MkdirTemp("", "platform-checkov-")
	if err != nil {
		return 2, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	path := filepath.Join(work, "native.checkov.yml")
	if err := os.WriteFile(path, config, 0600); err != nil {
		return 2, err
	}
	full := slices.Clone(args)
	full[3] = path
	full = append(full, "--output", "json")
	if err := snapshot.Verify(); err != nil {
		return 2, err
	}
	cmd := exec.CommandContext(ctx, engine, full...)
	cmd.Dir = snapshot.Root
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
		return 2, fmt.Errorf("native Checkov command exceeded its bounded scan")
	}
	status := 0
	if scanErr != nil {
		exit, ok := scanErr.(*exec.ExitError)
		if !ok || exit.ExitCode() != 1 {
			return 2, fmt.Errorf("native Checkov did not complete: %w", scanErr)
		}
		status = exit.ExitCode()
	}
	if err := validateReport(out.Bytes(), log.Bytes()); err != nil {
		return 2, err
	}
	if err := snapshot.Verify(); err != nil {
		return 2, err
	}
	if err := runtimeGuard(ctx, source, snapshot.Root, stderr); err != nil {
		return 2, err
	}
	return status, nil
}
