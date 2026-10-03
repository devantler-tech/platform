package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// One spelling table drives the normal gate and the workflow-if admission path.
// The control scan makes a missed second invocation a real false green.
func TestSharedParserBypassRefusals(t *testing.T) {
	cases := map[string]string{
		"3579 env prefix":                      "env ksail workload scan --framework nsa",
		"3579 shell string":                    "bash -c 'ksail workload scan --framework nsa'",
		"3579 eval string":                     "eval 'ksail workload scan --framework nsa'",
		"3579 even backslash run":              "echo done \\\\\nksail workload scan --framework nsa",
		"3579 continued executable":            "k\\\nsail workload scan --framework nsa",
		"3579 separator adjacency":             "true;ksail workload scan --framework nsa",
		"3579 partial flag continuation":       "ksail workload scan --frame\\\nwork nsa",
		"3582 env operand expansion":           "env ${COMMAND} workload scan --framework nsa",
		"3582 assembled eval":                  "eval ${KSAIL} ${WORKLOAD} ${SCAN} --framework nsa",
		"3582 all command words expanded":      "${KSAIL} ${WORKLOAD} ${SCAN} --framework nsa",
		"3582 substituted executable":          "$(printf ksail) workload scan --framework nsa",
		"3582 env without anchors":             "env -u UNUSED ${C} ${A} ${B}",
		"3582 eval without anchors":            "eval \"$C\"",
		"3582 substitution without anchors":    "out=$(${C} ${A} ${B})",
		"3582 reserved-word without anchors":   "if ${C} ${A} ${B}; then echo done; fi",
		"3582 direct without anchors":          "${C} ${A} ${B}",
		"3582 shell string without anchors":    "bash -c \"$C\"",
		"3582 command wrapper without anchors": "command ${C} ${A}",
		"3586 conditional subshell":            "( ksail workload scan --framework nsa )",
		"3586 alias":                           "shopt -s expand_aliases\nalias reduced='ksail workload scan --framework nsa'\nreduced",
		"3586 alias executable":                "shopt -s expand_aliases\nalias k=ksail\nk workload scan --framework nsa",
		"3586 path-qualified":                  "/tmp/bin/ksail workload scan --framework nsa",
		"4163 command substitution":            "out=$(ksail workload scan --framework nsa)",
		"4163 multiline substitution":          "out=$(ksail workload scan \\\n  --framework nsa)",
		"4163 expanding heredoc":               "cat <<EOF\n$(ksail workload scan --framework nsa)\nEOF",
		"4163 echo substitution G1":            "echo \"$(ksail workload scan --framework nsa)\"",
		"4163 nested substitution argument":    "echo \"$(printf '%s' \"$(ksail workload scan --framework nsa)\")\"",
		"4163 negated scan":                    "! ksail workload scan --framework nsa",
		"4164 shell heredoc":                   "bash <<'EOF'\nksail workload scan --framework nsa\nEOF",
		"4164 shell here-string":               "bash <<< 'ksail workload scan --framework nsa'",
		"4164 shell redirected input":          "bash < <(printf '%s\\n' 'ksail workload scan --framework nsa')",
		"4164 trap":                            "trap 'ksail workload scan --framework nsa' EXIT",
		"4164 xargs stdin":                     "printf '%s\\n' 'workload scan --framework nsa' | xargs ksail",
		"4164 xargs here-string":               "xargs ksail <<< 'workload scan --framework nsa'",
		"4164 pipeline interpreter":            "printf '%s\\n' 'ksail workload scan --framework nsa' | bash",
		"4379 root benchmark flag":             "ksail --benchmark workload scan --framework nsa",
		"4379 interleaved experimental":        "ksail workload --experimental scan --framework nsa",
		"4379 interleaved benchmark":           "ksail workload --benchmark scan --framework nsa -o kubescape.sarif",
		"4379 interleaved config":              "ksail workload --config=ksail.prod.yaml scan --framework nsa",
		"4379 root config":                     "ksail --config=ksail.prod.yaml workload scan --framework nsa",
		"4379 unframed scan":                   "ksail workload scan",
		"round1 literal assembled shell code":  "bash -c 'K=ks; K+=ail; W=work; W+=load; S=sc; S+=an; F=--frame; F+=work; \"$K\" \"$W\" \"$S\" \"$F\" nsa'",
		"round1 wrapped dynamic code":          "env bash -c \"$C\"",
		"round1 wrapped eval":                  "command eval \"$C\"",
		"round1 dynamic xargs executable":      "xargs \"$K\" <<< 'workload scan --framework nsa'",
		"round1 quoted Actions command":        "'${{ 'ksail' }}' workload scan --framework nsa",
		"round1 partial Bash parse":            "K=ks; K+=ail\nW=work; W+=load\nS=sc; S+=an\nF=--frame; F+=work\n\"$K\" \"$W\" \"$S\" \"$F\" nsa\n)",
		"round2 env split-string":              "env -S 'ksail workload scan --framework nsa'",
		"round2 dynamic shell options":         "O=-c; bash \"$O\" \"$C\"",
		"round2 delegated trap":                "command trap \"$C\" EXIT",
		"round2 xargs delegated wrapper":       "xargs env \"$K\" <<< 'workload scan --framework nsa'",
		"round2 group stdin":                   "{ bash; } <<'EOF'\nksail workload scan --framework nsa\nEOF",
		"round2 boolean root value":            "ksail workload --experimental=false scan --framework nsa",
		"boolean root numeric value":           "ksail workload --experimental=0 scan --framework nsa",
		"boolean root uppercase value":         "ksail workload --experimental=TRUE scan --framework nsa",
		"shell stdin descriptor source":        "bash 3<<'EOF' <&3\nksail workload scan --framework nsa\nEOF",
		"round3 xargs incomplete wrapper":      "xargs env <<< 'ksail workload scan --framework nsa'",
		"round3 builtin trap":                  "K=ks; K+=ail; C=\"$K workload scan --framework nsa\"; builtin trap \"$C\" EXIT",
		"round3 stdin output descriptor dup":   "bash 3<<'EOF' 0>&3\nksail workload scan --framework nsa\nEOF",
		"round4 positive shell option":         "K=ks; K+=ail; C=\"$K workload scan --framework nsa\"; bash +e -c \"$C\"",
		"round4 env clustered split-string":    "env -iS 'ksail workload scan --framework nsa'",
		"round5 multiargument env assignment":  "K=ks; K+=ail; W=work; W+=load; S=sc; S+=an; F=--frame; F+=work; set -- ready \"$K\" \"$W\" \"$S\" \"$F\" nsa; env \"TEST_VALUE=$@\"",
		"round5 array env assignment":          "WORDS=(ready ksail workload scan --framework nsa); env \"TEST_VALUE=${WORDS[@]}\"",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			for _, conditional := range []bool{false, true} {
				content := runBlock(goodScan + "\n" + body)
				if conditional {
					content = runBlock(goodScan) + "      - if: true\n        run: |\n"
					for _, line := range strings.Split(body, "\n") {
						content += "          " + line + "\n"
					}
				}
				if got, err := frameworkSet(writeTemp(t, content)); err == nil {
					t.Errorf("conditional=%v: missed executable scan; accepted %v", conditional, got)
				}
			}
		})
	}
}

