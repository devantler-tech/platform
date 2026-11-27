package preparation_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestMailV2CopyUsesItsOwnCompletionMarker(t *testing.T) {
	const oldMarker = ".private-mail-storage-migrated"
	const progress = ".private-mail-v2-copy-in-progress"
	const complete = ".private-mail-v2-copy-complete"
	const fence = ".private-mail-v1-fenced"
	const protocol = "private-mail-longhorn-v2"
	production := render(t, "k8s/providers/hetzner/apps")
	pod := mapping(t, mapping(t, mapping(t, object(t, production, "StatefulSet", "private-mail")["spec"])["template"])["spec"])
	copyInit := mapping(t, sequence(t, pod["initContainers"])[0])
	mounts := map[string]map[string]any{}
	for _, item := range sequence(t, copyInit["volumeMounts"]) {
		mount := mapping(t, item)
		mounts[stringField(t, mount, "name")] = mount
	}
	if copyInit["name"] != "migrate-storage" ||
		mounts["data"]["mountPath"] != "/var/lib/private-mail-original" || mounts["data"]["readOnly"] != true ||
		mounts["v2-data"]["mountPath"] != "/var/lib/stalwart" || mounts["v2-data"]["readOnly"] == true {
		t.Fatal("the cutover initializer must read the V1 mailbox and write only the V2 store")
	}
	script := sequence(t, copyInit["args"])[0].(string)
	if strings.Contains(script, "${") {
		t.Fatal("the copy script is literal content and must survive Flux's strict post-build substitution")
	}
	for _, tc := range []struct {
		name, state string
		sourceFence bool
		success     bool
	}{
		{name: "first copy retains the old marker", success: true},
		{name: "older marker alone cannot authorize unknown target data", state: oldMarker},
		{name: "interrupted owned copy can re-create its target", state: progress, success: true},
		{name: "new marker from the old volume is refused", state: "source contains a V2 completion marker"},
		{name: "fenced V1 cannot overwrite V2 after loss of receipt", state: progress, sourceFence: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
			for _, dir := range []string{filepath.Join(source, "data"), filepath.Join(target, "lost+found"), filepath.Join(root, "scratch")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			// Longhorn supplies an empty filesystem with lost+found. Existing
			// application data is present only in the partial/unknown cases.
			writeCopyFixture(t, filepath.Join(source, "data", "mail"), "original mailbox")
			writeCopyFixture(t, filepath.Join(source, oldMarker), "original V1 receipt")
			if tc.sourceFence {
				writeCopyFixture(t, filepath.Join(source, fence), protocol+" "+strings.Repeat("a", 64)+"\n")
			}
			if tc.state == "source contains a V2 completion marker" {
				writeCopyFixture(t, filepath.Join(source, complete), protocol+" fabricated")
			} else if tc.state != "" {
				content := "original V1 receipt"
				if tc.state == progress {
					content = protocol
				}
				writeCopyFixture(t, filepath.Join(target, tc.state), content+"\n")
				writeCopyFixture(t, filepath.Join(target, "unverified"), "partial target")
			}
			run := func() error {
				return runMailCopyFixture(t, script, root, source, target)
			}
			if err := run(); (err == nil) != tc.success {
				t.Fatalf("copy success = %t, want %t (%v)", err == nil, tc.success, err)
			}
			if !tc.success {
				if tc.state == oldMarker || tc.sourceFence {
					assertCopyFixture(t, filepath.Join(target, "unverified"), "partial target")
				}
				return
			}
			assertCopyFixture(t, filepath.Join(source, "data", "mail"), "original mailbox")
			assertCopyFixture(t, filepath.Join(target, "data", "mail"), "original mailbox")
			assertCopyFixture(t, filepath.Join(target, oldMarker), "original V1 receipt")
			receipt, err := os.ReadFile(filepath.Join(target, complete))
			if err != nil || !regexp.MustCompile("^"+protocol+" [0-9a-f]{64}\n$").Match(receipt) {
				t.Fatal("the verified copy did not write a complete V2 receipt")
			}
			if _, err := os.Stat(filepath.Join(target, progress)); !os.IsNotExist(err) {
				t.Fatal("the completed copy retained permission to wipe scratch")
			}
			writeCopyFixture(t, filepath.Join(source, fence), string(receipt))
			writeCopyFixture(t, filepath.Join(target, "data", "mail"), "new mail served on V2")
			if err := run(); err != nil {
				t.Fatal(err)
			}
			assertCopyFixture(t, filepath.Join(target, "data", "mail"), "new mail served on V2")
			writeCopyFixture(t, filepath.Join(source, fence), protocol+" "+strings.Repeat("f", 64)+"\n")
			if err := run(); err == nil {
				t.Fatal("a conflicting V1 fence must hold before serving V2")
			}
			writeCopyFixture(t, filepath.Join(source, fence), string(receipt))
			if err := os.Remove(filepath.Join(target, complete)); err != nil {
				t.Fatal(err)
			}
			if err := run(); err == nil {
				t.Fatal("lost completion evidence must hold the serving volume")
			}
			assertCopyFixture(t, filepath.Join(target, "data", "mail"), "new mail served on V2")
		})
	}
}

func runMailCopyFixture(t *testing.T, script, root, source, target string) error {
	t.Helper()
	body := strings.NewReplacer("/var/lib/private-mail-original", source, "/var/lib/stalwart", target, "/tmp/", filepath.Join(root, "scratch")+"/").Replace(script)
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", body)
	if runtime.GOOS == "darwin" {
		bin := filepath.Join(root, "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"find", "cp", "sort", "xargs", "sha256sum"} {
			gnu, err := exec.LookPath("g" + name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(gnu, filepath.Join(bin, name)); err != nil && !os.IsExist(err) {
				t.Fatal(err)
			}
		}
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("copy output: %s", out)
	}
	return err
}

func writeCopyFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertCopyFixture(t *testing.T, path, expected string) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != expected {
		t.Fatalf("fixture %q differed: %s (%v)", filepath.Base(path), actual, err)
	}
}
