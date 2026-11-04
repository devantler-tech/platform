// Command guard-osv-source runs complete native source scanning with the
// reviewed OSV release whose Go analyzer supports the repository compiler.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
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
	"golang.org/x/mod/modfile"
)

func main() {
	code, err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guard-osv-source:", err)
		code = 2
	}
	os.Exit(code)
}

func validArguments(args []string) bool {
	return slices.Equal(args, []string{"scan", "source", "--recursive", "."}) || slices.Equal(args, []string{"scan", "source", "--experimental-exclude", ".agents", "--experimental-exclude", "megalinter-reports", "--recursive", "."})
}

func nativeEnvironment() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GO") || strings.HasPrefix(key, "OSV_") || key == "CGO_ENABLED" {
			continue
		}
		env = append(env, item)
	}
	return append(env, "GOENV=off", "GOTOOLCHAIN=go1.27.3", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org", "GOMEMLIMIT=6GiB", "CGO_ENABLED=0")
}

func buildEngine(ctx context.Context, work string, stderr io.Writer) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "install", "github.com/google/osv-scanner/v2/cmd/osv-scanner@v2.7.0")
	cmd.Env = append(nativeEnvironment(), "GOBIN="+filepath.Join(work, "bin"))
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("native OSV source build failed: %w", err)
	}
	path := filepath.Join(work, "bin", "osv-scanner")
	info, err := buildinfo.ReadFile(path)
	if err != nil || info.GoVersion != "go1.27.3" || info.Main.Path != "github.com/google/osv-scanner/v2" || info.Main.Version != "v2.7.0" || info.Main.Sum != "h1:7aOrJ9vB7qbzxC3R+QCXs03SHQiRuM80RJdqZhUEl2g=" || info.Main.Replace != nil {
		return "", fmt.Errorf("native OSV binary has unknown compiler, release or source checksum")
	}
	return path, nil
}

func validateAdjudication(root string, files map[string]string) error {
	path := "scripts/tests/tenant-webapp-admission/osv-scanner.toml"
	for name := range files {
		if filepath.Base(name) == "osv-scanner.toml" && name != path {
			return fmt.Errorf("an additional OSV adjudication config requires review")
		}
		if filepath.Base(name) == "go.work" || filepath.Base(name) == "vendor" {
			return fmt.Errorf("unreviewed source workspace or vendor substitution")
		}
	}
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != "3d6eb09d09bc80fc08c54a47c7cbc11128fb123b746bbfe54c6f5a0ce53c64a1" {
		return fmt.Errorf("the exact three existing Kyverno database-range decisions changed")
	}
	data, err = os.ReadFile(filepath.Join(root, "scripts/tests/tenant-webapp-admission/go.mod"))
	if err != nil {
		return err
	}
	m, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return err
	}
	if len(m.Replace) != 1 || m.Replace[0].Old.Path != "k8s.io/pod-security-admission" || m.Replace[0].Old.Version != "" || m.Replace[0].New.Path != "github.com/kyverno/pod-security-admission" || m.Replace[0].New.Version != "v0.0.0-20261015135348-40bb0fb141a9" {
		return fmt.Errorf("the deployed Kyverno module may only use its reviewed PSA fork")
	}
	for _, dependency := range m.Require {
		if dependency.Mod.Path == "github.com/kyverno/kyverno" && dependency.Mod.Version == "v1.19.2" {
			return nil
		}
	}
	return fmt.Errorf("the existing advisory decisions require the reviewed Kyverno v1.19.2 release")
}

