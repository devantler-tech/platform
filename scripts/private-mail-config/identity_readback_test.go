package main

import "testing"

func TestIdentityReadbackRejectsAdditionalRolesAndCallbacks(t *testing.T) {
	for _, key := range []string{"roleIds", "defaultUserRoleIds", "redirectUris", "requireScopes"} {
		t.Run(key, func(t *testing.T) {
			want := map[string]any{key: map[string]any{"declared": true}}
			actual := map[string]any{key: map[string]any{"declared": true, "unexpected": true}}
			if policyMatches(want, actual, "") {
				t.Fatal("identity readback accepted an undeclared role, callback or scope")
			}
			if !policyMatches(want, want, "") {
				t.Fatal("identity readback rejected the exact declared policy")
			}
			empty := map[string]any{key: map[string]any{}}
			if policyMatches(empty, actual, "") || !policyMatches(empty, empty, "") {
				t.Fatal("empty identity policy must refuse retained entries and accept its exact readback")
			}
		})
	}
	if !policyMatches(map[string]any{"description": "managed"}, map[string]any{"description": "managed", "nativeDefault": true}, "") {
		t.Fatal("unrelated native object defaults must remain compatible")
	}
}
