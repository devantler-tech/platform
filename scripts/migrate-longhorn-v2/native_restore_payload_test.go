package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const restorePayloadPodUID = "11111111-2222-3333-4444-555555555555"

func restorePayloadFixture(t *testing.T) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("the native payload fixture requires sha256sum")
	}
	dir := t.TempDir()
	file, err := os.Create(filepath.Join(dir, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(134217728); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sha256sum", "payload.bin")
	cmd.Dir = dir
	checksum, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string][]byte{"payload.sha256": checksum, "sentinel": []byte("longhorn-native-canary-5582\n")} {
		if err = os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, strings.Fields(string(checksum))[0], strings.Repeat("c", 64)
}

func runRestorePayloadScript(t *testing.T, dir, script string) (string, error) {
	return runRestorePayloadScriptWithUID(t, dir, script, restorePayloadPodUID)
}

func runRestorePayloadScriptWithUID(t *testing.T, dir, script, uid string) (string, error) {
	t.Helper()
	// Only the mount path changes for the standalone native shell fixture.
	// The produced script otherwise executes exactly as it will in the Pod.
	if strings.Count(script, "cd /data\n") != 1 {
		t.Fatal("the payload script lacks its single fixed mount")
	}
	script = strings.Replace(script, "cd /data\n", "cd \"$CANARY_TEST_DATA\"\n", 1)
	cmd := exec.Command("/bin/sh", "-ec", script)
	cmd.Env = append(os.Environ(), "CANARY_TEST_DATA="+dir, "LONGHORN_NATIVE_RESTORE_POD_UID="+uid)
	output, err := cmd.Output()
	if failure, ok := err.(*exec.ExitError); ok {
		t.Logf("native shell failure: %s", failure.Stderr)
	}
	return string(output), err
}

func TestNativeRestoreWritesOnceAndVerifiesBothOriginalBytesAndNewToken(t *testing.T) {
	dir, original, token := restorePayloadFixture(t)
	read, err := nativeRestoreVerifyScript(original, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runRestorePayloadScript(t, dir, read); err == nil {
		t.Fatal("the restored source alone was accepted as a post-restore write")
	}
	write, err := nativeRestoreWriteScript(original, token, restorePayloadPodUID)
	if err != nil {
		t.Fatal(err)
	}
	output, err := runRestorePayloadScript(t, dir, write)
	if err != nil || verifyNativeRestorePayload(output, original, token) != nil {
		t.Fatal("the original checksum and synchronized new write were not proven", err, output)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "reattach-token"))
	if err != nil || string(contents) != token+"\n" {
		t.Fatal("the new token differs from its original intent", err)
	}
	if _, err = runRestorePayloadScript(t, dir, write); err == nil {
		t.Fatal("a replayed or uncertain write silently overwrote/adopted the token")
	}
	output, err = runRestorePayloadScript(t, dir, read)
	if err != nil || verifyNativeRestorePayload(output, original, token) != nil {
		t.Fatal("a read-only reattachment check could not prove both original bytes and the first write", err)
	}
}

func TestNativeRestoreRefusesChangedPayloadBeforeWritingAndPreservesExistingFiles(t *testing.T) {
	for _, fault := range []string{"changed payload and checksum", "wrong sentinel", "short payload", "existing token", "token symlink"} {
		t.Run(fault, func(t *testing.T) {
			dir, original, token := restorePayloadFixture(t)
			path := filepath.Join(dir, "reattach-token")
			switch fault {
			case "changed payload and checksum":
				file, err := os.OpenFile(filepath.Join(dir, "payload.bin"), os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteAt([]byte("changed"), 0)
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
				cmd := exec.Command("sha256sum", "payload.bin")
				cmd.Dir = dir
				checksum, err := cmd.Output()
				if err != nil || os.WriteFile(filepath.Join(dir, "payload.sha256"), checksum, 0o600) != nil {
					t.Fatal("could not prepare the changed checksum", err)
				}
			case "wrong sentinel":
				if err := os.WriteFile(filepath.Join(dir, "sentinel"), []byte("other\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "short payload":
				if err := os.Truncate(filepath.Join(dir, "payload.bin"), 8); err != nil {
					t.Fatal(err)
				}
			case "existing token":
				if err := os.WriteFile(path, []byte("preserve\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "token symlink":
				if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
					t.Fatal(err)
				}
			}
			script, err := nativeRestoreWriteScript(original, token, restorePayloadPodUID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = runRestorePayloadScript(t, dir, script); err == nil {
				t.Fatal("an invalid source or occupied token path was accepted")
			}
			if fault == "existing token" {
				contents, err := os.ReadFile(path)
				if err != nil || string(contents) != "preserve\n" {
					t.Fatal("the earlier write was changed", err)
				}
			} else if fault != "token symlink" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("the failed source check still created a token", err)
				}
			} else if _, err := os.Lstat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
				t.Fatal("the write followed the token symlink", err)
			}
		})
	}
}

func TestNativeRestorePayloadRejectsBorrowedOrAmbiguousReceiptsAndShellInput(t *testing.T) {
	original, token := strings.Repeat("a", 64), strings.Repeat("b", 64)
	digest := sha256.Sum256([]byte(token + "\n"))
	output := original + "  payload.bin\n" + hex.EncodeToString(digest[:]) + "  reattach-token\n"
	if verifyNativeRestorePayload(output, original, token) != nil {
		t.Fatal("the exact original payload and new token hashes were refused")
	}
	for _, altered := range []string{
		strings.Replace(output, original, token, 1),
		strings.Replace(output, "reattach-token", "other", 1),
		strings.Replace(output, hex.EncodeToString(digest[:]), original, 1),
		output + "extra\n", "", original + "  payload.bin\n",
	} {
		if verifyNativeRestorePayload(altered, original, token) == nil {
			t.Fatal("foreign or incomplete payload evidence was accepted")
		}
	}
	for _, invalid := range []string{"", "../token", token + "\n", "$(touch /data/other)", strings.Repeat("F", 64)} {
		for _, inputs := range [][2]string{{invalid, token}, {original, invalid}} {
			if script, err := nativeRestoreWriteScript(inputs[0], inputs[1], restorePayloadPodUID); err == nil || script != "" {
				t.Fatal("unvalidated shell input produced a write script")
			}
			if script, err := nativeRestoreVerifyScript(inputs[0], inputs[1]); err == nil || script != "" {
				t.Fatal("unvalidated shell input produced a verification script")
			}
			if verifyNativeRestorePayload(output, inputs[0], inputs[1]) == nil {
				t.Fatal("invalid original intent accepted a receipt")
			}
		}
	}
}
