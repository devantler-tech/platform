package main

import (
	"slices"
	"testing"
)

func TestCredentialModesKeepCanonicalRecoverySeparateFromLegacyCopy(t *testing.T) {
	legacy := []string{"apps/private-mail/admin", "apps/private-mail/maintainer", "apps/private-mail/agent"}
	canonical := []string{"infrastructure/email/admin", "infrastructure/email/maintainer", "infrastructure/email/agent"}
	for _, mode := range []string{"collect", "verify", "fixture"} {
		for _, suffix := range []string{"", "-email"} {
			t.Run(mode+suffix, func(t *testing.T) {
				base, paths, ok := credentialMode(mode + suffix)
				want := legacy
				if suffix != "" {
					want = canonical
				}
				if !ok || base != mode || !slices.Equal(paths, want) {
					t.Fatal("recovery mode selected the wrong fixed credential set")
				}
			})
		}
	}
	for _, mode := range []string{"", "image-email", "verify-email-email", "collect-other", "verify/email", "../collect", "fixture-email\n"} {
		if _, _, ok := credentialMode(mode); ok {
			t.Fatal("unsupported recovery mode was accepted")
		}
	}
}
