package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type failingResultWriter struct{}

func (failingResultWriter) Write([]byte) (int, error) {
	return 0, errors.New("fixture result output refused")
}

func TestSuccessfulValidationCannotIgnoreFailedResultOutput(t *testing.T) {
	root := fixture(t, map[string]string{"config.yaml": "kind: ConfigMap\ndata:\n  config.json: {}\n"})
	var stderr bytes.Buffer
	if rc := run([]string{"-root", root}, failingResultWriter{}, &stderr); rc != 1 {
		t.Fatalf("failed result output returned success: exit=%d stderr=%q", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot write validation result") {
		t.Fatalf("missing output failure diagnostic: %q", stderr.String())
	}
}

type failOnWrite struct{ calls, failAt int }

func (w *failOnWrite) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, errors.New("fixture report output refused")
	}
	return len(p), nil
}

func TestViolationReportOutputFailuresAreDiagnosed(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			root := fixture(t, map[string]string{"config.yaml": "kind: ConfigMap\ndata:\n  config.json: broken\n"})
			var stderr bytes.Buffer
			writer := &failOnWrite{failAt: failAt}
			if rc := run([]string{"-root", root}, writer, &stderr); rc != 1 || !strings.Contains(stderr.String(), "cannot write validation result") {
				t.Fatalf("report write %d: exit=%d stderr=%q", failAt, rc, stderr.String())
			}
			if writer.calls != failAt {
				t.Fatalf("continued writing after failure: calls=%d want=%d", writer.calls, failAt)
			}
		})
	}
}

func TestYAMLCommentsPreserveDocumentAndDataSelection(t *testing.T) {
	for _, marker := range []string{"--- # next document", "---\t# next document"} {
		t.Run(marker, func(t *testing.T) {
			root := fixture(t, map[string]string{"mixed.yaml": "kind: ConfigMap\ndata:\n  good.json: {}\n" + marker + "\nkind: Secret\ndata:\n  ignored.json: broken\n" + marker + "\nkind: ConfigMap\ndata:\n  second.json: []\n"})
			rc, out, stderr := check(t, root)
			if rc != 0 || out != "✓ 2 embedded JSON blob(s) parse cleanly.\n" || stderr != "" {
				t.Fatalf("mixed documents: exit=%d stdout=%q stderr=%q", rc, out, stderr)
			}
		})
	}
	for _, header := range []string{"kind: ConfigMap\ndata: # JSON configuration", "kind: ConfigMap # configuration\ndata:"} {
		t.Run(header, func(t *testing.T) {
			root := fixture(t, map[string]string{"comment.yaml": header + "\n  config.json: broken\n"})
			rc, out, stderr := check(t, root)
			if rc != 1 || !strings.Contains(out, "comment.yaml:3  (config.json:") || stderr != "" {
				t.Fatalf("comment hid JSON: exit=%d stdout=%q stderr=%q", rc, out, stderr)
			}
		})
	}
}

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "k8s"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, text := range files {
		path := filepath.Join(root, "k8s", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func check(t *testing.T, root string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	rc := run([]string{"-root", root}, &out, &stderr)
	return rc, out.String(), stderr.String()
}

func TestAcceptedManifestForms(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		checked        string
	}{
		{"registered literal", "kind: ConfigMap\ndata:\n  exceptionPolicies: |\n    {\n      \"valid\": true\n    }\n", "1"},
		{"literal chomping", "kind: ConfigMap\ndata:\n  config.json: |-\n    [1,2]\n", "1"},
		{"literal indentation", "kind: ConfigMap\ndata:\n  config.json: |2+\n    {\"valid\":true}\n\n", "1"},
		{"plain", "kind: ConfigMap\ndata:\n  config.json: {\"valid\":true}\n", "1"},
		{"commented mapping", "kind: ConfigMap # configuration\ndata: # JSON values\n  config.json: {\"marker\":\"--- # literal\"}\n", "1"},
		{"single quoted", "kind: ConfigMap\ndata:\n  config.json: '{\"valid\":true}'\n", "1"},
		{"double quoted", "kind: ConfigMap\ndata:\n  config.json: \"true\"\n", "1"},
		{"large number", "kind: ConfigMap\ndata:\n  config.json: 1e1000\n", "1"},
		{"multiple documents", "kind: Secret\ndata:\n  bad.json: broken\n---\nkind: ConfigMap\ndata:\n  good.json: []\n---\nkind: ConfigMap\ndata:\n  exceptionPolicies: {}\n", "2"},
		{"blank block lines", "kind: ConfigMap\ndata:\n  first.json: |\n\n    [\n\n      true\n    ]\n\n  second.json: false\n", "2"},
		{"yaml extension", "kind: ConfigMap\ndata:\n  config.json: null\n", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixture(t, map[string]string{"config.yml": tc.manifest})
			rc, out, stderr := check(t, root)
			want := "✓ " + tc.checked + " embedded JSON blob(s) parse cleanly.\n"
			if rc != 0 || out != want || stderr != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q; want %q", rc, out, stderr, want)
			}
		})
	}
}

