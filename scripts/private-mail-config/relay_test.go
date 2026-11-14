package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRelayPassword has an SES SMTP password's structure: version byte 4 and
// a 32-byte signature, Base64 encoded. It authenticates nowhere.
var testRelayPassword = relayPasswordWithVersion(0x04)

func relayPasswordWithVersion(version byte) string {
	return base64.StdEncoding.EncodeToString(append([]byte{version}, bytes.Repeat([]byte{0x5a}, 32)...))
}

// TestRelayPasswordMustBeAnSESSMTPPassword refuses values that would only fail
// once mail is sent: a raw IAM secret, another version, truncation or wrapping.
func TestRelayPasswordMustBeAnSESSMTPPassword(t *testing.T) {
	if !validRelayPassword(testRelayPassword) {
		t.Fatal("structurally valid SES SMTP password refused")
	}
	for name, value := range map[string]string{
		"raw IAM secret":   "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"other version":    relayPasswordWithVersion(0x02),
		"truncated":        testRelayPassword[:43],
		"extended":         testRelayPassword + "A",
		"wrapped":          testRelayPassword[:22] + "\n" + testRelayPassword[22:],
		"trailing newline": testRelayPassword[:43] + "\n",
		"not Base64":       strings.Repeat("-", 44),
		"empty":            "",
	} {
		if validRelayPassword(value) {
			t.Fatalf("%s accepted as an SES SMTP password", name)
		}
	}
}

// relayInputs writes a complete private input set for the relay tests.
func relayInputs(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "inputs")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"listener-inventory.json": string(emptyInventory(t)),
		"settings.json":           `{"domain":"example.test","hostname":"mail.example.test","maintainer":"maintainer","agent":"agents","administrator":"operator"}`,
		"administrator-password":  strings.Repeat("A", 40), "maintainer-password": strings.Repeat("B", 40), "agent-password": strings.Repeat("C", 40),
		"relay-username": "AKIAEXAMPLE000000000", "relay-password": testRelayPassword,
	} {
		if err := os.WriteFile(filepath.Join(input, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return input, filepath.Join(dir, "plan.ndjson")
}

// TestRelayCredentialIsRequiredAndNeverEchoed keeps a missing or malformed
// relay login from producing a plan that would send without authentication.
func TestRelayCredentialIsRequiredAndNeverEchoed(t *testing.T) {
	input, output := relayInputs(t)
	var log bytes.Buffer
	if command([]string{"--input-dir", input, "--output-file", output}, &log) != 0 {
		t.Fatalf("complete relay inputs refused: %s", &log)
	}
	plan, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plan, []byte(`"object":"MtaRoute"`)) || !bytes.Contains(plan, []byte(`"object":"MtaOutboundStrategy"`)) {
		t.Fatal("plan does not route remote mail through the relay")
	}
	for _, name := range []string{"relay-username", "relay-password"} {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{"", "private-canary-" + strings.Repeat("E", 40)} {
				input, output := relayInputs(t)
				path := filepath.Join(input, name)
				if value == "" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
				var log bytes.Buffer
				if command([]string{"--input-dir", input, "--output-file", output}, &log) == 0 {
					t.Fatal("missing or malformed relay credential accepted")
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("refused relay credential left an output plan")
				}
				if strings.Contains(log.String(), "canary") || strings.Contains(log.String(), "AKIA") {
					t.Fatal("relay credential echoed")
				}
			}
		})
	}
}

// TestRelayPasswordIsNeverReadBack keeps verification from requesting or
// comparing the stored secret, which the server masks on read.
func TestRelayPasswordIsNeverReadBack(t *testing.T) {
	fields := publicPolicyFields(map[string]any{
		"name": "ses", "authUsername": "AKIAEXAMPLE000000000",
		"authSecret": map[string]any{"@type": "Value", "secret": testRelayPassword},
	})
	if _, present := fields["authSecret"]; present || fields["authUsername"] != "AKIAEXAMPLE000000000" || fields["name"] != "ses" || len(fields) != 2 {
		t.Fatal("relay secret is read back, or public relay fields are skipped")
	}
}
