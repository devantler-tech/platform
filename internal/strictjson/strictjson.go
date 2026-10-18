// Package strictjson refuses JSON documents that a typed decode would read
// ambiguously. encoding/json keeps the last of two duplicate members, matches
// field names case-insensitively and ignores a trailing document, so evidence
// that relies on a typed decode alone can be satisfied by a value the producer
// never meant. Callers validate the raw document here first, then decode it.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// MaxDepth bounds object and array nesting.
const MaxDepth = 64

// Validate accepts exactly one JSON value with no duplicate object member
// names and nesting no deeper than MaxDepth. canonical maps an object path to
// the member names the caller decodes there ("" is the root, "a.b" a nested
// object, "a[]" the elements of array a); a member that differs from one of
// those names only by letter case is refused, because a typed decode would
// read it as that member. Members the caller does not decode keep any spelling.
func Validate(data []byte, canonical map[string][]string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(string, int) error
	walk = func(path string, depth int) error {
		if depth > MaxDepth {
			return errors.New("evidence nesting exceeds bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("incomplete evidence")
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil && endsInput(err, len(data)) {
					return errors.New("incomplete evidence")
				}
				if err != nil || !ok || seen[key] {
					return errors.New("ambiguous evidence fields")
				}
				seen[key] = true
				for _, name := range canonical[path] {
					if key != name && strings.EqualFold(key, name) {
						return errors.New("case-aliased evidence field")
					}
				}
				child := key
				if path != "" {
					child = path + "." + key
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(path+"[]", depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected evidence delimiter")
		}
		end, err := decoder.Token()
		if err != nil || (delimiter == '{' && end != json.Delim('}')) || (delimiter == '[' && end != json.Delim(']')) {
			return errors.New("incomplete evidence")
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing evidence")
	}
	return nil
}

// endsInput reports whether err says the document stopped before a value was
// complete. Go releases disagree on how a decoder reports that: some return
// io.EOF, others a syntax error positioned at the end of the input.
func endsInput(err error, length int) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var syntax *json.SyntaxError
	return errors.As(err, &syntax) && syntax.Offset >= int64(length)
}