func TestLegacyJSONConstantsOnlyAsValueTokens(t *testing.T) {
	for _, value := range []string{
		"NaN", "Infinity", "-Infinity", `[NaN, Infinity, -Infinity]`,
		`{"a":NaN,"b":Infinity,"c":-Infinity}`, `"NaN Infinity -Infinity"`,
		`{"NaN":"Infinity","escaped":"\\\"NaN"}`,
	} {
		t.Run(value, func(t *testing.T) {
			root := fixture(t, map[string]string{"values.yaml": "kind: ConfigMap\ndata:\n  config.json: |\n    " + value + "\n"})
			rc, out, stderr := check(t, root)
			if rc != 0 || out != "✓ 1 embedded JSON blob(s) parse cleanly.\n" || stderr != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", rc, out, stderr)
			}
		})
	}
	for _, value := range []string{
		"NaNX", "XNaN", "InfinityExtra", "-Infinity0", "--Infinity", "nan",
		`{"a":InfinityX}`, `{NaN:true}`, `[NaN true]`, `"bad\NaN"`, `"NaN`,
	} {
		t.Run("reject "+value, func(t *testing.T) {
			root := fixture(t, map[string]string{"values.yaml": "kind: ConfigMap\ndata:\n  config.json: |\n    " + value + "\n"})
			rc, out, _ := check(t, root)
			if rc != 1 || !strings.Contains(out, "Embedded JSON does not parse (1)") || !strings.Contains(out, "k8s/values.yaml:3  (config.json:") {
				t.Fatalf("malformed constant/string received exit=%d stdout=%q", rc, out)
			}
		})
	}
}

func TestScopeAndEncryptedSkips(t *testing.T) {
	root := fixture(t, map[string]string{
		"encrypted.enc.yaml": "kind: ConfigMap\ndata:\n  exceptionPolicies: broken\n",
		"unselected.yaml":    "kind: Secret\ndata:\n  exceptionPolicies: broken\n---\nkind: ConfigMap\nbinaryData:\n  config.json: broken\ndata:\n  not-json: broken\n  exceptionPolicies: ENC[encrypted]\n  config.json: >\n    ENC[encrypted]\n",
		"ignored.txt":        "kind: ConfigMap\ndata:\n  config.json: broken\n",
		"empty.yaml":         "kind: ConfigMap\ndata:\n  empty.json:\n",
		"nested.yaml":        "kind: ConfigMap\ndata:\n  other: value\n    nested.json: broken\n",
	})
	rc, out, stderr := check(t, root)
	if rc != 0 || out != "✓ 0 embedded JSON blob(s) parse cleanly.\n" || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", rc, out, stderr)
	}
}

func TestInvalidAndFoldedDiagnosticsAreSortedAndLocated(t *testing.T) {
	root := fixture(t, map[string]string{
		"z.yaml": "kind: ConfigMap\ndata:\n  broken.json: |\n    {\"bad\":true,}\n  folded.json: >-\n    {}\n",
		"a.yaml": "kind: Secret\ndata:\n  ignored.json: broken\n---\nkind: ConfigMap\ndata:\n  exceptionPolicies: |\n    [true,]\n",
	})
	rc, out, stderr := check(t, root)
	if rc != 1 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", rc, out, stderr)
	}
	for _, want := range []string{
		"\n✗ Embedded JSON does not parse (2):\n",
		"k8s/a.yaml:7  (exceptionPolicies:",
		"k8s/z.yaml:3  (broken.json:",
		"line 1 column 7 (char 6)",
		"\n✗ Embedded JSON in a folded scalar — use a literal block scalar '|' (1):\n",
		"k8s/z.yaml:5  (folded.json)", "3 embedded-JSON violation(s).",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if strings.Index(out, "k8s/a.yaml:7") > strings.Index(out, "k8s/z.yaml:3") {
		t.Fatalf("diagnostics were not sorted: %s", out)
	}
	_, again, _ := check(t, root)
	if out != again {
		t.Fatalf("diagnostics changed across identical reads:\n%s\n%s", out, again)
	}
}

