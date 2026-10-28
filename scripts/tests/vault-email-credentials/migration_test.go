package emailcredentials_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const syntheticToken = "test-only-root-token"

type apiFixture struct {
	mu          sync.Mutex
	keys        map[string]map[string]any
	writes      int
	denied      string
	missingKV   bool
	invalidKV   bool
	race        string
	changeAfter bool
	loseReply   bool
}

// credentials supplies synthetic records with extra fields that must survive copying.
func credentials() map[string]map[string]any {
	keys := map[string]map[string]any{}
	for _, account := range []string{"admin", "maintainer", "agent"} {
		keys["apps/private-mail/"+account] = map[string]any{
			"password": strings.Repeat("A", 48) + account,
			"recovery": map[string]any{"preserve": true},
		}
	}
	return keys
}

// serve models KV v2 reads, conditional creation, races and uncertain replies.
func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("X-Vault-Token") != syntheticToken {
		http.Error(w, `{"errors":["authentication refused"]}`, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/sys/mounts" {
		mounts := map[string]any{}
		if !f.missingKV {
			version := "2"
			if f.invalidKV {
				version = "1"
			}
			mounts["secret/"] = map[string]any{"type": "kv", "options": map[string]string{"version": version}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": mounts})
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/v1/secret/data/")
	if !ok || !allowedKey(key) {
		http.Error(w, `{"errors":["unexpected path"]}`, http.StatusBadRequest)
		return
	}
	if key == f.denied {
		http.Error(w, `{"errors":["read refused"]}`, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		data, exists := f.keys[key]
		if !exists {
			http.Error(w, `{"errors":[]}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"data": data, "metadata": map[string]any{"version": 1},
		}})
		return
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(key, "infrastructure/email/") {
		http.Error(w, `{"errors":["unexpected mutation"]}`, http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Options struct {
			CAS *int `json:"cas"`
		} `json:"options"`
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Options.CAS == nil || *request.Options.CAS != 0 {
		http.Error(w, `{"errors":["creation must be conditional"]}`, http.StatusBadRequest)
		return
	}
	if f.race != "" {
		data := request.Data
		if f.race == "different" {
			data = map[string]any{"password": strings.Repeat("Z", 48)}
		}
		f.keys[key] = data
		f.race = ""
		http.Error(w, `{"errors":["check-and-set conflict"]}`, http.StatusBadRequest)
		return
	}
	if _, exists := f.keys[key]; exists {
		http.Error(w, `{"errors":["check-and-set conflict"]}`, http.StatusBadRequest)
		return
	}
	f.keys[key] = request.Data
	f.writes++
	if f.loseReply {
		f.loseReply = false
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
		return
	}
	if f.changeAfter {
		f.keys["apps/private-mail/admin"] = map[string]any{"password": strings.Repeat("B", 48)}
		f.changeAfter = false
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": 1}})
}

// allowedKey rejects fixture requests outside the six declared migration paths.
func allowedKey(key string) bool {
	for _, prefix := range []string{"apps/private-mail/", "infrastructure/email/"} {
		for _, account := range []string{"admin", "maintainer", "agent"} {
			if key == prefix+account {
				return true
			}
		}
	}
	return false
}

// repositoryRoot binds fixtures to this test's checkout rather than the caller's directory.
func repositoryRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

// runMigration executes the canonical program against an isolated synthetic API.
func runMigration(t *testing.T, fixture *apiFixture, mode string) (string, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	return runMigrationAt(t, server.URL, mode)
}

// runMigrationAt confines inputs and checks output privacy and scratch cleanup.
func runMigrationAt(t *testing.T, serverURL, mode string) (string, error) {
	return runMigrationAtExpected(t, serverURL, mode, credentials())
}

func runMigrationAtExpected(t *testing.T, serverURL, mode string, expected any) (string, error) {
	t.Helper()
	script, err := os.ReadFile(filepath.Join(repositoryRoot(), "scripts/prepare-email-credentials.sh"))
	if err != nil {
		t.Fatal("missing canonical Email credential preparation", err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"prepare.sh": string(script), "token": syntheticToken} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if expected != nil {
		data, marshalErr := json.Marshal(expected)
		if marshalErr != nil || os.WriteFile(filepath.Join(dir, "expected.json"), data, 0600) != nil {
			t.Fatal("could not prepare synthetic recovered records")
		}
	}
	environment := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LC_ALL=C",
		"BAO_ADDR=" + serverURL, "EMAIL_CUSTODY_MODE=" + mode,
		"EMAIL_CUSTODY_TOKEN_FILE=" + filepath.Join(dir, "token"),
		"EMAIL_CUSTODY_WORK_DIR=" + dir,
		"EMAIL_CUSTODY_EXPECTED_FILE=" + filepath.Join(dir, "expected.json"),
	}
	command := exec.Command("bash", "-eu", filepath.Join(dir, "prepare.sh"))
	command.Env = environment
	out, runErr := command.CombinedOutput()
	if strings.Contains(string(out), syntheticToken) || strings.Contains(string(out), strings.Repeat("A", 48)) || strings.Contains(string(out), serverURL) {
		t.Fatal("credential preparation emitted private material")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := 2
	if expected != nil {
		wantEntries++
	}
	if len(entries) != wantEntries {
		t.Fatal("credential scratch material survived the operation")
	}
	return string(out), runErr
}

// A rotation after recovery must refuse every write, including when only the
// last account differs. The copied set must be the one actually restored.
func TestCopyBindsEverySourceToRecoveredRecordsBeforeWriting(t *testing.T) {
	for _, test := range []string{"matching", "rotated-admin", "rotated-maintainer", "rotated-agent", "missing-input", "missing-record", "extra-record", "changed-extra-field"} {
		t.Run(test, func(t *testing.T) {
			fixture := &apiFixture{keys: credentials()}
			expected := credentials()
			switch test {
			case "rotated-admin", "rotated-maintainer", "rotated-agent":
				account := strings.TrimPrefix(test, "rotated-")
				fixture.keys["apps/private-mail/"+account]["password"] = strings.Repeat("Z", 48)
			case "missing-input":
				expected = nil
			case "missing-record":
				delete(expected, "apps/private-mail/agent")
			case "extra-record":
				expected["apps/private-mail/other"] = map[string]any{"password": strings.Repeat("Z", 48)}
			case "changed-extra-field":
				fixture.keys["apps/private-mail/agent"]["recovery"] = map[string]any{"preserve": false}
			}
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			var input any = expected
			if expected == nil {
				input = nil
			}
			out, err := runMigrationAtExpected(t, server.URL, "copy", input)
			if test == "matching" {
				if err != nil || fixture.writes != 3 || !strings.Contains(out, "=VERIFIED") {
					t.Fatalf("matching recoverable records were refused: %v %s", err, out)
				}
			} else if err == nil || fixture.writes != 0 || !strings.Contains(out, "=FAIL") {
				t.Fatalf("copy accepted values outside the restored set or wrote before complete comparison: writes=%d %v %s", fixture.writes, err, out)
			}
		})
	}
}

// TestPlanIsReadOnlyAndCopyPreservesEveryCurrentField verifies copying and idempotent replay.
func TestPlanIsReadOnlyAndCopyPreservesEveryCurrentField(t *testing.T) {
	fixture := &apiFixture{keys: credentials()}
	out, err := runMigration(t, fixture, "plan")
	if err != nil || !strings.Contains(out, "=PLANNED mode=plan existing=0 copied=0 pending=3 unseeded=0") || fixture.writes != 0 {
		t.Fatalf("plan must report three pending copies without writing: %v %s", err, out)
	}
	for attempt := 0; attempt < 2; attempt++ {
		out, err = runMigration(t, fixture, "copy")
		if err != nil || fixture.writes != 3 {
			t.Fatalf("copy and replay must create exactly three records: %v %s", err, out)
		}
		for _, account := range []string{"admin", "maintainer", "agent"} {
			if !reflect.DeepEqual(fixture.keys["apps/private-mail/"+account], fixture.keys["infrastructure/email/"+account]) {
				t.Fatal("copy changed the current credential or discarded another field")
			}
		}
	}
	if !strings.Contains(out, "=VERIFIED mode=copy existing=3 copied=0 pending=0 unseeded=0") {
		t.Fatal("replay must distinguish existing verified values from new copies")
	}
}

// TestMigrationRefusesConflictsAndUnreadableState requires a deliberate refusal.
func TestMigrationRefusesConflictsAndUnreadableState(t *testing.T) {
	for _, name := range []string{"destination-conflict", "read-refused", "invalid-password", "numeric-field", "invalid-mount", "different-race", "source-rotation", "invalid-mode"} {
		t.Run(name, func(t *testing.T) {
			fixture := &apiFixture{keys: credentials()}
			mode := "copy"
			switch name {
			case "destination-conflict":
				fixture.keys["infrastructure/email/admin"] = map[string]any{"password": strings.Repeat("Z", 48)}
			case "read-refused":
				fixture.denied = "apps/private-mail/admin"
			case "invalid-password":
				fixture.keys["apps/private-mail/admin"]["password"] = "invalid"
			case "numeric-field":
				fixture.keys["apps/private-mail/admin"]["extra"] = json.Number("9007199254740993")
			case "invalid-mount":
				fixture.invalidKV = true
			case "different-race":
				fixture.race = "different"
			case "source-rotation":
				fixture.changeAfter = true
			case "invalid-mode":
				mode = "replace"
			}
			out, err := runMigration(t, fixture, mode)
			if err == nil || !strings.Contains(out, "EMAIL_CUSTODY_PREPARATION=FAIL") {
				t.Fatalf("uncertain state must fail closed: %s", out)
			}
			if name != "source-rotation" && fixture.writes != 0 {
				t.Fatal("refusal wrote a credential")
			}
		})
	}
}

// TestConcurrentIdenticalCreationIsVerifiedWithoutOverwrite preserves another writer's value.
func TestConcurrentIdenticalCreationIsVerifiedWithoutOverwrite(t *testing.T) {
	fixture := &apiFixture{keys: credentials(), race: "same"}
	out, err := runMigration(t, fixture, "copy")
	if err != nil || fixture.writes != 2 || !strings.Contains(out, "existing=1 copied=2") {
		t.Fatalf("identical conditional-create race must be read back and compared: %v %s", err, out)
	}
}

// TestLostWriteReplyIsRecoveredByReadbackWithoutRotation recovers an uncertain commit.
func TestLostWriteReplyIsRecoveredByReadbackWithoutRotation(t *testing.T) {
	fixture := &apiFixture{keys: credentials(), loseReply: true}
	if _, err := runMigration(t, fixture, "copy"); err == nil || fixture.writes != 1 {
		t.Fatal("a committed write with a lost reply must remain an uncertain result")
	}
	out, err := runMigration(t, fixture, "copy")
	if err != nil || fixture.writes != 3 || !strings.Contains(out, "=VERIFIED mode=copy existing=1 copied=2") {
		t.Fatalf("retry must verify the committed value and copy only missing records: %v %s", err, out)
	}
	for _, account := range []string{"admin", "maintainer", "agent"} {
		if !reflect.DeepEqual(fixture.keys["apps/private-mail/"+account], fixture.keys["infrastructure/email/"+account]) {
			t.Fatal("uncertain-write recovery changed or erased a credential")
		}
	}
}

// TestUnseededInstallationIsExplicitAndNeverCreatesCredentials prevents false migration proof.
func TestUnseededInstallationIsExplicitAndNeverCreatesCredentials(t *testing.T) {
	for _, missingMount := range []bool{false, true} {
		t.Run(fmt.Sprint(missingMount), func(t *testing.T) {
			fixture := &apiFixture{keys: map[string]map[string]any{}, missingKV: missingMount}
			out, err := runMigration(t, fixture, "plan")
			if err != nil || fixture.writes != 0 || !strings.Contains(out, "=UNSEEDED") || !strings.Contains(out, "unseeded=3") {
				t.Fatalf("fresh installation must report unseeded state: %v %s", err, out)
			}
		})
	}
}
