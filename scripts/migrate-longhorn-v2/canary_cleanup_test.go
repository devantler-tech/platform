package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNativeRestoreRetirementRequiresAbsenceWithoutDeletingAnything(t *testing.T) {
	for _, remaining := range []string{"", "uid-restore", "replacement"} {
		original := resource("restore")
		c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
			request := strings.Join(args, " ")
			if program != "kubectl" || !strings.Contains(request, " get restores.velero.io restore ") {
				t.Fatal("native restore cleanup issued something other than its fixed read")
			}
			if remaining == "" {
				if !strings.Contains(request, "--ignore-not-found") {
					return nil, errors.New("native deletion already removed the Restore")
				}
				return nil, nil
			}
			current := resource("restore")
			current["metadata"].(object)["uid"] = remaining
			return json.Marshal(current)
		}}
		p := canaryRun{c: c}
		err := p.requireNativeRestoreGone(t.Context(), original)
		if (err == nil) != (remaining == "") {
			t.Fatalf("remaining identity %s: %v", remaining, err)
		}
	}
}
