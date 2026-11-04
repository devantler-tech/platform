package acrapplicability

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const dbModified = "2026-11-02T22:01:43Z"

func advisoryFixture(t *testing.T) []byte {
	t.Helper()
	m := object{"id": Advisory, "modified": "2026-08-25T19:43:05Z", "affected": []any{object{"package": object{"name": Module, "ecosystem": "Go"}, "ranges": []any{object{"type": "SEMVER", "events": []any{object{"introduced": "0"}}}}, "ecosystem_specific": object{"imports": []any{object{"path": affectedPackage, "symbols": []any{"ACRCredHelper.Get", "isACRRegistry"}}}}}}}
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func reportFixture(t *testing.T, paired bool) []object {
	t.Helper()
	var osv object
	if e := json.Unmarshal(advisoryFixture(t), &osv); e != nil {
		t.Fatal(e)
	}
	frames := []object{
		{"config": object{"protocol_version": "v1.0.0", "scanner_name": "govulncheck", "scanner_version": "v1.8.0", "db": "https://vuln.go.dev", "db_last_modified": dbModified, "go_version": "go1.27.3", "scan_level": "symbol", "scan_mode": "source"}},
		{"SBOM": object{"go_version": "go1.27.3", "modules": []any{object{"path": ownerModule}, object{"path": Module, "version": Version}}, "roots": []any{ownerModule, ownerModule + ".test"}}},
		{"osv": osv},
		{"finding": object{"osv": Advisory, "trace": []any{object{"module": Module, "version": Version}}}},
		{"finding": object{"osv": Advisory, "trace": []any{object{"module": Module, "version": Version, "package": affectedPackage}}}},
		{"finding": object{"osv": "GO-OTHER", "trace": []any{object{"module": "other.example", "function": "Other"}}}},
	}
	if paired {
		frames = append(frames, object{"finding": object{"osv": Advisory, "trace": []any{object{"module": Module, "version": Version, "package": affectedPackage, "receiver": "ACRCredHelper", "function": "Get"}, object{"package": ownerModule, "function": controlName}}}})
	}
	return frames
}

func encodeFrames(t *testing.T, frames []object) []byte {
	t.Helper()
	var out []byte
	for _, frame := range frames {
		b, e := json.Marshal(frame)
		if e != nil {
			t.Fatal(e)
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	return out
}

func TestNativeReportsRequirePositiveRootsAndRealPairedReachability(t *testing.T) {
	for _, paired := range []bool{false, true} {
		data := encodeFrames(t, reportFixture(t, paired))
		got, e := nativeReport(data, advisoryFixture(t), dbModified, paired, []string{ownerModule})
		if e != nil {
			t.Fatal(e)
		}
		if !slices.Contains(got.OtherIDs, "GO-OTHER") {
			t.Fatal("unrelated advisory was lost")
		}
		if paired && (got.Reachable != 1 || !got.Control) {
			t.Fatal("paired source did not reach the affected function")
		}
	}
}

func TestIncompleteMisboundAndVacuousReportsRefuse(t *testing.T) {
	for _, mode := range []string{"missing config", "duplicate config", "module scan", "wrong compiler", "wrong database", "missing SBOM", "missing code root", "missing test root", "wrong version", "changed advisory", "unknown message", "malformed function", "original reachable", "paired no reachable", "paired no control"} {
		t.Run(mode, func(t *testing.T) {
			paired := strings.HasPrefix(mode, "paired")
			frames := reportFixture(t, paired)
			switch mode {
			case "missing config":
				frames = frames[1:]
			case "duplicate config":
				frames = append(frames, frames[0])
			case "module scan":
				frames[0]["config"].(object)["scan_level"] = "module"
			case "wrong compiler":
				frames[0]["config"].(object)["go_version"] = "go1.27.2"
			case "wrong database":
				frames[0]["config"].(object)["db_last_modified"] = "2026-01-01T00:00:00Z"
			case "missing SBOM":
				frames = append(frames[:1], frames[2:]...)
			case "missing code root":
				frames[1]["SBOM"].(object)["roots"] = []any{ownerModule + ".test"}
			case "missing test root":
				frames[1]["SBOM"].(object)["roots"] = []any{ownerModule}
			case "wrong version":
				frames[1]["SBOM"].(object)["modules"].([]any)[1].(object)["version"] = "v0.0.1"
			case "changed advisory":
				frames[2]["osv"].(object)["modified"] = "2026-09-01T00:00:00Z"
			case "unknown message":
				frames = append(frames, object{"error": object{"message": "incomplete"}})
			case "malformed function":
				frames = append(frames, object{"finding": object{"osv": Advisory, "trace": []any{object{"module": Module, "version": Version, "package": affectedPackage, "function": 42}}}})
			case "original reachable":
				frames = reportFixture(t, true)
			case "paired no reachable":
				frames = reportFixture(t, false)
			case "paired no control":
				frames[len(frames)-1]["finding"].(object)["trace"].([]any)[1].(object)["function"] = "DifferentTest"
			}
			if _, e := nativeReport(encodeFrames(t, frames), advisoryFixture(t), dbModified, paired, []string{ownerModule}); e == nil {
				t.Fatal("incomplete or misbound native evidence was accepted")
			}
		})
	}
	for _, data := range [][]byte{nil, []byte("not json"), append(encodeFrames(t, reportFixture(t, false)), []byte("{")...)} {
		if _, e := nativeReport(data, advisoryFixture(t), dbModified, false, []string{ownerModule}); e == nil {
			t.Fatal("malformed native stream was accepted")
		}
	}
}

func TestAdvisoryScopeChangesRefuse(t *testing.T) {
	for _, mode := range []string{"id", "module", "package", "symbol", "range", "platform", "withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			var m object
			if e := json.Unmarshal(advisoryFixture(t), &m); e != nil {
				t.Fatal(e)
			}
			a := m["affected"].([]any)[0].(object)
			imp := a["ecosystem_specific"].(object)["imports"].([]any)[0].(object)
			switch mode {
			case "id":
				m["id"] = "GO-OTHER"
			case "module":
				a["package"].(object)["name"] = "other.example"
			case "package":
				imp["path"] = "other.example"
			case "symbol":
				imp["symbols"] = []any{"Different"}
			case "range":
				a["ranges"].([]any)[0].(object)["events"] = []any{object{"introduced": "0"}, object{"fixed": "v9.0.0"}}
			case "platform":
				imp["goos"] = []any{"windows"}
			case "withdrawn":
				m["withdrawn"] = "2026-01-01T00:00:00Z"
			}
			b, e := json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			if _, e := advisoryScope(b); e == nil {
				t.Fatal("changed canonical advisory scope accepted")
			}
		})
	}
}

func TestCompilationMustCoverEveryTrackedGoFileIncludingTests(t *testing.T) {
	for _, mode := range []string{"complete", "ignored variant", "omitted test", "test only", "other module", "load error", "no packages"} {
		t.Run(mode, func(t *testing.T) {
			root, files := inputs(t)
			var goFiles []string
			for _, p := range files {
				if strings.HasSuffix(p, ".go") {
					goFiles = append(goFiles, p)
				}
			}
			p := object{"ImportPath": ownerModule, "Dir": filepath.Join(root, filepath.Dir(Owner)), "GoFiles": []string{"main.go"}, "TestGoFiles": []string{"main_test.go"}, "Module": object{"Path": ownerModule, "Main": true}}
			switch mode {
			case "ignored variant":
				p["IgnoredGoFiles"] = []string{"ignored_windows.go"}
			case "omitted test":
				p["TestGoFiles"] = []string{}
			case "test only":
				p["GoFiles"] = []string{}
			case "other module":
				p["Module"] = object{"Path": "other.example", "Main": true}
			case "load error":
				p["Incomplete"] = true
			}
			b, e := json.Marshal(p)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "no packages" {
				b = nil
			}
			_, e = compiledInputs(root, goFiles, b)
			if (e == nil) != (mode == "complete") {
				t.Fatalf("mode %s: %v", mode, e)
			}
		})
	}
}

func TestCallerOverlaysAndWorkspaceAreRefused(t *testing.T) {
	for name, value := range map[string]string{"GOFLAGS": "-overlay=untrusted.json", "GOWORK": "/untrusted/go.work", "GOCACHEPROG": "untrusted-helper", "GOEXPERIMENT": "untrusted"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			_, e := Prove(context.Background(), t.TempDir())
			if e == nil || !strings.Contains(e.Error(), "configuration refused") {
				t.Fatalf("unsafe caller configuration was not refused first: %v", e)
			}
		})
	}
}

func TestOlderAmbientLauncherUsesTheFrozenRootCompiler(t *testing.T) {
	compiler, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	compiler, err = filepath.Abs(compiler)
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "older-go")
	// The ambient 1.26.7 launcher stays old for 'local'; only the explicit
	// frozen selector delegates to the actual root compiler. No PATH is changed.
	script := "#!/bin/sh\nif [ \"${GOTOOLCHAIN:-}\" != go1.27.3 ]; then printf \"go1.26.7\\n\"; exit 0; fi\nexec \"$ACR_TEST_COMPILER\" \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	data, _, _, err := run(context.Background(), t.TempDir(), cleanEnvironment(false, "ACR_TEST_COMPILER="+compiler), time.Minute, launcher, "env", "GOVERSION")
	if err != nil || strings.TrimSpace(string(data)) != "go1.27.3" {
		t.Fatalf("ambient launcher did not select the frozen compiler: %s %v", data, err)
	}
}