func TestSharedParserOrdinaryShellControls(t *testing.T) {
	cases := map[string]string{
		"inline comment":                            "echo ok # ; \"$STATUS\"",
		"brace group":                               "{\n echo hello\n}",
		"multiline array":                           "ARGS=(\n \"$VALUE\"\n)\necho \"${ARGS[@]}\"",
		"other framework-aware tool":                "checkov --file manifest.yaml --framework kubernetes",
		"quoted scanner substring":                  "bash -c 'echo \"scanner starting\"'",
		"quoted scan prose":                         "echo 'ksail workload scan --framework nsa'",
		"odd backslash continuation":                "echo hello \\\n world",
		"ordinary interpreter heredoc":              "bash <<'EOF'\necho hello\nEOF",
		"non-stdin shell input":                     "bash -c 'echo ready' 3<<< 'ksail workload scan --framework nsa'",
		"shadowed shell stdin":                      "bash <<'DATA' <<'CODE'\nksail workload scan --framework nsa\nDATA\necho ready\nCODE",
		"date substitution":                         "STAMP=$(date -u +%s)",
		"quoted heredoc":                            "cat <<'EOF'\n$(ksail workload scan --framework nsa)\nEOF",
		"ordinary heredoc":                          "cat <<EOF\nscan output belongs here\nEOF",
		"cleanup trap":                              "trap 'rm -f \"$RUNNER_TEMP/output\"' EXIT",
		"ordinary shell string":                     "bash -c 'echo scan-ready'",
		"literal shell string with inner expansion": "bash -c 'echo \"$HOME\"'",
		"harmless multiline substitution":           "STAMP=$(\n date -u +%s\n)",
		"escaped backticks":                         "echo \"\\`ksail workload scan --framework nsa\\`\"",
		"ordinary env":                              "env -u UNUSED VALUE=\"$HOME\" echo ready",
		"ordinary alias":                            "alias ll='ls -l'",
		"IP assignment":                             "IP=10.0.0.1",
		"array assignment":                          "IPS=(10.0.0.1 10.0.0.2)",
		"workflow expression":                       "echo \"${{ github.sha }}\" >> \"$GITHUB_OUTPUT\"",
		"quoted expression terminator":              "echo \"${{ format('}}') }}\"",
		"two expressions":                           "echo \"${{ github.sha }} ${{ github.ref }}\"",
		"ordinary ksail command":                    "ksail --version\nksail workload validate",
		"round3 env delegated option data":          "env printf '%s\\n' '-Something ordinary'",
		"round3 script mode stdin data":             "bash scripts/ordinary.sh <<< 'ksail workload scan --framework nsa'",
		"round3 redirected pipeline stdin":          "printf '%s\\n' 'ksail workload scan --framework nsa' | bash <<'CODE'\necho ready\nCODE",
		"round4 command query operand":              "command -v \"$TOOL\"",
		"round4 quoted env assignment":              "env \"TEST_VALUE=$VALUE\" echo ready",
		"round4 shell positional stdin data":        "bash -s -- \"$ARG\" <<'CODE'\necho ready\nCODE",
		"round5 env attached unset operand":         "env -uSHELL echo ready",
		"round5 command permuted query options":     "command -vp \"$TOOL\"",
		"round6 alias query":                        "alias ksail 2>/dev/null || true",
		"round6 quoted substitution env value":      "env \"TEST_VALUE=$(printf '%s' \"$@\")\" echo ready",
		"failure mode restored":                     "set +e\nset -e",
		"eval failure mode restored":                "eval 'set +e; set -e'",
		"unused ordinary helper":                    "helper() { set +e; echo ready; }\necho ready",
		"invoked helper preserves failure mode":     "helper() { set +e; set -e; }\nhelper",
		"successful exit handler removed":           "trap 'exit 0' EXIT\ntrap - EXIT",
		"transitive helper preserves failure mode":  "restore() { set -e; }; helper() { restore; }; helper",
		"child alias does not escape":               "(alias false=true)",
		"unused local alias does not escape":        "helper() { alias false=true; }",
		"unused nested function does not escape":    "helper() { false() { return 0; }; }",
		"unused scanner alias does not escape":      "helper() { alias ksail=echo; }",
		"child scanner function does not escape":    "(ksail() { :; })",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			for _, conditional := range []bool{false, true} {
				content := runBlock(body + "\n" + goodScan)
				if conditional {
					content = runBlock(goodScan) + "      - if: true\n        run: |\n"
					for _, line := range strings.Split(body, "\n") {
						content += "          " + line + "\n"
					}
				}
				got, err := frameworkSet(writeTemp(t, content))
				if err != nil || strings.Join(got, ",") != "mitre,nsa" {
					t.Errorf("conditional=%v: ordinary shell was refused: %v, %v", conditional, got, err)
				}
			}
		})
	}
}

