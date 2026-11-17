package main

import (
	"context"
	"testing"
)

func TestNativeSeedCannotConstructAProductionOperationOutsideItsGrant(t *testing.T) {
	for _, fault := range []string{"ordinary client", "read-only context", "branch", "retry", "other confirmation", "other source", "other run"} {
		t.Run(fault, func(t *testing.T) {
			env := nativeSeedDispatch()
			c := client{context: "admin@prod", nativeSeed: true, run: func(context.Context, []byte, string, ...string) ([]byte, error) {
				t.Fatal("an unadmitted operation reached an external command")
				return nil, nil
			}}
			source, run := env["GITHUB_SHA"], env["GITHUB_RUN_ID"]
			switch fault {
			case "ordinary client":
				c.nativeSeed = false
			case "read-only context":
				c.context = "oidc@prod"
			case "branch":
				env["GITHUB_REF"] = "refs/heads/unsafe"
			case "retry":
				env["GITHUB_RUN_ATTEMPT"] = "2"
			case "other confirmation":
				env["LONGHORN_V2_CONFIRM"] = "benchmark-longhorn-storage"
			case "other source":
				source = "ffffffffffffffffffffffffffffffffffffffff"
			case "other run":
				run = "5678"
			}
			for k, v := range env {
				t.Setenv(k, v)
			}
			if err := runNativeSeed(t.Context(), c, source, run); err == nil {
				t.Fatal("the canary source admitted another workflow, owner or operation")
			}
		})
	}
}
