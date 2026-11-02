package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestObservedResourceNamesCannotChangeKubectlScope(t *testing.T) {
	for _, tc := range []struct{ kind, namespace, name string }{
		{"volumes.longhorn.io", "longhorn-system", "--context=other"},
		{"volumes.longhorn.io", "longhorn-system", "../../secrets"},
		{"--raw=/api/v1/secrets", "longhorn-system", "volume"},
		{"volumes.longhorn.io", "../other", "volume"},
		{"volumes.longhorn.io", "longhorn-system", "volume\nother"},
	} {
		calls := 0
		c := client{context: "admin@prod", run: func(context.Context, []byte, string, ...string) ([]byte, error) {
			calls++
			return json.Marshal(resource("volume"))
		}}
		if _, err := c.get(context.Background(), tc.kind, tc.namespace, tc.name, false); err == nil || calls != 0 {
			t.Fatal("observed data could select a command option, path or namespace")
		}
	}
}