func TestNonterminalScanConjunctionCannotHideFailure(t *testing.T) {
	for _, separator := range []string{"\n", "; "} {
		if _, err := setOf(t, goodScan+" && echo scanned"+separator+"echo ready"); err == nil {
			t.Errorf("nonterminal scan conjunction accepted with %q", separator)
		}
	}
}

func TestChildLocalBindingsDoNotShadowFailureFallback(t *testing.T) {
	for _, prefix := range []string{"(alias false=true)", "helper() { alias false=true; }", "helper() { false() { return 0; }; }"} {
		if _, err := setOf(t, prefix+"\n"+goodScan+" || false"); err != nil {
			t.Errorf("child-local binding escaped: %s: %v", prefix, err)
		}
	}
}

func TestLocalFallbackMustStillProveFailure(t *testing.T) {
	for _, binding := range []string{"false() { return 0; }", "shopt -s expand_aliases; alias false=true", "exit() { return 0; }"} {
		fallback := "false"
		if strings.HasPrefix(binding, "exit()") {
			fallback = "exit 1"
		}
		if _, err := setOf(t, binding+"\n"+goodScan+" || "+fallback); err == nil {
			t.Errorf("locally replaced fallback credited: %s", binding)
		}
	}
}

func TestEvalBindingsMustReachTheCaller(t *testing.T) {
	for _, declaration := range []string{"false() { return 0; }", "alias false=true"} {
		if _, err := setOf(t, "eval '"+declaration+"'\n"+goodScan+" || false"); err == nil {
			t.Errorf("eval binding lost in caller: %s", declaration)
		}
	}
}

