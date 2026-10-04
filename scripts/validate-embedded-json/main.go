// Command validate-embedded-json checks registered ConfigMap data keys and
// keys ending in .json. Literal blocks and inline JSON are checked; folded
// blocks are refused, and encrypted files/values are never inspected.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Add a key here when a ConfigMap embeds JSON without a .json suffix.
var registeredKeys = map[string]bool{"exceptionPolicies": true}

type blob struct {
	key, value, style string
	line              int
}

// This scanner retains the existing source selection rather than treating
// every YAML scalar as JSON. Schema validation separately checks the YAML.
func keyLine(line string) (indent int, key, value string, ok bool) {
	start := 0
	for offset, r := range line {
		if !unicode.IsSpace(r) {
			start = offset
			break
		}
		indent++
		start = offset + utf8.RuneLen(r)
	}
	if start >= len(line) || line[start] == '#' || line[start] == ':' {
		return 0, "", "", false
	}
	colon := strings.IndexByte(line[start:], ':')
	if colon < 0 {
		return 0, "", "", false
	}
	colon += start
	return indent, line[start:colon], strings.TrimLeft(line[colon+1:], " \t"), true
}

func leadingSpace(line string) int {
	count := 0
	for _, r := range line {
		if !unicode.IsSpace(r) {
			break
		}
		count++
	}
	return count
}

func blockScalar(lines []string, start, end, keyIndent int) (string, int) {
	index, dedent := start, -1
	for index < end {
		line := lines[index]
		if strings.TrimSpace(line) != "" {
			indent := leadingSpace(line)
			if indent <= keyIndent {
				break
			}
			if dedent < 0 || indent < dedent {
				dedent = indent
			}
		}
		index++
	}
	if dedent < 0 {
		return "", index
	}
	content := make([]string, 0, index-start)
	for _, line := range lines[start:index] {
		if strings.TrimSpace(line) == "" {
			content = append(content, "")
		} else {
			content = append(content, string([]rune(line)[dedent:]))
		}
	}
	return strings.Join(content, "\n"), index
}

func emptyOrComment(value string) bool {
	value = strings.Trim(value, " \t")
	return value == "" || strings.HasPrefix(value, "#")
}

func scalarWithOptionalComment(value, scalar string) bool {
	if value == scalar {
		return true
	}
	if !strings.HasPrefix(value, scalar) {
		return false
	}
	tail := value[len(scalar):]
	return len(tail) > 0 && (tail[0] == ' ' || tail[0] == '\t') && emptyOrComment(tail)
}

func embeddedValues(text string) []blob {
	// The previous reader used universal newlines. Normalize before scanning so
	// CRLF and CR inputs retain the same source selection and line positions.
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	// Match splitlines: the final line break terminates the preceding line; it
	// does not append another blank line to a literal block's JSON text.
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var blobs []blob
	for start := 0; start < len(lines); {
		end := start
		isConfigMap := false
		for end < len(lines) && !scalarWithOptionalComment(lines[end], "---") {
			if strings.HasPrefix(lines[end], "kind:") && scalarWithOptionalComment(strings.TrimLeft(lines[end][5:], " \t"), "ConfigMap") {
				isConfigMap = true
			}
			end++
		}
		if isConfigMap {
			inData, dataIndent := false, -1
			for index := start; index < end; {
				indent, key, value, ok := keyLine(lines[index])
				lineNo := index + 1
				index++
				if !ok {
					continue
				}
				if indent == 0 {
					inData, dataIndent = key == "data" && emptyOrComment(value), -1
					continue
				}
				if !inData {
					continue
				}
				if dataIndent < 0 {
					dataIndent = indent
				}
				if indent != dataIndent || (!registeredKeys[key] && !strings.HasSuffix(key, ".json")) {
					continue
				}
				style := "plain"
				if strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">") {
					if value[0] == '>' {
						style = "folded"
					} else {
						style = "block"
					}
					value, index = blockScalar(lines, index, end, indent)
				} else if value == "" {
					continue
				} else if len(value) > 1 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
					value = value[1 : len(value)-1]
				}
				blobs = append(blobs, blob{key: key, value: value, style: style, line: lineNo})
			}
		}
		start = end + 1
	}
	return blobs
}

