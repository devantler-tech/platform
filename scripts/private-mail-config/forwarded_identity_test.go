package main

import (
	"encoding/json"
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
			v := fixture()
			if err := json.Unmarshal([]byte(tc.setting), &v.settings); err != nil {
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
