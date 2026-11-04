package celapplicability

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const nativeFixture = `package ext
import "fmt"
func NativeTypes(...any) { fmt.Println("native") }
func ParseStructTag(string) any { fmt.Println("tag"); return nil }
`

// These controls execute valid Go against a local fixture before asking the
// source checker to reject the reachable functions. No downloaded code runs.
func TestRejectsExecutableReferences(t *testing.T) {
	for _, tc := range []struct{ name, imports, call string }{
		{"ordinary import", `"github.com/google/cel-go/ext"`, `ext.NativeTypes(ext.ParseStructTag("json"))`},
		{"renamed import", `native "github.com/google/cel-go/ext"`, `native.NativeTypes(native.ParseStructTag("json"))`},
		{"dot import", `. "github.com/google/cel-go/ext"`, `NativeTypes(ParseStructTag("json"))`},
		{"function values", `"github.com/google/cel-go/ext"`, `f, option := ext.NativeTypes, ext.ParseStructTag; f(option("json"))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "package main\nimport " + tc.imports + "\nfunc main() { " + tc.call + " }\n"
			runFixture(t, "main.go", source, "run", ".")
			if err := inspectGo(sourceFile{Path: "main.go", FirstParty: true, Data: []byte(source)}, map[string]int{}); err == nil {
				t.Fatal("accepted functions proved reachable by native Go execution")
			}
		})
	}
}

func TestRejectsReferencesInTaggedFirstPartyTests(t *testing.T) {
	const source = "//go:build celcontrol\n\npackage main\nimport (\"testing\"; \"github.com/google/cel-go/ext\")\nfunc TestReach(t *testing.T) { ext.NativeTypes(ext.ParseStructTag(\"json\")) }\n"
	runFixture(t, "hidden_test.go", source, "test", "-tags=celcontrol", "-v", ".")
	if err := inspectGo(sourceFile{Path: "testdata/hidden_windows_test.go", FirstParty: true, Data: []byte(source)}, map[string]int{}); err == nil {
		t.Fatal("a tag, OS suffix or testdata directory concealed a first-party test reference")
	}
}

func TestAcceptsOnlyCanonicalDeclarations(t *testing.T) {
	counts := map[string]int{}
	file := sourceFile{Module: modulePath, Version: moduleVersion, Path: "ext/native.go", Data: []byte(nativeFixture)}
	if err := inspectGo(file, counts); err != nil {
		t.Fatal(err)
	}
	if counts["NativeTypes"] != 1 || counts["ParseStructTag"] != 1 {
		t.Fatalf("canonical declarations not witnessed: %v", counts)
	}
	file.Path = "other/native.go"
	if err := inspectGo(file, map[string]int{}); err == nil {
		t.Fatal("accepted declarations from an unexpected package")
	}
}

func TestRejectsTargetedIndirection(t *testing.T) {
	for _, source := range []string{
		"package main\nimport _ \"unsafe\"\n//go:linkname hidden github.com/google/cel-go/ext.NativeTypes\nfunc hidden()\n",
		"package main\nfunc f(p interface{ Lookup(string) (any,error) }) { _,_ = p.Lookup(\"Native\"+\"Types\") }\n",
		"package main\nfunc f(p interface{ MethodByName(string) any }) { _ = p.MethodByName(\"\\u004eativeTypes\") }\n",
		"package main\nfunc f(p interface{ MethodByName(string) any }, name string) { _ = p.MethodByName(name) }\n",
	} {
		if err := inspectGo(sourceFile{Path: "indirection.go", FirstParty: true, Data: []byte(source)}, map[string]int{}); err == nil {
			t.Fatalf("accepted unsupported symbolic lookup: %s", source)
		}
	}
}

func TestFirstPartyDynamicCodeAndSymbolStringsStayUnsupported(t *testing.T) {
	for _, source := range []string{
		"package main\nimport _ \"os/exec\"\n",
		"package main\nimport _ \"unsafe\"\n",
		"package main\nimport _ \"plugin\"\n",
		"package main\nimport \"C\"\n",
		"package main\nfunc f() string { return \"Native\"+\"Types\" }\n",
		"package main\nconst name = \"\\u004eativeTypes\"\n",
	} {
		if err := inspectGo(sourceFile{Path: "dynamic.go", FirstParty: true, Data: []byte(source)}, map[string]int{}); err == nil {
			t.Fatal("accepted unsupported first-party runtime code or symbol lookup input")
		}
	}
}

func runFixture(t *testing.T, name, source string, args ...string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":            "module example.test/caller\ngo 1.27.3\nrequire github.com/google/cel-go v0.26.0\nreplace github.com/google/cel-go => ./cel\n",
		"cel/go.mod":        "module github.com/google/cel-go\ngo 1.27.3\n",
		"cel/ext/native.go": nativeFixture,
		name:                source,
	}
	if strings.HasSuffix(name, "_test.go") {
		files["main.go"] = "package main\nfunc main() {}\n"
	}
	for name, data := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=go1.27.3", "GOFLAGS=", "GOPROXY=off")
	data, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(data), "tag\nnative\n") {
		t.Fatalf("native reachability control did not execute both functions: %v\n%s", err, data)
	}
}