func TestScannerCannotRunWithFailureHandlingDisabled(t *testing.T) {
	for _, prefix := range []string{
		"set +e",
		"eval 'set +e'",
		"trap 'exit 0' EXIT",
		"trap 'exit 0' ERR",
		"helper() { set +e; }; helper",
		"exit 0",
		"trap 'exit 0' EXIT\ntrap - ERR",
		"set +e\nif false; then set -e; fi",
		"set +e\n(set -e)",
		"trap 'exit 0' EXIT\nif false; then trap - EXIT; fi",
		"disable() { set +e; }; helper() { disable; }; helper",
		"exit 256",
		"trap 'exit 256' EXIT",
		"trap -- 'exit 0' EXIT",
		"helper() { set +e; }; if false; then helper() { set -e; }; fi; helper",
		"set +e\nset() { :; }; set -e",
		"restore() { return 0; set -e; }; set +e; restore",
		"exec bash -c 'echo hello'",
		"set +e\nenv set -e",
		"shopt -s lastpipe\ntrue | set +e",
		"exit -- 0",
		"trap 'exit -- 0' EXIT",
		"trap 'finish' EXIT\nfinish() { exit 0; }",
		"finish() { echo ready; }; trap 'finish' EXIT\nfinish() { exit 0; }",
		"set +e; restore() { set -e; }; command restore",
		"exec env echo ready",
		"trap 'trap \"exit 0\" EXIT' ERR",
		"trap 'exit 0' DEBUG",
		"set +e; /nonexistent/set -e",
		"restore() { set -e; }; set +e; ./restore",
	} {
		if _, err := setOf(t, prefix+"\n"+goodScan); err == nil {
			t.Errorf("scanner failure can be discarded by: %s", prefix)
		}
	}
}

func TestLocallyShadowedScannerCannotSupplyTheOnlyGate(t *testing.T) {
	for _, binding := range []string{
		"shopt -s expand_aliases\nalias ksail=echo",
		"shopt -s expand_aliases\nB='ksail=echo'\nalias \"$B\"",
		"ksail() { echo ready; }",
	} {
		if _, err := setOf(t, binding+"\n"+goodScan); err == nil {
			t.Errorf("locally shadowed CLI cannot establish the scan gate: %s", binding)
		}
	}
}

func TestFullScanFailureMaskIsRefusedWithoutASecondInvocation(t *testing.T) {
	if _, err := setOf(t, goodScan+" && echo scanned || true"); err == nil || !strings.Contains(err.Error(), "failure is discarded") {
		t.Fatalf("the sole full scan's masked status was accepted: %v", err)
	}
}

