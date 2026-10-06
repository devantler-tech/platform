package bookmarks

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type document struct {
	manifest        map[string]any
	bookmarks       []any
	settings        map[string]any
	services        []any
	group, bookmark string
}

func repository(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve fixture repository")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func fixture(t *testing.T, root string) *document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "k8s/bases/apps/homepage/config-map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	d := &document{}
	if err := yaml.Unmarshal(data, &d.manifest); err != nil {
		t.Fatal(err)
	}
	values := d.manifest["data"].(map[string]any)
	for name, target := range map[string]any{"bookmarks.yaml": &d.bookmarks, "settings.yaml": &d.settings, "services.yaml": &d.services} {
		if err := yaml.Unmarshal([]byte(values[name].(string)), target); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.bookmarks) < 2 {
		t.Fatal("real fixture needs two bookmark groups")
	}
	for name := range d.bookmarks[0].(map[string]any) {
		d.group = name
	}
	entries := d.entries()
	if len(entries) == 0 {
		t.Fatal("real fixture has no bookmark")
	}
	for name := range entries[0].(map[string]any) {
		d.bookmark = name
	}
	return d
}

func (d *document) entries() []any { return d.bookmarks[0].(map[string]any)[d.group].([]any) }
func (d *document) fields() map[string]any {
	return d.entries()[0].(map[string]any)[d.bookmark].([]any)[0].(map[string]any)
}
func (d *document) renameGroup(name string) {
	group := d.bookmarks[0].(map[string]any)
	entries := group[d.group]
	delete(group, d.group)
	group[name] = entries
	d.group = name
}