// OSV 2.7 defaults to dropping declared standard-library versions. Preserve
// the previous complete module scope explicitly, using only private configs.
func prepareModuleConfigurations(root string, files map[string]string) (map[string]string, error) {
	result := map[string]string{}
	for name := range files {
		if filepath.Base(name) != "go.mod" {
			continue
		}
		path := filepath.Join(root, filepath.Dir(name), "osv-scanner.toml")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			data = []byte("ScanGoModVersion = true\n")
			if err := os.WriteFile(path, data, 0400); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		result[path] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	return result, nil
}

func verifyModuleConfigurations(configs map[string]string) error {
	if len(configs) == 0 {
		return fmt.Errorf("no native standard-library scan configurations")
	}
	for path, expected := range configs {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("native module configuration is missing or linked")
		}
		data, err := os.ReadFile(path)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != expected {
			return fmt.Errorf("native module configuration changed during scanning")
		}
	}
	return nil
}

func nativeNegativeControl(ctx context.Context, engine, root, work string, stderr io.Writer) error {
	module := filepath.Join(work, "negative-control")
	materialize := exec.CommandContext(ctx, "bash", filepath.Join(root, "scripts/check-go-negative-control.sh"), "--materialize", module)
	materialize.Dir = root
	materialize.Stderr = stderr
	if err := materialize.Run(); err != nil {
		return fmt.Errorf("native negative control materialization failed: %w", err)
	}
	module, err := filepath.EvalSymlinks(module)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(module, "osv-scanner.toml"), []byte("ScanGoModVersion = true\nGoVersionOverride = \"1.24.0\"\n"), 0600); err != nil {
		return err
	}
	env := nativeEnvironment()
	for i, item := range env {
		if strings.HasPrefix(item, "GOTOOLCHAIN=") {
			env[i] = "GOTOOLCHAIN=go1.24.0"
		}
	}
	cmd := exec.CommandContext(ctx, engine, "scan", "source", "--call-analysis=go", "--recursive", "--format", "json", ".")
	cmd.Dir = module
	cmd.Env = env
	var out, log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &log
	err = cmd.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 {
		return fmt.Errorf("native old-compiler control failed to retain its finding exit")
	}
	files := map[string]string{"go.mod": "registered-control"}
	if err := validateReport(out.Bytes(), log.Bytes(), module, files); err != nil {
		return err
	}
	var report struct {
		Results []struct {
			Packages []struct {
				Groups []struct {
					Analysis map[string]struct {
						Called bool `json:"called"`
					} `json:"experimental_analysis"`
				} `json:"groups"`
			} `json:"packages"`
		} `json:"results"`
	}
	if json.Unmarshal(out.Bytes(), &report) != nil {
		return fmt.Errorf("native negative report is unreadable")
	}
	called := 0
	for _, source := range report.Results {
		for _, pkg := range source.Packages {
			for _, group := range pkg.Groups {
				for _, analysis := range group.Analysis {
					if analysis.Called {
						called++
					}
				}
			}
		}
	}
	if called == 0 {
		return fmt.Errorf("native old-compiler control found no reachable advisory")
	}
	if _, err := fmt.Fprintf(stderr, "guard-osv-source: native Go 1.24 control retains exit 1 and %d called advisory records\n", called); err != nil {
		return err
	}
	return nil
}