func TestInterleavedRootFlagsUseTheActualFrameworkList(t *testing.T) {
	for _, scan := range []string{
		"ksail --benchmark workload scan --framework nsa,mitre",
		"ksail workload --experimental scan --framework nsa,mitre",
		"ksail --config=ksail.prod.yaml workload scan --framework nsa,mitre",
	} {
		got, err := setOf(t, scan)
		if err != nil || strings.Join(got, ",") != "mitre,nsa" {
			t.Errorf("valid root flags %q: %v, %v", scan, got, err)
		}
	}
}

// Run an isolated recording stand-in under the runner's bash failure semantics.
// It proves both an executable missed shape and the chained status-mask defect.
func TestParserRegressionBashWitnesses(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace")
	stub := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"$SCAN_WITNESS_TRACE\"\nexit \"${SCAN_WITNESS_STATUS:-0}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ksail"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	fragments := "K=ks; K+=ail; W=work; W+=load; S=sc; S+=an; F=--frame; F+=work; "
	for name, witness := range map[string]struct {
		body string
		zero bool
	}{
		"G1 echo substitution":      {"echo \"$(ksail workload scan --framework nsa)\"", true},
		"G2 expanding data heredoc": {"cat <<EOF\n$(ksail workload scan --framework nsa)\nEOF", true},
		"G3 sole masked full scan":  {goodScan + " && echo scanned || true", true},
		"shell heredoc":             {"bash <<'EOF'\nksail workload scan --framework nsa\nEOF", false},
		"shell here-string":         {"bash <<< 'ksail workload scan --framework nsa'", false},
		"exit trap":                 {"trap 'ksail workload scan --framework nsa' EXIT", false},
		"xargs here-string":         {"xargs ksail <<< 'workload scan --framework nsa'", false},
		"literal assembled code":    {"bash -c '" + fragments + "\"$K\" \"$W\" \"$S\" \"$F\" nsa'", false},
		"wrapped code":              {fragments + "C=\"$K $W $S $F nsa\"; env bash -c \"$C\"", false},
		"assembled xargs":           {fragments + "xargs \"$K\" <<< 'workload scan --framework nsa'", false},
		"partially parsed Bash":     {fragments + "\"$K\" \"$W\" \"$S\" \"$F\" nsa\n)", false},
		// This is a literal substitution witness; it does not exercise Actions.
		"quoted Actions literal after substitution": {"'ksail' workload scan --framework nsa", false},
		"failure-preserving fallback":               {goodScan + " || exit 1", false},
		"errexit disabled":                          {"set +e\n" + goodScan + "\necho ready", true},
		"eval changes errexit":                      {"eval 'set +e'\n" + goodScan + "\necho ready", true},
		"successful exit handler":                   {"trap 'exit 0' EXIT\n" + goodScan, true},
		"successful error handler":                  {"trap 'exit 0' ERR\n" + goodScan + "\necho ready", true},
		"helper changes errexit":                    {"helper() { set +e; }; helper\n" + goodScan + "\necho ready", true},
		"conditional restoration is not executed":   {"set +e\nif false; then set -e; fi\n" + goodScan + "\necho ready", true},
		"subshell restoration does not propagate":   {"set +e\n(set -e)\n" + goodScan + "\necho ready", true},
		"conditional handler clearing not executed": {"trap 'exit 0' EXIT\nif false; then trap - EXIT; fi\n" + goodScan, true},
	} {
		t.Run(name, func(t *testing.T) {
			body := witness.body
			if err := os.WriteFile(trace, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", body)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "SCAN_WITNESS_TRACE="+trace, "SCAN_WITNESS_STATUS=42")
			output, err := cmd.CombinedOutput()
			data, readErr := os.ReadFile(trace)
			if readErr != nil || !strings.Contains(string(data), "workload scan --framework") {
				t.Fatalf("witness did not execute scan %q: %v, %s", body, readErr, output)
			}
			if witness.zero && err != nil {
				t.Fatalf("mask witness must discard failure: %v, %s", err, output)
			}
			if !witness.zero && err == nil {
				t.Fatalf("failing stand-in must fail unmasked shape: %q", body)
			}
			t.Logf("executed %q, shell success=%v", strings.TrimSpace(string(data)), err == nil)
		})
	}
}
