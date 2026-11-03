package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestQuotaCorrectionPreservesTheAuditedDenyBody(t *testing.T) {
	original, err := os.ReadFile("testdata/use_resource_quota.rego.golden")
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := patchQuotaRule(original)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(corrected)) != "b0465cbf394c01bbaf55292f3c733ef02293e2d48a2732ad50f871baa02db3c9" {
		t.Fatal("quota check must add only the reviewed per-resource applicability condition")
	}
	for name, input := range map[string][]byte{
		"changed deny body": bytes.Replace(original, []byte("not resource_quota_configured"), []byte("false"), 1),
		"empty":             nil,
		"already corrected": corrected,
		"additional rule":   append(bytes.Clone(original), []byte("\ndeny contains false\n")...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := patchQuotaRule(input); err == nil {
				t.Fatal("unknown policy input must refuse correction")
			}
		})
	}
}

func TestAdapterRetainsTheUnknownScopeRefusalStatus(t *testing.T) {
	adapter, err := filepath.Abs("../trivy-resource-quota.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", adapter, "--not-a-scan")
	cmd.Env = append(os.Environ(), "TRIVY_NATIVE_BIN=/usr/bin/false")
	output, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !bytes.Contains(output, []byte("unknown scan scope")) {
		t.Fatalf("adapter must retain the guard's refusal status: %v: %s", err, output)
	}
}

func TestRefusedOrTimedOutScanRetainsItsNativeFindingsAndDiagnostics(t *testing.T) {
	timeout := errors.New("native timeout")
	for _, scanErr := range []error{nil, timeout} {
		var stdout, stderr bytes.Buffer
		status, err := finishScan(nativeResult{stdout: []byte("retained native finding\n"), stderr: []byte("retained native diagnostic\n"), status: 1}, scanErr, filepath.Join(t.TempDir(), "missing-cache"), &stdout, &stderr)
		if status != 2 || err == nil || stdout.String() != "retained native finding\n" || stderr.String() != "retained native diagnostic\n" {
			t.Fatalf("refusal lost native evidence: status=%d err=%v stdout=%q stderr=%q", status, err, stdout.String(), stderr.String())
		}
		if scanErr != nil && !errors.Is(err, timeout) {
			t.Fatal("native timeout cause was discarded")
		}
	}
}

func TestNativeMegaLinterExclusionsRetainTheCompleteScannerScope(t *testing.T) {
	args := []string{"fs", "--scanners", "vuln,misconfig", "--exit-code", "1", "--ignorefile", ".trivyignore.yaml", "--skip-dirs", "tests", "--config-data", ".trivy/data", "--skip-dirs", "**/.agents", "--skip-dirs", "**/megalinter-reports", "."}
	if !validScanArguments(args) {
		t.Fatal("the observed native MegaLinter command must retain its existing generated-directory exclusions")
	}
	for index, replacement := range map[int]string{2: "misconfig", 4: "0", 8: "k8s", 12: "**/scripts", 14: "**/k8s", 15: "k8s"} {
		changed := slices.Clone(args)
		changed[index] = replacement
		if validScanArguments(changed) {
			t.Fatalf("changed scanner scope or verdict input passed at argument %d", index)
		}
	}
}

func TestPolicyInventoryBindsNamesBytesAndFileTypes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rule.rego")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := policyInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := policyInventory(dir)
	if err != nil || before == after {
		t.Fatal("changed policy bytes must change the inventory")
	}
	if err := os.Rename(file, filepath.Join(dir, "other.rego")); err != nil {
		t.Fatal(err)
	}
	renamed, err := policyInventory(dir)
	if err != nil || renamed == after {
		t.Fatal("policy paths must be part of the inventory")
	}
	if err := os.Symlink(filepath.Join(dir, "other.rego"), file); err != nil {
		t.Fatal(err)
	}
	if _, err := policyInventory(dir); err == nil {
		t.Fatal("linked policy input must be refused")
	}
}

func TestDiskLoaderEvidenceRefusesFallbackAndPartialCoverage(t *testing.T) {
	good := "DEBUG Initializing embedded checks\nDEBUG [vulndb] There is no valid metadata file err.message=\"file open error\"\nDEBUG [misconfig] Checks successfully loaded from disk\nDEBUG [rego] Checks from disk are loaded count=580\nDEBUG Skipping path path=\"tests\"\n"
	if err := validateLoaderLog([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"", "DEBUG Initializing embedded checks\n",
		good + "WARN Falling back to embedded checks\n",
		good + "ERROR rego_parse_error: bad rule\n",
		good + "WARN Skipping policy after a compile failure\n",
		strings.ReplaceAll(good, "count=580", "count=0"),
		strings.ReplaceAll(good, "count=580", "count=579"),
		good + "DEBUG [rego] Checks from disk are loaded count=0\n",
	} {
		if err := validateLoaderLog([]byte(bad)); err == nil {
			t.Fatalf("unverified loader evidence must be refused: %q", bad)
		}
	}
}

func TestNativeQuotaAndUnrelatedFindingControls(t *testing.T) {
	engine, cache := os.Getenv("PLATFORM_TRIVY_CONTROL_ENGINE"), os.Getenv("PLATFORM_TRIVY_CONTROL_CACHE")
	if engine == "" && cache == "" {
		t.Skip("optional offline diagnostic; the full scanner wrapper always runs these native controls")
	}
	work := t.TempDir()
	corrected := filepath.Join(work, "corrected")
	if err := os.Mkdir(corrected, 0700); err != nil {
		t.Fatal(err)
	}
	if err := correctedBundle(cache, corrected); err != nil {
		t.Fatal(err)
	}
	if err := nativeControls(engine, cache, corrected, work); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundle(cache, originalInventory); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundle(corrected, correctedInventory); err != nil {
		t.Fatal(err)
	}
}
