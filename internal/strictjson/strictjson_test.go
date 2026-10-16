package strictjson

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	canonical := map[string][]string{
		"":          {"items", "sealed"},
		"items[]":   {"name"},
		"items[].x": {"value"},
	}
	for _, test := range []struct {
		name, document, err string
	}{
		{"plain object", `{"sealed":false,"items":[{"name":"a","x":{"value":1}}]}`, ""},
		{"unrelated members keep their spelling", `{"sealed":false,"native":{"Label":"one","label":"two"}}`, ""},
		{"scalar document", `true`, ""},
		{"duplicate member", `{"sealed":true,"sealed":false}`, "ambiguous evidence fields"},
		{"escaped duplicate member", `{"sealed":true,"sealed":false}`, "ambiguous evidence fields"},
		{"case alias at the root", `{"Sealed":false}`, "case-aliased evidence field"},
		{"case alias inside an array element", `{"items":[{"Name":"a"}]}`, "case-aliased evidence field"},
		{"case alias in a nested object", `{"items":[{"x":{"VALUE":1}}]}`, "case-aliased evidence field"},
		{"duplicate inside an array element", `{"items":[{"name":"a","name":"b"}]}`, "ambiguous evidence fields"},
		{"trailing document", `{"sealed":false} {}`, "trailing evidence"},
		{"truncated document", `{"sealed":false`, "incomplete evidence"},
		{"empty input", ``, "incomplete evidence"},
		{"nesting beyond the bound", strings.Repeat("[", MaxDepth+2) + strings.Repeat("]", MaxDepth+2), "evidence nesting exceeds bound"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := Validate([]byte(test.document), canonical)
			if test.err == "" {
				if err != nil {
					t.Fatalf("refused valid document: %v", err)
				}
				return
			}
			if err == nil || err.Error() != test.err {
				t.Fatalf("want %q, got %v", test.err, err)
			}
		})
	}
}