func TestRegisteredAndJSONKeysRejectFoldedAndEmptyBlocks(t *testing.T) {
	for _, key := range []string{"exceptionPolicies", "custom.json"} {
		for _, marker := range []string{">", ">-", ">2+", "|"} {
			t.Run(key+marker, func(t *testing.T) {
				root := fixture(t, map[string]string{"bad.yaml": "kind: ConfigMap\ndata:\n  " + key + ": " + marker + "\n"})
				rc, out, _ := check(t, root)
				if rc != 1 || !strings.Contains(out, "k8s/bad.yaml:3  ("+key) {
					t.Fatalf("exit=%d stdout=%q", rc, out)
				}
				if strings.HasPrefix(marker, ">") && !strings.Contains(out, "folded scalar") {
					t.Fatalf("folded scalar was not identified: %q", out)
				}
			})
		}
	}
}

func TestUnicodeJSONPosition(t *testing.T) {
	root := fixture(t, map[string]string{"unicode.yaml": "kind: ConfigMap\ndata:\n  config.json: |\n    {\"æ\":}\n"})
	rc, out, _ := check(t, root)
	if rc != 1 || !strings.Contains(out, "line 1 column 6 (char 5)") {
		t.Fatalf("JSON position must count characters rather than bytes: exit=%d out=%q", rc, out)
	}
}

func TestUniversalNewlineInput(t *testing.T) {
	for _, newline := range []string{"\r\n", "\r"} {
		t.Run(newline, func(t *testing.T) {
			manifest := "kind: Secret\ndata:\n  ignored.json: broken\n---\nkind: ConfigMap\ndata:\n  config.json: |\n    [true,]\n"
			root := fixture(t, map[string]string{"newline.yaml": strings.ReplaceAll(manifest, "\n", newline)})
			rc, out, stderr := check(t, root)
			if rc != 1 || stderr != "" || !strings.Contains(out, "k8s/newline.yaml:7  (config.json:") || !strings.Contains(out, "line 1 column 7 (char 6)") {
				t.Fatalf("newline input was skipped or mislocated: exit=%d stdout=%q stderr=%q", rc, out, stderr)
			}
		})
	}
}

func TestIncompleteScanFailsClosed(t *testing.T) {
	root := fixture(t, map[string]string{"a-good.yaml": "kind: ConfigMap\ndata:\n  config.json: {}\n"})
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "k8s", "z-unreadable.yaml")); err != nil {
		t.Fatal(err)
	}
	rc, out, stderr := check(t, root)
	if rc != 1 || out != "" || !strings.Contains(stderr, "cannot complete validation:") || !strings.Contains(stderr, "z-unreadable.yaml") {
		t.Fatalf("incomplete scan must not report clean: exit=%d stdout=%q stderr=%q", rc, out, stderr)
	}
}

func TestTruncatedBlockPositionIgnoresTerminalLineBreak(t *testing.T) {
	for _, tail := range []string{"", "\n", "\r\n"} {
		t.Run(tail, func(t *testing.T) {
			root := fixture(t, map[string]string{"truncated.yaml": "kind: ConfigMap\ndata:\n  config.json: |\n    {" + tail})
			rc, out, _ := check(t, root)
			if rc != 1 || !strings.Contains(out, "line 1 column 2 (char 1)") {
				t.Fatalf("terminal line break shifted JSON EOF position: exit=%d stdout=%q", rc, out)
			}
		})
	}
}

func TestBashCallerFromAnotherDirectory(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value, diagnostic string
		exit                    int
	}{
		{"valid", "{}", "✓ 1 embedded JSON blob(s) parse cleanly.\n", 0},
		{"invalid JSON", "broken", "Embedded JSON does not parse (1)", 1},
		{"folded scalar", ">\n    {}", "Embedded JSON in a folded scalar", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixture(t, map[string]string{"config.yaml": "kind: ConfigMap\ndata:\n  config.json: " + tc.value + "\n"})
			cmd := exec.Command("bash", filepath.Join(repo, "scripts", "validate-embedded-json.sh"), "-root", root)
			cmd.Dir = t.TempDir()
			out, err := cmd.CombinedOutput()
			exit := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatalf("alternate-CWD caller could not run: %v", err)
				}
				exit = exitError.ExitCode()
			}
			if exit != tc.exit || !strings.Contains(string(out), tc.diagnostic) {
				t.Fatalf("alternate-CWD caller: exit=%d output=%q; want exit=%d diagnostic=%q", exit, out, tc.exit, tc.diagnostic)
			}
		})
	}
}
