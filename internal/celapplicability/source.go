// Package celapplicability proves a single, source-bound CEL advisory assessment.
package celapplicability

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

const modulePath = "github.com/google/cel-go"
const moduleVersion = "v0.26.0"

type sourceFile struct {
	Module, Version, Path string
	FirstParty            bool
	Data                  []byte
}

func targeted(data []byte) bool {
	return bytes.Contains(data, []byte("NativeTypes")) || bytes.Contains(data, []byte("ParseStructTag"))
}

func inspectGo(source sourceFile, declarations map[string]int) error {
	// An ordinary Go identifier cannot contain escapes. Files without either
	// name cannot reference it; symbolic lookup sites also need AST inspection.
	if !source.FirstParty && !targeted(source.Data) && !bytes.Contains(source.Data, []byte("Lookup")) &&
		!bytes.Contains(source.Data, []byte("MethodByName")) && !bytes.Contains(source.Data, []byte("FuncForPC")) {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, source.Path, source.Data, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("unreadable Go syntax in %s: %w", source.Path, err)
	}
	if source.FirstParty {
		for _, imported := range file.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil || name == "os/exec" || name == "plugin" || name == "unsafe" || name == "C" {
				return fmt.Errorf("unsupported first-party runtime/native code import in %s", source.Path)
			}
		}
	}
	allowed := map[*ast.Ident]bool{}
	if source.Module == modulePath && source.Version == moduleVersion && source.Path == "ext/native.go" && file.Name.Name == "ext" {
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil && targetName(function.Name.Name) {
				allowed[function.Name] = true
				declarations[function.Name.Name]++
			}
		}
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.Contains(comment.Text, "go:linkname") && targeted([]byte(comment.Text)) {
				return fmt.Errorf("targeted go:linkname in %s", source.Path)
			}
		}
	}
	var failure error
	ast.Inspect(file, func(node ast.Node) bool {
		if failure != nil {
			return false
		}
		if identifier, ok := node.(*ast.Ident); ok && targetName(identifier.Name) && !allowed[identifier] {
			failure = fmt.Errorf("CEL function reference %s in %s:%d", identifier.Name, source.Path, fset.Position(identifier.Pos()).Line)
			return false
		}
		if expression, ok := node.(ast.Expr); ok && source.FirstParty {
			if value, constant := stringConstant(expression); constant && targeted([]byte(value)) {
				failure = fmt.Errorf("unsupported first-party symbolic name/code string in %s", source.Path)
				return false
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !symbolicLookup(selector.Sel.Name) {
			return true
		}
		if source.FirstParty {
			failure = fmt.Errorf("unsupported first-party symbolic lookup in %s", source.Path)
			return false
		}
		for _, argument := range call.Args {
			if value, ok := stringConstant(argument); ok && targeted([]byte(value)) {
				failure = fmt.Errorf("targeted symbolic lookup in %s", source.Path)
				return false
			}
		}
		return true
	})
	return failure
}

func targetName(name string) bool { return name == "NativeTypes" || name == "ParseStructTag" }

func symbolicLookup(name string) bool {
	return name == "Lookup" || name == "MethodByName" || name == "FuncForPC"
}

func stringConstant(expression ast.Expr) (string, bool) {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			text, err := strconv.Unquote(value.Value)
			return text, err == nil
		}
	case *ast.ParenExpr:
		return stringConstant(value.X)
	case *ast.BinaryExpr:
		if value.Op == token.ADD {
			left, lok := stringConstant(value.X)
			right, rok := stringConstant(value.Y)
			return left + right, lok && rok
		}
	}
	return "", false
}

func inspectNative(source sourceFile) error {
	if targeted(source.Data) {
		return fmt.Errorf("unsupported native symbol reference in %s", source.Path)
	}
	return nil
}

func completeDeclarations(declarations map[string]int) error {
	if declarations["NativeTypes"] != 1 || declarations["ParseStructTag"] != 1 || len(declarations) != 2 {
		return errors.New("expected exactly the two canonical CEL declarations")
	}
	return nil
}
