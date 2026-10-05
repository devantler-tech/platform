package arcstaging_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// Manifest text is data, even when it contains command substitutions. These
// guards must inspect it without starting a shell or any other subprocess.
func TestConfigurationGuardsNeverExecuteManifestText(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		document, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range document.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "os/exec" || name == "syscall" {
				t.Errorf("%s imports %s: configuration guards must parse data, not execute it", file, name)
			}
		}
	}
}

func TestBootstrapParametersAcceptOnlyUniqueLiteralArguments(t *testing.T) {
	const path = "auth/kubernetes/role/arc-secret-reader"
	const header = "bao write " + path + " "
	parameters, err := parseBootstrapParameters(header+"\\\n  policies=infra-arc-app-readonly \\\n  ttl=1h", path)
	if err != nil || parameters["policies"] != "infra-arc-app-readonly" || parameters["ttl"] != "1h" {
		t.Fatalf("literal continuation arguments not parsed: %v, %v", parameters, err)
	}
	for _, arguments := range []string{
		"policies=$(printf safe)",
		"policies=`printf safe`",
		"policies=$POLICY",
		"policies=safe;true",
		"policies='safe'",
		"policies=safe\npolicies=other",
		"policies=",
		"policies",
	} {
		t.Run(arguments, func(t *testing.T) {
			if _, err := parseBootstrapParameters(header+arguments, path); err == nil {
				t.Fatal("accepted shell syntax, an empty value, or an ambiguous parameter")
			}
		})
	}
	if _, err := parseBootstrapParameters(header+"policies=safe", "auth/kubernetes/role/other"); err == nil {
		t.Fatal("accepted a write to a different authentication role")
	}
	for _, role := range []string{"$ROLE", "`printf role`", "$(printf role)"} {
		path := "auth/kubernetes/role/" + role
		if _, err := parseBootstrapParameters("bao write "+path+" policies=safe", path); err == nil {
			t.Fatalf("accepted matching but non-literal role path %q", path)
		}
	}
}
