package main

import (
	"errors"
	"strings"
	"testing"
)

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("fixture read failed") }

func TestRenderedPayloadAdmission(t *testing.T) {
	for _, kind := range []string{"role", "policy"} {
		t.Run(kind, func(t *testing.T) {
			body := expectedPayload(kind)
			duplicatePrefix := `{"path":{},`
			if kind == "role" {
				duplicatePrefix = `{"token_max_ttl":0,`
			}
			if err := run([]string{kind}, strings.NewReader("\n"+body+"\n")); err != nil {
				t.Fatalf("valid native JSON: %v", err)
			}
			for name, invalid := range map[string]string{
				"multiple":  body + "\n" + body,
				"duplicate": duplicatePrefix + strings.TrimPrefix(body, "{"),
				"missing":   `{}`,
				"array":     "[" + body + "]",
				"malformed": body[:len(body)-1],
				"oversized": strings.Repeat(" ", 16<<10) + body,
			} {
				t.Run(name, func(t *testing.T) {
					if err := run([]string{kind}, strings.NewReader(invalid)); err == nil {
						t.Fatal("invalid input was accepted")
					}
				})
			}
			if err := run([]string{kind}, brokenReader{}); err == nil {
				t.Fatal("failed read was accepted")
			}
		})
	}
	for _, args := range [][]string{nil, {"unknown"}, {"role", "policy"}} {
		if err := run(args, strings.NewReader(expectedPayload("role"))); err == nil {
			t.Fatal("invalid invocation was accepted")
		}
	}
}
