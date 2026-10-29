package main

import "testing"

func TestForwardAddressAcceptsOnlyTheOwnedLoopbackPort(t *testing.T) {
	port, ok := forwardPort("Forwarding from 127.0.0.1:49152 -> 8080")
	if !ok || port != "49152" {
		t.Fatal("native ephemeral loopback listener was refused")
	}
	for _, line := range []string{"Forwarding from 0.0.0.0:49152 -> 8080", "Forwarding from 127.0.0.1:0 -> 8080", "Forwarding from 127.0.0.1:99999 -> 8080", "Forwarding from 127.0.0.1:49152 -> 25", "private output Forwarding from 127.0.0.1:49152 -> 8080"} {
		if _, ok := forwardPort(line); ok {
			t.Fatal("unowned or invalid forwarding endpoint was accepted")
		}
	}
}
func TestMailboxSessionMustPreserveTheOriginalAccount(t *testing.T) {
	data := []byte(`{"primaryAccounts":{"urn:ietf:params:jmap:mail":"company-account"},"accounts":{"company-account":{"isReadOnly":false}}}`)
	if account, err := sessionAccount(data, ""); err != nil || account != "company-account" {
		t.Fatal("writable original account was refused")
	}
	if _, err := sessionAccount(data, "another-account"); err == nil {
		t.Fatal("restored session changed account identity")
	}
	for _, data := range []string{`{}`, `{"primaryAccounts":{"urn:ietf:params:jmap:mail":"../foreign"}}`, `{"primaryAccounts":{"urn:ietf:params:jmap:mail":"company-account"},"accounts":{"company-account":{"isReadOnly":true}}}`} {
		if _, err := sessionAccount([]byte(data), ""); err == nil {
			t.Fatal("missing, foreign or readonly account was accepted")
		}
	}
}