func validateReport(data, log []byte, root string, files map[string]string) error {
	if regexp.MustCompile(`(?i)failed to run code analysis|govulncheck on[^\n]*error|panic:|fatal error|\[error\]`).Match(log) {
		return fmt.Errorf("native source analysis did not complete")
	}
	var report struct {
		Results []struct {
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
			Packages []struct {
				Package struct{ Name, Version, Ecosystem string } `json:"package"`
			} `json:"packages"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.Results == nil {
		return fmt.Errorf("native scanner returned no complete examined source inventory")
	}
	seen := map[string]bool{}
	// The native extractor logs every examined module, including modules with
	// no vulnerability. Asking for --all-packages would additionally force
	// ./... analysis on a tools-only module with no local Go packages.
	for _, match := range regexp.MustCompile(`(?m)^Scanned (.+/go[.]mod) file and found ([1-9][0-9]*) packages\r?$`).FindAllStringSubmatch(string(log), -1) {
		rel, err := filepath.Rel(root, match[1])
		if err != nil || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("native extractor escaped its frozen checkout")
		}
		path := filepath.ToSlash(rel)
		if _, exists := files[path]; !exists || seen[path] {
			return fmt.Errorf("unknown or duplicated native module extraction")
		}
		seen[path] = true
	}
	for _, source := range report.Results {
		rel, err := filepath.Rel(root, source.Source.Path)
		if err != nil || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("native source report escaped its frozen checkout")
		}
		if len(source.Packages) == 0 {
			return fmt.Errorf("a reported source has no examined packages")
		}
		for _, pkg := range source.Packages {
			if pkg.Package.Name == "" || pkg.Package.Ecosystem == "" {
				return fmt.Errorf("native source inventory contains an unidentified package")
			}
		}
		if filepath.Base(rel) == "go.mod" && !seen[filepath.ToSlash(rel)] {
			return fmt.Errorf("native result names a module without positive extraction evidence")
		}
	}
	for path := range files {
		if filepath.Base(path) == "go.mod" && !seen[path] {
			return fmt.Errorf("native scanner did not examine tracked module %s", path)
		}
	}
	return nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	metadata := slices.Equal(args, []string{"--version"}) || slices.Equal(args, []string{"--help"}) || slices.Equal(args, []string{"scan", "source", "--help"})
	if !metadata && !validArguments(args) {
		return 2, fmt.Errorf("require the complete native recursive source command without changed analysis or config scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	work, err := os.MkdirTemp("", "platform-osv-")
	if err != nil {
		return 2, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	engine, err := buildEngine(ctx, work, stderr)
	if err != nil {
		return 2, err
	}
	if metadata {
		cmd := exec.CommandContext(ctx, engine, args...)
		cmd.Env = nativeEnvironment()
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			return 2, err
		}
		return 0, nil
	}
	snapshot, cleanup, err := scansnapshot.Clone(ctx, ".")
	if err != nil {
		return 2, err
	}
	defer cleanup()
	if err := validateAdjudication(snapshot.Root, snapshot.Files); err != nil {
		return 2, err
	}
	configs, err := prepareModuleConfigurations(snapshot.Root, snapshot.Files)
	if err != nil {
		return 2, err
	}
	if err := nativeNegativeControl(ctx, engine, snapshot.Root, work, stderr); err != nil {
		return 2, err
	}
	if err := verifyModuleConfigurations(configs); err != nil {
		return 2, err
	}
	if err := snapshot.Verify(); err != nil {
		return 2, err
	}
	if _, err := fmt.Fprintf(stderr, "guard-osv-source: native OSV 2.7.0, Go 1.27.3; complete source commit %s\n", snapshot.Head); err != nil {
		return 2, err
	}
	full := append(slices.Clone(args[:len(args)-1]), "--call-analysis=go", "--format", "json", ".")
	cmd := exec.CommandContext(ctx, engine, full...)
	cmd.Dir = snapshot.Root
	cmd.Env = nativeEnvironment()
	var output, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	scanErr := cmd.Run()
	status := 0
	if _, err := stdout.Write(output.Bytes()); err != nil {
		return 2, err
	}
	if _, err := stderr.Write(diagnostics.Bytes()); err != nil {
		return 2, err
	}
	if ctx.Err() != nil {
		return 2, fmt.Errorf("native scanner exceeded its existing bounded command")
	}
	if scanErr != nil {
		exit, ok := scanErr.(*exec.ExitError)
		if !ok || exit.ExitCode() != 1 {
			return 2, fmt.Errorf("native scanner did not complete: %w", scanErr)
		}
		status = exit.ExitCode()
	}
	if err := validateReport(output.Bytes(), diagnostics.Bytes(), snapshot.Root, snapshot.Files); err != nil {
		return 2, err
	}
	if err := snapshot.Verify(); err != nil {
		return 2, err
	}
	if err := verifyModuleConfigurations(configs); err != nil {
		return 2, err
	}
	return status, nil
}