func (d *document) write(t *testing.T) string {
	t.Helper()
	values := d.manifest["data"].(map[string]any)
	for name, value := range map[string]any{"bookmarks.yaml": d.bookmarks, "settings.yaml": d.settings, "services.yaml": d.services} {
		data, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		values[name] = string(data)
	}
	data, err := yaml.Marshal(d.manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config-map.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func execute(t *testing.T, validator, config, root string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", validator, config, root)
	// The validator must resolve paths independently of the caller's directory.
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("validator exceeded its offline observation deadline: %v", ctx.Err())
	}
	if err == nil {
		return 0, string(output)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("could not execute real validator: %v", err)
	}
	return exit.ExitCode(), string(output)
}

func TestRealConfigMapFromAnotherDirectory(t *testing.T) {
	root := repository(t)
	status, output := execute(t, filepath.Join(root, "scripts/validate-homepage-bookmarks.sh"), filepath.Join(root, "k8s/bases/apps/homepage/config-map.yaml"), filepath.Join(root, "k8s"))
	if status != 0 || !strings.Contains(output, "homepage bookmark(s)") {
		t.Fatalf("status=%d output=%s", status, output)
	}
}

func TestBookmarkContracts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*document)
		want   string
	}{
		{"round trip", func(_ *document) {}, ""},
		{"mdi icon", func(d *document) { d.fields()["icon"] = "mdi-console" }, ""},
		{"lowercase hex color", func(d *document) { d.fields()["icon"] = "si-github-#aabbcc" }, ""},
		{"split fields", func(d *document) {
			d.entries()[0].(map[string]any)[d.bookmark] = []any{map[string]any{"icon": "docker"}, map[string]any{"href": "https://example.com"}}
		}, ""},
		{"same name in different groups", func(d *document) {
			group := d.bookmarks[1].(map[string]any)
			for name, entries := range group {
				group[name] = append(entries.([]any), d.entries()[0])
			}
		}, ""},
		{"missing icon", func(d *document) { delete(d.fields(), "icon") }, "GROUP -> BOOKMARK: missing icon"},
		{"space in icon", func(d *document) { d.fields()["icon"] = "not a slug" }, "does not match <slug>[-#RRGGBB]"},
		{"short hex color", func(d *document) { d.fields()["icon"] = "si-github-#fff" }, "does not match <slug>[-#RRGGBB]"},
		{"missing href", func(d *document) { delete(d.fields(), "href") }, "GROUP -> BOOKMARK: missing href"},
		{"http href", func(d *document) { d.fields()["href"] = "http://example.com" }, "must be an https:// URL"},
		{"href without scheme", func(d *document) { d.fields()["href"] = "example.com" }, "must be an https:// URL"},
		{"duplicate name in group", func(d *document) {
			group := d.bookmarks[0].(map[string]any)
			group[d.group] = append(d.entries(), d.entries()[0])
		}, "GROUP -> BOOKMARK: duplicate bookmark name in this group"},
		{"service group collision", func(d *document) {
			d.renameGroup("Security")
			d.settings["layout"].(map[string]any)["Security"] = map[string]any{}
		}, "Security: bookmark group reuses a service group name"},
		{"unlisted group", func(d *document) { d.renameGroup("Unlisted Group") }, "Unlisted Group: bookmark group is not listed in settings.yaml layout"},
		{"two group names", func(d *document) { d.bookmarks[0].(map[string]any)["Extra"] = d.entries() }, "bookmark group item 1 must map exactly one group name"},
		{"two bookmark names", func(d *document) {
			d.entries()[0].(map[string]any)["Extra"] = []any{map[string]any{"href": "http://bad"}}
		}, "GROUP: every bookmark item must map exactly one bookmark name"},
		{"scalar fields", func(d *document) { d.entries()[0].(map[string]any)[d.bookmark] = []any{"docker"} }, "GROUP -> BOOKMARK: missing icon"},
		{"empty bookmarks", func(d *document) { d.bookmarks = []any{} }, "no bookmark groups parsed"},
		{"group without bookmarks", func(d *document) { d.bookmarks[0].(map[string]any)[d.group] = []any{} }, "GROUP: group has no bookmarks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := repository(t)
			d := fixture(t, root)
			tc.change(d)
			status, output := execute(t, filepath.Join(root, "scripts/validate-homepage-bookmarks.sh"), d.write(t), filepath.Join(root, "k8s"))
			if tc.want == "" {
				if status != 0 || !strings.Contains(output, "homepage bookmark(s)") {
					t.Fatalf("status=%d output=%s", status, output)
				}
				return
			}
			want := strings.ReplaceAll(strings.ReplaceAll(tc.want, "GROUP", d.group), "BOOKMARK", d.bookmark)
			if status != 1 || !strings.Contains(output, want) {
				t.Fatalf("wanted exit 1 and %q; status=%d output=%s", want, status, output)
			}
		})
	}
}

func TestServiceCoverageCannotPassVacuously(t *testing.T) {
	root := repository(t)
	d := fixture(t, root)
	d.services = []any{}
	status, output := execute(t, filepath.Join(root, "scripts/validate-homepage-bookmarks.sh"), d.write(t), t.TempDir())
	if status != 1 || !strings.Contains(output, "no service groups found") {
		t.Fatalf("status=%d output=%s", status, output)
	}
}

func TestValidatorDoesNotInvokeExternalYAMLTools(t *testing.T) {
	root := repository(t)
	tools := t.TempDir()
	for _, name := range []string{"yq", "jq"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\necho external-parser-invoked >&2\nexit 88\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	status, output := execute(t, filepath.Join(root, "scripts/validate-homepage-bookmarks.sh"), filepath.Join(root, "k8s/bases/apps/homepage/config-map.yaml"), filepath.Join(root, "k8s"))
	if status != 0 || strings.Contains(output, "external-parser-invoked") {
		t.Fatalf("native validator must parse its inputs itself: status=%d output=%s", status, output)
	}
}

func TestUnreadableInputIsUnknown(t *testing.T) {
	root := repository(t)
	status, output := execute(t, filepath.Join(root, "scripts/validate-homepage-bookmarks.sh"), filepath.Join(t.TempDir(), "missing.yaml"), filepath.Join(root, "k8s"))
	if status != 2 || !strings.Contains(output, "missing or unreadable:") || strings.Contains(output, " valid.") {
		t.Fatalf("missing input must not produce a clean verdict: status=%d output=%s", status, output)
	}
}
