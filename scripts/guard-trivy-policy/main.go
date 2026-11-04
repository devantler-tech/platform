// Command guard-trivy-policy runs the complete native repository scan with one
// audited correction to ResourceQuota applicability in a private policy bundle.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/celapplicability"
	"github.com/devantler-tech/platform/internal/scansnapshot"
	"github.com/devantler-tech/platform/internal/scanvex"
	"github.com/devantler-tech/platform/internal/trivydispositions"
	"github.com/devantler-tech/platform/internal/trivyruntimedispositions"
)

func patchQuotaRule(input []byte) ([]byte, error) {
	if fmt.Sprintf("%x", sha256.Sum256(input)) != "b170a5ec5865cf089afa40745bda647505f9651ba22b80a20f34785074b176e6" {
		return nil, fmt.Errorf("unknown ResourceQuota policy source")
	}
	return bytes.Replace(input, []byte("deny contains res if {\n"), []byte("deny contains res if {\n\tinput.kind == \"ResourceQuota\"\n"), 1), nil
}

var repositoryArguments = []string{"fs", "--scanners", "vuln,misconfig", "--exit-code", "1", "--ignorefile", ".trivyignore.yaml", "--skip-dirs", "tests", "--config-data", ".trivy/data", "."}

func validScanArguments(args []string) bool {
	if slices.Equal(args, repositoryArguments) {
		return true
	}
	mega := append(slices.Clone(repositoryArguments[:len(repositoryArguments)-1]), "--skip-dirs", "**/.agents", "--skip-dirs", "**/megalinter-reports", ".")
	return slices.Equal(args, mega)
}

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "guard-trivy-policy: %v\n", err)
		code = 2
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) < 2 || !filepath.IsAbs(args[0]) {
		return 2, fmt.Errorf("an absolute native scanner path and the complete repository scan are required")
	}
	engine, args := args[0], args[1:]
	if slices.Equal(args, []string{"--version"}) || slices.Equal(args, []string{"--help"}) || slices.Equal(args, []string{"fs", "--help"}) {
		cmd := exec.Command(engine, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return 2, err
		}
		return 0, nil
	}
	if !validScanArguments(args) {
		return 2, fmt.Errorf("unknown scan scope or arguments; require the complete native vuln,misconfig repository command")
	}
	version, err := exec.Command(engine, "--version").Output()
	if err != nil || !strings.HasPrefix(string(version), "Version: 0.74.0\n") {
		return 2, fmt.Errorf("native Trivy 0.74.0 is required")
	}
	snapshot, cleanup, err := scansnapshot.Clone(context.Background(), ".")
	if err != nil {
		return 2, err
	}
	defer cleanup()
	if err := trivydispositions.Validate(snapshot.Root); err != nil {
		return 2, err
	}
	if err := trivyruntimedispositions.Validate(snapshot.Root); err != nil {
		return 2, err
	}
	proof, err := celapplicability.Prove(context.Background(), snapshot.Root)
	if err != nil {
		return 2, err
	}
	paths := make([]string, 0, len(proof.Scopes))
	for _, scope := range proof.Scopes {
		paths = append(paths, scope.GoModPath)
	}
	scopes := []scanvex.Scope{{IDs: append([]string{proof.Vulnerability}, proof.Aliases...), Module: "github.com/google/cel-go", Version: "v0.26.0", Paths: paths, Justification: proof.Justification}}
	work, err := os.MkdirTemp("", "trivy-quota-policy-")
	if err != nil {
		return 2, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	vexPath := filepath.Join(work, "source-applicability.json")
	if err := scanvex.Write(vexPath, scopes); err != nil {
		return 2, err
	}
	// Retain the proof outside the scanned frozen checkout in the native report tree.
	reportDir := filepath.Join("megalinter-reports", "source-applicability")
	for _, directory := range []string{"megalinter-reports", reportDir} {
		if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
			return 2, err
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return 2, fmt.Errorf("linked or invalid native report directory")
		}
	}
	receipt, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		return 2, err
	}
	receiptFile, err := os.CreateTemp(reportDir, "trivy-cel-"+snapshot.Head+"-*.json")
	if err != nil {
		return 2, err
	}
	if _, err := receiptFile.Write(receipt); err != nil {
		_ = receiptFile.Close()
		return 2, err
	}
	if err := receiptFile.Close(); err != nil {
		return 2, err
	}
	fmt.Fprintf(os.Stderr, "guard-trivy-policy: complete source commit %s; advisory-specific proof of %d source files\n", snapshot.Head, proof.SourceFiles)
	original, corrected := filepath.Join(work, "original"), filepath.Join(work, "corrected")
	for _, dir := range []string{original, corrected, filepath.Join(work, "bootstrap"), filepath.Join(work, "controls")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return 2, err
		}
	}
	if err := os.WriteFile(filepath.Join(work, "bootstrap", "quota.yaml"), []byte(controlValidQuota), 0600); err != nil {
		return 2, err
	}
	// A fresh private cache must obtain the current official bundle. Never use
	// stale cached/embedded rules after an update or download failure.
	bootstrap, err := runNative(engine, original, []string{"config", "--include-non-failures", "--format", "json", "--exit-code", "0", filepath.Join(work, "bootstrap")}, 2*time.Minute)
	if err != nil || bootstrap.status != 0 {
		if _, writeErr := os.Stderr.Write(bootstrap.stderr); writeErr != nil {
			return 2, writeErr
		}
		return 2, fmt.Errorf("current native bundle acquisition failed (native exit %d): %v", bootstrap.status, err)
	}
	if err := validateLoaderLog(bootstrap.stderr); err != nil {
		return 2, err
	}
	_, _, passes, err := reportRows(bootstrap.stdout)
	if err != nil || passes != 1 {
		return 2, fmt.Errorf("bundle acquisition did not examine the complete quota control")
	}
	if err := correctedBundle(original, corrected); err != nil {
		return 2, err
	}
	if err := nativeControls(engine, original, corrected, filepath.Join(work, "controls")); err != nil {
		return 2, err
	}
	if err := verifyBundle(original, originalInventory); err != nil {
		return 2, err
	}
	if err := verifyBundle(corrected, correctedInventory); err != nil {
		return 2, err
	}
	// The native engine retains active and adjudicated findings in its JSON.
	// Both its source and the applicability proof read the same frozen commit.
	if err := snapshot.Verify(); err != nil {
		return 2, err
	}
	fullArgs := append(slices.Clone(args[:len(args)-1]), "--skip-check-update", "--vex", vexPath, "--show-suppressed", "--format", "json", args[len(args)-1])
	result, err := runNativeIn(engine, corrected, fullArgs, 10*time.Minute, snapshot.Root)
	status, err := finishScan(result, err, corrected, os.Stdout, os.Stderr)
	if err != nil {
		return 2, err
	}
	if err := scanvex.ValidateTrivy(result.stdout, vexPath, scopes); err != nil {
		return 2, err
	}
	if err := snapshot.Verify(); err != nil {
		return 2, err
	}
	if err := trivydispositions.Validate(snapshot.Root); err != nil {
		return 2, err
	}
	if err := trivyruntimedispositions.Validate(snapshot.Root); err != nil {
		return 2, err
	}
	fmt.Fprintln(os.Stderr, "guard-trivy-policy: verified complete 580-module bundle and native quota/unrelated-finding refusal controls")
	return status, nil
}
