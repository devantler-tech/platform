// guard-bash-local-initialization detects textual reads of uninitialized locals
// in nounset Bash scripts. It parses source without executing it; it is not a
// control-flow or interprocedural analyzer.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type event struct {
	pos        syntax.Pos
	kind, name string
	enabled    bool
}

// arithmeticEvents preserves reads before writes for supported arithmetic forms.
func arithmeticEvents(expr syntax.ArithmExpr) []event {
	if expr == nil {
		return nil
	}
	switch n := expr.(type) {
	case *syntax.Word:
		if name := n.Lit(); name != "" {
			return []event{{pos: n.Pos(), kind: "read", name: name}}
		}
		var events []event
		syntax.Walk(n, func(node syntax.Node) bool {
			if param, ok := node.(*syntax.ParamExp); ok {
				events = append(events, event{pos: param.Pos(), kind: "read", name: param.Param.Value})
			}
			return true
		})
		return events
	case *syntax.BinaryArithm:
		if n.Op.String() == "=" {
			events := arithmeticEvents(n.Y)
			if word, ok := n.X.(*syntax.Word); ok {
				events = append(events, event{pos: n.End(), kind: "write", name: word.Lit()})
			}
			return events
		}
		return append(arithmeticEvents(n.X), arithmeticEvents(n.Y)...)
	case *syntax.UnaryArithm:
		return arithmeticEvents(n.X)
	case *syntax.ParenArithm:
		return arithmeticEvents(n.X)
	}
	return nil
}

// setNounset recognizes static set options, stopping at positional arguments.
func setNounset(call *syntax.CallExpr) (bool, bool) {
	if len(call.Args) < 2 || call.Args[0].Lit() != "set" {
		return false, false
	}
	known, enabled := false, false
	for i := 1; i < len(call.Args); i++ {
		arg := call.Args[i].Lit()
		if arg == "--" || len(arg) < 2 || (arg[0] != '-' && arg[0] != '+') {
			break
		}
		if arg == "-o" || arg == "+o" {
			if i+1 < len(call.Args) {
				i++
				if call.Args[i].Lit() == "nounset" {
					known, enabled = true, arg[0] == '-'
				}
			}
			continue
		}
		if strings.Contains(arg[1:], "u") {
			known, enabled = true, arg[0] == '-'
		}
	}
	return enabled, known
}

// Assignment builtins are recognized only with literal destination names.
func builtinWrites(call *syntax.CallExpr) []string {
	if len(call.Args) == 0 {
		return nil
	}
	args := call.Args[1:]
	switch call.Args[0].Lit() {
	case "printf":
		if len(args) >= 2 && args[0].Lit() == "-v" {
			return []string{args[1].Lit()}
		}
	case "read", "mapfile", "readarray":
		var names []string
		for i := 0; i < len(args); i++ {
			arg := args[i].Lit()
			if arg == "" {
				return nil
			}
			if arg == "--" {
				for _, word := range args[i+1:] {
					names = append(names, word.Lit())
				}
				break
			}
			if strings.HasPrefix(arg, "-") {
				// These options consume a separate argument, not a destination.
				if arg == "-a" {
					if i+1 < len(args) {
						i++
						names = append(names, args[i].Lit())
					}
					continue
				}
				if arg == "-d" || arg == "-n" || arg == "-N" || arg == "-p" || arg == "-t" || arg == "-u" || arg == "-O" || arg == "-s" || arg == "-c" || arg == "-C" {
					i++
				}
				continue
			}
			names = append(names, arg)
		}
		return names
	}
	return nil
}

