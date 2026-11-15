package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

const sampleLog = "2026-11-15T09:58:45.1Z PROD_RECOVERY_SOURCE=PASS\n" +
	"2026-11-15T09:58:50.1Z PROD_OPERATION_EXIT=3\n" +
	"2026-11-15T09:58:50.2Z ----- BEGIN PROD OPERATION OUTPUT -----\n" +
	"2026-11-15T09:58:50.2Z -----BEGIN AGE ENCRYPTED FILE-----\n" +
	"2026-11-15T09:58:50.2Z YWdlLWVuY3J5cHRpb24=\n" +
	"2026-11-15T09:58:50.2Z -----END AGE ENCRYPTED FILE-----\n" +
	"2026-11-15T09:58:50.3Z ----- END PROD OPERATION OUTPUT -----\n" +
	"2026-11-15T09:58:51.0Z Post job cleanup.\n"

func TestExtractStripsTimestampsAndKeepsTheExitStatus(t *testing.T) {
	blocks, err := extract(sampleLog)
	if err != nil {
		t.Fatal(err)
	}
	want := "-----BEGIN AGE ENCRYPTED FILE-----\nYWdlLWVuY3J5cHRpb24=\n-----END AGE ENCRYPTED FILE-----\n"
	if len(blocks) != 1 || blocks[0].exit != "3" || blocks[0].ciphertext != want {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtractRefusesAnUnterminatedBlock(t *testing.T) {
	if _, err := extract(strings.Split(sampleLog, "----- END")[0]); err == nil {
		t.Fatal("unterminated block accepted")
	}
}

const goodMeta = ".github/workflows/observe-runtime-health.yaml\tmain\tworkflow_dispatch\tdevantler\tdevantler\tdevantler-tech/platform\t95275df0eaa65ed5b646befe69b9f4a95338d1b9"

type fake struct {
	meta, caller, log, plain string
	decryptErr               error
	calls                    []string
}

func (f *fake) run(_ context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch {
	case name == "gh" && strings.HasSuffix(args[1], "/jobs"):
		return []byte("111\n"), nil
	case name == "gh" && strings.Contains(strings.Join(args, " "), "/contents/"):
		return []byte(f.caller), nil
	case name == "gh" && args[1] != "--allow-escape-sequences" && strings.Contains(args[1], "/actions/runs/"):
		return []byte(f.meta + "\n"), nil
	case name == "gh":
		return []byte(f.log), nil
	case name == "age":
		if f.decryptErr != nil {
			return nil, f.decryptErr
		}
		_, _ = io.ReadAll(stdin)
		return []byte(f.plain), nil
	}
	return nil, errors.New("unexpected command")
}

func newFake() *fake {
	return &fake{meta: goodMeta, caller: "jobs:\n  observe:\n    uses: ./.github/workflows/prod-operation.yaml # zizmor: ignore\n", log: sampleLog, plain: "facts\n"}
}

func TestReadDecryptsTheVerifiedRunsBlocks(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	if err := read(context.Background(), "43699809761", "/id.txt", f.run, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "== job 111, exit 3\nfacts\n" {
		t.Fatalf("got %q", out.String())
	}
	want := []string{
		"gh api repos/devantler-tech/platform/actions/runs/43699809761 --jq",
		"gh api -H Accept: application/vnd.github.raw repos/devantler-tech/platform/contents/.github/workflows/observe-runtime-health.yaml?ref=95275df0eaa65ed5b646befe69b9f4a95338d1b9",
		"gh api repos/devantler-tech/platform/actions/runs/43699809761/jobs --jq .jobs[] | select(.name | contains(\" / \")) | .id",
		"gh api --allow-escape-sequences repos/devantler-tech/platform/actions/jobs/111/logs",
		"age --decrypt --identity /id.txt",
	}
	if len(f.calls) != len(want) {
		t.Fatalf("calls %q", f.calls)
	}
	for i := range want {
		if !strings.HasPrefix(f.calls[i], want[i]) {
			t.Fatalf("call %d = %q, want prefix %q", i, f.calls[i], want[i])
		}
	}
}

func TestReadRefusesForgedOrBrokenRuns(t *testing.T) {
	field := func(i int, v string) string {
		parts := strings.Split(goodMeta, "\t")
		parts[i] = v
		return strings.Join(parts, "\t")
	}
	for name, mutate := range map[string]func(*fake){
		"another workflow path": func(f *fake) { f.meta = field(0, "../../evil.yaml") },
		"another branch":        func(f *fake) { f.meta = field(1, "feature") },
		"a pull request run":    func(f *fake) { f.meta = field(2, "pull_request") },
		"another actor":         func(f *fake) { f.meta = field(3, "someone") },
		"another trigger":       func(f *fake) { f.meta = field(4, "someone") },
		"a fork":                func(f *fake) { f.meta = field(5, "fork/platform") },
		"not a prod caller":     func(f *fake) { f.caller = "jobs: {}\n" },
		"no encrypted block":    func(f *fake) { f.log = "PROD_OPERATION=UNVERIFIED\n" },
		"non-numeric exit":      func(f *fake) { f.log = strings.Replace(sampleLog, "EXIT=3", "EXIT=\x1b[31m0", 1) },
		"decryption refused":    func(f *fake) { f.decryptErr = errors.New("no identity matched") },
		"unterminated block":    func(f *fake) { f.log = strings.Split(sampleLog, "----- END")[0] },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			mutate(f)
			var out bytes.Buffer
			if err := read(context.Background(), "1", "/id.txt", f.run, &out); err == nil || out.Len() != 0 {
				t.Fatalf("want an error and no output, got %v %q", err, out.String())
			}
		})
	}
	t.Run("non-numeric run id", func(t *testing.T) {
		var out bytes.Buffer
		if err := read(context.Background(), "1;rm", "/id.txt", newFake().run, &out); err == nil {
			t.Fatal("accepted")
		}
	})
}

func TestReadStripsTerminalControlCharacters(t *testing.T) {
	f := newFake()
	f.plain = "ok\x1b[2J\tline\r\x07\n"
	var out bytes.Buffer
	if err := read(context.Background(), "1", "/id.txt", f.run, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "== job 111, exit 3\nok[2J\tline\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestIdentityFileOrder(t *testing.T) {
	env := map[string]string{"HOME": "/home/me"}
	get := func(k string) string { return env[k] }
	if got := identityFile(get); got != "/home/me/.config/sops/age/keys.txt" {
		t.Fatalf("default %q", got)
	}
	env["SOPS_AGE_KEY_FILE"] = "/sops.txt"
	if got := identityFile(get); got != "/sops.txt" {
		t.Fatalf("sops %q", got)
	}
	env["AGE_IDENTITY_FILE"] = "/age.txt"
	if got := identityFile(get); got != "/age.txt" {
		t.Fatalf("age %q", got)
	}
}
