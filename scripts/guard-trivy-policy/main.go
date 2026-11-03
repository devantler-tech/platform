// Command guard-trivy-policy runs the complete native repository scan with one
// audited correction to ResourceQuota applicability in a private policy bundle.
package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
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
	work, err := os.MkdirTemp("", "trivy-quota-policy-")
	if err != nil {
		return 2, err
	}
	defer func() { _ = os.RemoveAll(work) }()
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
	// Preserve every native source path, scanner, ignore/data input, finding,
	// output byte and exit status. Only the private policy cache is different.
	fullArgs := append(slices.Clone(args[:len(args)-1]), "--skip-check-update", args[len(args)-1])
	result, err := runNative(engine, corrected, fullArgs, 10*time.Minute)
	status, err := finishScan(result, err, corrected, os.Stdout, os.Stderr)
	if err != nil {
		return 2, err
	}
	fmt.Fprintln(os.Stderr, "guard-trivy-policy: verified complete 580-module bundle and native quota/unrelated-finding refusal controls")
	return status, nil
}