// lintSource orders declaration, read and write events within each function.
func lintSource(path string, input io.Reader) ([]string, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(input, path)
	if err != nil {
		return nil, err
	}
	nounset := false
	var functions []*syntax.FuncDecl
	syntax.Walk(file, func(node syntax.Node) bool {
		if fn, ok := node.(*syntax.FuncDecl); ok {
			functions = append(functions, fn)
		}
		return true
	})
	syntax.Walk(file, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.FuncDecl); ok {
			return false
		}
		if call, ok := node.(*syntax.CallExpr); ok {
			if enabled, known := setNounset(call); known {
				nounset = enabled
			}
		}
		return true
	})
	var findings []string
	for _, fn := range functions {
		var events []event
		declarationEnds := map[*syntax.Assign]syntax.Pos{}
		globalAssignments := map[*syntax.Assign]bool{}
		syntax.Walk(fn.Body, func(node syntax.Node) bool {
			switch n := node.(type) {
			case *syntax.FuncDecl:
				// A nested function has its own local scope.
				return false
			case *syntax.DeclClause:
				if n.Variant.Value != "local" && n.Variant.Value != "declare" && n.Variant.Value != "typeset" {
					break
				}
				// Global and array destinations have different initialization rules,
				// but their initializers still read previously declared locals.
				skipDeclarations := false
				globalDeclaration := false
				for _, arg := range n.Args {
					if arg.Name == nil && arg.Value != nil {
						option := arg.Value.Lit()
						if strings.HasPrefix(option, "-") && strings.ContainsAny(option[1:], "gaA") {
							skipDeclarations = true
							globalDeclaration = globalDeclaration || strings.Contains(option[1:], "g")
						}
					}
				}
				for _, arg := range n.Args {
					if arg.Name != nil {
						// Every argument expands before the declaration builtin runs.
						declarationEnds[arg] = n.End()
						globalAssignments[arg] = globalDeclaration
					}
				}
				if skipDeclarations {
					break
				}
				for _, arg := range n.Args {
					if arg.Name != nil {
						events = append(events, event{pos: n.End(), kind: "local", name: arg.Name.Value})
					}
				}
			case *syntax.Assign:
				if n.Name != nil && !n.Naked && !globalAssignments[n] {
					position := n.End()
					if end, declaration := declarationEnds[n]; declaration {
						position = end
					}
					events = append(events, event{pos: position, kind: "write", name: n.Name.Value})
				}
			case *syntax.ParamExp:
				if n.Exp != nil {
					switch n.Exp.Op.String() {
					case ":=", "=":
						events = append(events, event{pos: n.End(), kind: "write", name: n.Param.Value})
						return true
					case ":-", "-", ":+", "+", ":?", "?":
						return true
					}
				}
				events = append(events, event{pos: n.Pos(), kind: "read", name: n.Param.Value})
			case *syntax.WordIter:
				events = append(events, event{pos: n.End(), kind: "write", name: n.Name.Value})
			case *syntax.CStyleLoop:
				events = append(events, arithmeticEvents(n.Init)...)
				events = append(events, arithmeticEvents(n.Cond)...)
				events = append(events, arithmeticEvents(n.Post)...)
				return false
			case *syntax.ArithmCmd:
				events = append(events, arithmeticEvents(n.X)...)
				return false
			case *syntax.ArithmExp:
				events = append(events, arithmeticEvents(n.X)...)
				return false
			case *syntax.CallExpr:
				if enabled, known := setNounset(n); known {
					events = append(events, event{pos: n.End(), kind: "set", enabled: enabled})
				}
				for _, name := range builtinWrites(n) {
					if name != "" {
						events = append(events, event{pos: n.End(), kind: "write", name: name})
					}
				}
			}
			return true
		})
		sort.SliceStable(events, func(i, j int) bool { return events[i].pos.Offset() < events[j].pos.Offset() })
		locals, reported := map[string]bool{}, map[string]bool{}
		enabled := nounset
		for _, e := range events {
			switch e.kind {
			case "local":
				if _, exists := locals[e.name]; !exists {
					locals[e.name] = false
				}
			case "write":
				if _, exists := locals[e.name]; exists {
					locals[e.name] = true
				}
			case "set":
				enabled = e.enabled
			case "read":
				if initialized, exists := locals[e.name]; exists && !initialized && enabled && !reported[e.name] {
					findings = append(findings, fmt.Sprintf("%s:%d:%d: function %s reads local %s before assignment under nounset", path, e.pos.Line(), e.pos.Col(), fn.Name.Value, e.name))
					reported[e.name] = true
				}
			}
		}
	}
	return findings, nil
}

// run reads caller-selected local paths and returns 2 when coverage is unknown.
func run(paths []string, output io.Writer) int {
	files := map[string]bool{}
	unknown := false
	for _, path := range paths {
		err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(name, ".sh") {
				files[name] = true
			}
			return nil
		})
		if err != nil {
			if _, err := fmt.Fprintf(output, "UNKNOWN: %s: %v\n", path, err); err != nil {
				return 2
			}
			unknown = true
		}
	}
	if len(files) == 0 {
		_, _ = fmt.Fprintln(output, "UNKNOWN: no shell files examined") // Already failing closed.
		return 2
	}
	ordered := make([]string, 0, len(files))
	for path := range files {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	examined, count := 0, 0
	for _, path := range ordered {
		file, err := os.Open(path)
		if err != nil {
			if _, err := fmt.Fprintf(output, "UNKNOWN: %s: %v\n", path, err); err != nil {
				return 2
			}
			unknown = true
			continue
		}
		findings, err := lintSource(path, file)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			if _, err := fmt.Fprintf(output, "UNKNOWN: %s: parse=%v close=%v\n", path, err, closeErr); err != nil {
				return 2
			}
			unknown = true
			continue
		}
		examined++
		for _, finding := range findings {
			if _, err := fmt.Fprintln(output, finding); err != nil {
				return 2
			}
			count++
		}
	}
	if _, err := fmt.Fprintf(output, "examined=%d findings=%d\n", examined, count); err != nil {
		return 2
	}
	if unknown {
		return 2
	}
	if count > 0 {
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }
