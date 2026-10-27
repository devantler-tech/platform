package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardedClientIdentityRequiresExplicitOptInAndRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		want          bool
	}{
		{"existing installations", `{}`, false},
		{"trusted transport activated", `{"useForwardedClientIP":true}`, true},
		{"explicit rollback", `{"useForwardedClientIP":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := bootstrapInputs(t)
			path := filepath.Join(dir, "settings.json")
			base, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.setting != `{}` {
				base = []byte(strings.TrimSuffix(string(base), "}") + "," + tc.setting[1:])
			}
			if err := os.WriteFile(path, base, 0600); err != nil {
				t.Fatal(err)
			}
			v, err := loadInputs(dir)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := render(v)
			if err != nil {
				t.Fatal(err)
			}
			seen := 0
			for _, line := range strings.Split(strings.TrimSpace(string(plan)), "\n") {
				var op operation
				if err := json.Unmarshal([]byte(line), &op); err != nil {
					t.Fatal(err)
				}
				if op.Object == "Http" && op.Type == "update" {
					seen++
					if got, ok := op.Value["useXForwarded"].(bool); !ok || got != tc.want {
						t.Fatalf("forwarded identity = %v, want %v", op.Value["useXForwarded"], tc.want)
					}
				}
			}
			if seen != 1 {
				t.Fatalf("expected one explicit HTTP policy, got %d", seen)
			}
		})
	}
}

func TestForwardedClientIdentityRejectsMalformedPrivateSettings(t *testing.T) {
	for _, value := range []string{`null`, `"true"`, `1`, `{}`, `[]`, `true,"useForwardedClientIP":false`} {
		t.Run(value, func(t *testing.T) {
			dir := bootstrapInputs(t)
			path := filepath.Join(dir, "settings.json")
			base, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data := strings.TrimSuffix(string(base), "}") + `,"useForwardedClientIP":` + value + `}`
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadInputs(dir); err == nil {
				t.Fatal("malformed or repeated forwarding policy accepted")
			}
		})
	}
}
