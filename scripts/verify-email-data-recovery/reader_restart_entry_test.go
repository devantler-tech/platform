package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// readerRestartDispatch supplies only the separately confirmed original-reader preparation.
func readerRestartDispatch() map[string]string {
	env := readerCustodyDispatch()
	delete(env, "EMAIL_READER_INPUTS_CONFIRM")
	env["GITHUB_WORKFLOW_REF"] = "devantler-tech/platform/.github/workflows/prepare-retained-email-reader-witness.yaml@refs/heads/main"
	env["EMAIL_READER_WITNESS_CONFIRM"] = "prepare-retained-email-reader-witness"
	env["EMAIL_RETAINED_READER_UID"] = "11111111-1111-2222-3333-444444444444"
	env["EMAIL_RETAINED_CLAIM_UID"] = "22222222-1111-2222-3333-444444444444"
	env["EMAIL_RETAINED_PV_UID"] = "33333333-1111-2222-3333-444444444444"
	env["EMAIL_RETAINED_BACKEND_UID"] = "44444444-1111-2222-3333-444444444444"
	env["EMAIL_RETAINED_REPLICA_UIDS"] = `["55555555-1111-2222-3333-444444444444","66666666-1111-2222-3333-444444444444","77777777-1111-2222-3333-444444444444"]`
	return env
}

// TestReaderRestartCommandsUseTheirOwnDispatch executes both entrypoints without native access.
func TestReaderRestartCommandsUseTheirOwnDispatch(t *testing.T) {
	if flag := os.Getenv("EMAIL_RESTART_WITNESS_CHILD"); flag != "" {
		os.Args = []string{"verify-email-data-recovery", flag}
		main()
		return
	}
	for _, flag := range []string{"--inspect-reader-witness", "--prepare-reader-witness"} {
		for _, confirmed := range []bool{false, true} {
			command := exec.Command(os.Args[0], "-test.run=^TestReaderRestartCommandsUseTheirOwnDispatch$")
			command.Env = []string{"EMAIL_RESTART_WITNESS_CHILD=" + flag, "PATH=/nonexistent", "GOCOVERDIR=" + t.TempDir()}
			stage := "dispatch"
			if confirmed {
				for key, value := range readerRestartDispatch() {
					command.Env = append(command.Env, key+"="+value)
				}
				stage = "source"
			}
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(output), "EMAIL_READER_RESTART_WITNESS=UNVERIFIED stage="+stage+" restart=NOT_AUTHORIZED recovery=UNVERIFIED") || strings.Contains(string(output), "=STORED") {
				t.Fatalf("restart preparation lost its own dispatch or certified incomplete evidence: %q", output)
			}
		}
	}
}