// The existing decoder accepts NaN and +/-Infinity. Preserve only complete,
// unquoted JSON value tokens. Padding retains byte offsets for diagnostics;
// identifiers, string contents and escapes are never rewritten.
func legacyConstants(value string) string {
	result := []byte(value)
	inString, escaped := false, false
	for index := 0; index < len(value); index++ {
		if inString {
			if escaped {
				escaped = false
			} else if value[index] == '\\' {
				escaped = true
			} else if value[index] == '"' {
				inString = false
			}
			continue
		}
		if value[index] == '"' {
			inString = true
			continue
		}
		if index > 0 && !strings.ContainsRune(" \t\r\n[:,", rune(value[index-1])) {
			continue
		}
		for _, token := range []string{"-Infinity", "Infinity", "NaN"} {
			end := index + len(token)
			if !strings.HasPrefix(value[index:], token) || (end < len(value) && !strings.ContainsRune(" \t\r\n]},", rune(value[end]))) {
				continue
			}
			result[index] = '0'
			for position := index + 1; position < end; position++ {
				result[position] = ' '
			}
			index = end - 1
			break
		}
	}
	return string(result)
}

func jsonProblem(value string) string {
	var raw json.RawMessage
	err := json.Unmarshal([]byte(legacyConstants(value)), &raw)
	if err == nil {
		return ""
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		return err.Error()
	}
	offset := int(syntax.Offset) - 1
	if strings.HasPrefix(err.Error(), "unexpected end") {
		offset = len(value)
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(value) {
		offset = len(value)
	}
	prefix := value[:offset]
	line := strings.Count(prefix, "\n") + 1
	column := utf8.RuneCountInString(prefix[strings.LastIndexByte(prefix, '\n')+1:]) + 1
	return fmt.Sprintf("%s: line %d column %d (char %d)", err, line, column, utf8.RuneCountInString(prefix))
}

func run(args []string, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate-embedded-json", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "repository root containing k8s/")
	if flags.Parse(args) != nil {
		return 2
	}
	if flags.NArg() != 0 {
		// Diagnostic output is best effort; this path already exits unsuccessfully.
		_, _ = fmt.Fprintln(stderr, "usage: validate-embedded-json [-root <repository>]")
		return 2
	}
	var invalid, folded []string
	checked := 0
	err := filepath.WalkDir(filepath.Join(*root, "k8s"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) || strings.HasSuffix(name, ".enc.yaml") {
			return nil
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(*root, path)
		if err != nil {
			return err
		}
		for _, item := range embeddedValues(strings.ToValidUTF8(string(text), "\uFFFD")) {
			if strings.Contains(item.value, "ENC[") {
				continue
			}
			where := fmt.Sprintf("%s:%d", filepath.ToSlash(rel), item.line)
			if item.style == "folded" {
				folded = append(folded, fmt.Sprintf("%s  (%s)", where, item.key))
				continue
			}
			checked++
			if problem := jsonProblem(item.value); problem != "" {
				invalid = append(invalid, fmt.Sprintf("%s  (%s: %s)", where, item.key, problem))
			}
		}
		return nil
	})
	if err != nil {
		// A failed diagnostic write cannot change the unsuccessful scan result.
		_, _ = fmt.Fprintf(stderr, "validate-embedded-json: cannot complete validation: %v\n", err)
		return 1
	}
	writeResult := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(out, format, args...); err != nil {
			// There is no further output channel; the caller still fails closed.
			_, _ = fmt.Fprintf(stderr, "validate-embedded-json: cannot write validation result: %v\n", err)
			return false
		}
		return true
	}
	for _, category := range []struct {
		items []string
		label string
	}{
		{invalid, "Embedded JSON does not parse"},
		{folded, "Embedded JSON in a folded scalar — use a literal block scalar '|'"},
	} {
		if len(category.items) == 0 {
			continue
		}
		sort.Strings(category.items)
		if !writeResult("\n✗ %s (%d):\n", category.label, len(category.items)) {
			return 1
		}
		for _, item := range category.items {
			if !writeResult("   %s\n", item) {
				return 1
			}
		}
	}
	if problems := len(invalid) + len(folded); problems > 0 {
		writeResult("\n%d embedded-JSON violation(s). See scripts/validate-embedded-json/.\n", problems)
		return 1
	}
	if !writeResult("✓ %d embedded JSON blob(s) parse cleanly.\n", checked) {
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
