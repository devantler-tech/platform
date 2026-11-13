package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureList is a small list with every entry shape the real one uses.
const fixtureList = `# what production is built from
k8s/
ksail.prod.yaml

# the deploy
.github/workflows/
scripts/production-inputs/
scripts/deploy.sh
`

// repo is a throwaway git repository.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	fixture := &repo{t: t, dir: t.TempDir()}
	fixture.git("init", "--quiet", "--initial-branch=main")
	return fixture
}

func (fixture *repo) git(arguments ...string) string {
	fixture.t.Helper()
	command := exec.Command("git", append([]string{"-C", fixture.dir,
		"-c", "user.name=test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		fixture.t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func (fixture *repo) write(file, text string) {
	fixture.t.Helper()
	target := filepath.Join(fixture.dir, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fixture.t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *repo) remove(file string) {
	fixture.t.Helper()
	if err := os.Remove(filepath.Join(fixture.dir, filepath.FromSlash(file))); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *repo) commit(message string) string {
	fixture.t.Helper()
	fixture.git("add", "--all")
	fixture.git("commit", "--quiet", "--allow-empty", "-m", message)
	return fixture.git("rev-parse", "HEAD")
}

func (fixture *repo) exists(file string) bool {
	_, err := os.Lstat(filepath.Join(fixture.dir, filepath.FromSlash(file)))
	return err == nil
}

// baseline commits one file of every kind the decision has to tell apart.
func baseline(t *testing.T) (*repo, string) {
	t.Helper()
	fixture := newRepo(t)
	fixture.write(listPath, fixtureList)
	fixture.write("k8s/apps/app.yaml", "kind: Deployment\n")
	fixture.write("ksail.prod.yaml", "kind: Cluster\n")
	fixture.write(".github/workflows/ci.yaml", "name: CI\n")
	fixture.write("scripts/deploy.sh", "echo deploy\n")
	fixture.write("scripts/deploy.sh.md", "notes\n")
	fixture.write("scripts/lint.sh", "echo lint\n")
	fixture.write("scripts/tests/test-deploy.sh", "echo test\n")
	fixture.write("docs/k8s/guide.md", "guide\n")
	fixture.write("ksail.yaml", "kind: Cluster\n")
	return fixture, fixture.commit("base")
}

func TestParseInputsAcceptsPlainPaths(t *testing.T) {
	list, err := parseInputs(fixtureList)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]bool{
		"k8s/apps/app.yaml":                 true,
		"k8s":                               false,
		"docs/k8s/guide.md":                 false,
		"ksail.prod.yaml":                   true,
		"ksail.prod.yaml.bak":               false,
		"ksail.yaml":                        false,
		"scripts/deploy.sh":                 true,
		"scripts/deploy.sh.md":              false,
		"scripts/lint.sh":                   false,
		"scripts/production-inputs/main.go": true,
		".github/workflows/ci.yaml":         true,
		".github/renovate.json":             false,
	} {
		if got := list.covers(file); got != want {
			t.Errorf("covers(%q) = %t, want %t", file, got, want)
		}
	}
}

func TestParseInputsRefusesAnythingButPlainPaths(t *testing.T) {
	for name, text := range map[string]string{
		"empty":           "# nothing\n\n",
		"wildcard":        "k8s/*.yaml\n",
		"negation":        "!k8s/\n",
		"absolute":        "/k8s/\n",
		"parent":          "../k8s/\n",
		"inner parent":    "k8s/../scripts/\n",
		"current":         "./k8s/\n",
		"double slash":    "k8s//apps/\n",
		"space":           "k8s/my app/\n",
		"leading space":   " k8s/\n",
		"trailing space":  "k8s/ \n",
		"duplicate":       "k8s/\nk8s/\n",
		"quoted":          "\"k8s/\"\n",
		"character class": "k8s/[ab]/\n",
		"inline comment":  "k8s/ # manifests\n",
	} {
		if _, err := parseInputs(text); err == nil {
			t.Errorf("%s: parseInputs accepted %q", name, text)
		}
	}
}

func TestDecideLeavesProductionAloneForOtherFiles(t *testing.T) {
	fixture, base := baseline(t)
	fixture.write("scripts/tests/test-deploy.sh", "echo changed test\n")
	fixture.write("scripts/lint.sh", "echo changed lint\n")
	fixture.write("scripts/deploy.sh.md", "changed notes\n")
	fixture.write("docs/k8s/guide.md", "changed guide\n")
	fixture.write("ksail.yaml", "kind: Cluster # local\n")
	fixture.write("README.md", "new\n")
	head := fixture.commit("tests, docs and a local file")

	result := decide(fixture.dir, base, head)
	if result.production {
		t.Fatalf("a change outside the list was called a production change: %s %v", result.reason, result.paths)
	}
	if len(result.paths) != 6 {
		t.Fatalf("the decision examined %d files, want 6: %v", len(result.paths), result.paths)
	}
}

func TestDecideDeploysForEveryProductionInput(t *testing.T) {
	for name, change := range map[string]func(*repo){
		"a manifest":                 func(fixture *repo) { fixture.write("k8s/apps/app.yaml", "kind: StatefulSet\n") },
		"a new manifest":             func(fixture *repo) { fixture.write("k8s/apps/new.yaml", "kind: Service\n") },
		"a removed manifest":         func(fixture *repo) { fixture.remove("k8s/apps/app.yaml") },
		"the cluster config":         func(fixture *repo) { fixture.write("ksail.prod.yaml", "kind: Cluster # changed\n") },
		"a workflow":                 func(fixture *repo) { fixture.write(".github/workflows/ci.yaml", "name: CI 2\n") },
		"a deploy script":            func(fixture *repo) { fixture.write("scripts/deploy.sh", "echo changed\n") },
		"the classifier":             func(fixture *repo) { fixture.write("scripts/production-inputs/main.go", "package main\n") },
		"a comment in the list":      func(fixture *repo) { fixture.write(listPath, fixtureList+"# note\n") },
		"a file moved out of a list": func(fixture *repo) { fixture.git("mv", "k8s/apps/app.yaml", "docs/app.yaml") },
		"a file moved into a list":   func(fixture *repo) { fixture.git("mv", "scripts/lint.sh", "k8s/lint.sh") },
		"a mode change": func(fixture *repo) {
			if err := os.Chmod(filepath.Join(fixture.dir, "scripts", "deploy.sh"), 0o755); err != nil {
				fixture.t.Fatal(err)
			}
		},
		// The change takes a script off the list and alters it in one step.
		"a file dropped from the list and changed": func(fixture *repo) {
			fixture.write(listPath, strings.Replace(fixtureList, "scripts/deploy.sh\n", "", 1))
			fixture.write("scripts/deploy.sh", "echo no longer listed\n")
		},
		"a production input beside other files": func(fixture *repo) {
			fixture.write("scripts/tests/test-deploy.sh", "echo changed test\n")
			fixture.write("k8s/apps/app.yaml", "kind: DaemonSet\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture, base := baseline(t)
			change(fixture)
			head := fixture.commit(name)
			if result := decide(fixture.dir, base, head); !result.production || len(result.paths) == 0 {
				t.Fatalf("%s was not called a production change: %s", name, result.reason)
			}
		})
	}
}

func TestDecideDeploysWheneverItCannotCompare(t *testing.T) {
	fixture, base := baseline(t)
	fixture.write("docs/k8s/guide.md", "changed guide\n")
	head := fixture.commit("docs only")
	if decide(fixture.dir, base, head).production {
		t.Fatal("the control case must not be a production change")
	}

	unknown := strings.Repeat("0", 39) + "1"
	for name, pair := range map[string][2]string{
		"an empty base":         {"", head},
		"an empty head":         {base, ""},
		"an abbreviated base":   {base[:12], head},
		"a branch name":         {"main", head},
		"an option as a commit": {"--output=/tmp/x", head},
		"an unknown base":       {unknown, head},
		"an unknown head":       {base, unknown},
		"the same commit twice": {head, head},
	} {
		if result := decide(fixture.dir, pair[0], pair[1]); !result.production || result.reason == "" {
			t.Errorf("%s: answered %t with reason %q, want production with a reason", name, result.production, result.reason)
		}
	}
	if result := decide(filepath.Join(fixture.dir, "missing"), base, head); !result.production {
		t.Error("a repository that cannot be read must answer production")
	}

	// A tree object is not a commit.
	if result := decide(fixture.dir, fixture.git("rev-parse", base+"^{tree}"), head); !result.production {
		t.Error("a tree id in place of a commit must answer production")
	}
}

func TestDecideDeploysWhenEitherListCannotBeUsed(t *testing.T) {
	for name, text := range map[string]string{
		"a list with a wildcard": "k8s/*\n",
		"an empty list":          "# nothing\n",
	} {
		t.Run(name+" at the head", func(t *testing.T) {
			fixture, base := baseline(t)
			fixture.write(listPath, text)
			fixture.git("add", "--all")
			tree := fixture.git("write-tree")
			docsOnly := fixture.git("commit-tree", tree, "-p", base, "-m", "broken list")
			if result := decide(fixture.dir, base, docsOnly); !result.production || !strings.Contains(result.reason, "head") {
				t.Fatalf("answered %t (%s), want production because of the head list", result.production, result.reason)
			}
		})
		t.Run(name+" at the base", func(t *testing.T) {
			fixture := newRepo(t)
			fixture.write(listPath, text)
			fixture.write("docs/guide.md", "guide\n")
			base := fixture.commit("base with a broken list")
			fixture.write(listPath, fixtureList)
			head := fixture.commit("repair")
			// The repair itself is a listed path; change only a document on top
			// so the base list is the one thing that decides.
			fixture.write("docs/guide.md", "changed\n")
			next := fixture.commit("docs")
			if result := decide(fixture.dir, base, next); !result.production || !strings.Contains(result.reason, "base") {
				t.Fatalf("answered %t (%s), want production because of the base list", result.production, result.reason)
			}
			if decide(fixture.dir, head, next).production {
				t.Fatal("with both lists usable the same change is not a production change")
			}
		})
	}

	// The head's list covers nothing that changed, not even itself, so only the
	// missing base list can make this a production change. Reading a missing
	// list as an empty one would answer false.
	t.Run("no list at the base", func(t *testing.T) {
		fixture := newRepo(t)
		fixture.write("docs/guide.md", "guide\n")
		fixture.write("k8s/app.yaml", "kind: Deployment\n")
		base := fixture.commit("before the list existed")
		fixture.write("docs/guide.md", "changed\n")
		fixture.write(listPath, "k8s/\n")
		head := fixture.commit("docs and a first list")
		result := decide(fixture.dir, base, head)
		if !result.production || !strings.Contains(result.reason, "base") || !strings.Contains(result.reason, "has no "+listPath) {
			t.Fatalf("answered %t (%s), want production because the base has no list", result.production, result.reason)
		}
	})

	t.Run("no list at the head", func(t *testing.T) {
		fixture, base := baseline(t)
		fixture.remove(listPath)
		fixture.write("docs/k8s/guide.md", "changed\n")
		head := fixture.commit("drop the list")
		result := decide(fixture.dir, base, head)
		if !result.production || !strings.Contains(result.reason, "head") || !strings.Contains(result.reason, "has no "+listPath) {
			t.Fatalf("answered %t (%s), want production because the head has no list", result.production, result.reason)
		}
	})
}

func TestChangedReportsToTheWorkflow(t *testing.T) {
	fixture, base := baseline(t)
	fixture.write("docs/k8s/guide.md", "changed guide\n")
	docs := fixture.commit("docs only")
	fixture.write("k8s/apps/app.yaml", "kind: Job\n")
	manifest := fixture.commit("manifest")

	for head, want := range map[string]string{docs: "production=false", manifest: "production=true"} {
		output := filepath.Join(t.TempDir(), "output")
		summary := filepath.Join(t.TempDir(), "summary")
		t.Setenv("GITHUB_OUTPUT", output)
		t.Setenv("GITHUB_STEP_SUMMARY", summary)
		t.Chdir(fixture.dir)
		var stdout bytes.Buffer
		if code := run([]string{"changed", "--base", base, "--head", head}, &stdout); code != 0 {
			t.Fatalf("changed exited %d", code)
		}
		// The workflow step compares the whole of standard output with one line.
		if stdout.String() != want+"\n" {
			t.Fatalf("printed %q, want exactly %q", stdout.String(), want+"\n")
		}
		// Only that step writes the job output, once.
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("the command wrote the job output itself (%v)", err)
		}
		if text, err := os.ReadFile(summary); err != nil || !strings.Contains(string(text), want) {
			t.Fatalf("the job summary does not state %q: %q (%v)", want, text, err)
		}
	}

	// A summary that cannot be written prints no answer, so the step stays true.
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "missing", "summary"))
	var unwritable bytes.Buffer
	if code := run([]string{"changed", "--base", base, "--head", docs}, &unwritable); code == 0 || unwritable.Len() != 0 {
		t.Fatalf("an unwritable summary exited %d and printed %q, want a failure and no answer", code, unwritable.String())
	}

	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	var stdout bytes.Buffer
	if code := run([]string{"changed", "--base", base}, &stdout); code != 0 || strings.TrimSpace(stdout.String()) != "production=true" {
		t.Fatalf("a missing head exited %d and printed %q, want production=true", code, stdout.String())
	}
	if code := run([]string{"changed", "--base", base, "--head", docs, "extra"}, &stdout); code == 0 {
		t.Fatal("an unexpected argument must not exit 0")
	}
	if code := run([]string{"deploy"}, &stdout); code == 0 {
		t.Fatal("an unknown command must not exit 0")
	}
}

func TestRestrictKeepsOnlyTheList(t *testing.T) {
	fixture, _ := baseline(t)
	var stdout bytes.Buffer
	if err := restrict(fixture.dir, &stdout); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]bool{
		"k8s/apps/app.yaml":            true,
		"ksail.prod.yaml":              true,
		".github/workflows/ci.yaml":    true,
		"scripts/deploy.sh":            true,
		listPath:                       true,
		"scripts/deploy.sh.md":         false,
		"scripts/lint.sh":              false,
		"scripts/tests/test-deploy.sh": false,
		"docs/k8s/guide.md":            false,
		"ksail.yaml":                   false,
	} {
		if got := fixture.exists(file); got != want {
			t.Errorf("%s present = %t, want %t", file, got, want)
		}
	}
	if !strings.Contains(stdout.String(), "5 of 10 tracked files") {
		t.Errorf("unexpected report: %q", stdout.String())
	}
	if status := fixture.git("status", "--porcelain"); status != "" {
		t.Errorf("the restricted tree is not clean: %q", status)
	}

	// Running it again changes nothing, and history stays readable.
	if err := restrict(fixture.dir, &stdout); err != nil {
		t.Fatal(err)
	}
	if got := fixture.git("show", "HEAD:scripts/lint.sh"); got != "echo lint" {
		t.Errorf("history lost a file: %q", got)
	}
}

func TestRestrictRefusesAListItCannotHonour(t *testing.T) {
	t.Run("a stale entry", func(t *testing.T) {
		fixture := newRepo(t)
		fixture.write(listPath, fixtureList+"scripts/gone.sh\ntalos/\n")
		fixture.write("k8s/app.yaml", "x\n")
		fixture.write("ksail.prod.yaml", "x\n")
		fixture.write(".github/workflows/ci.yaml", "x\n")
		fixture.write("scripts/deploy.sh", "x\n")
		fixture.write("docs/guide.md", "x\n")
		fixture.commit("base")
		err := restrict(fixture.dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "scripts/gone.sh") || !strings.Contains(err.Error(), "talos/") {
			t.Fatalf("want both stale entries named, got %v", err)
		}
		if !fixture.exists("docs/guide.md") {
			t.Fatal("a refused list must leave the working tree alone")
		}
	})
	t.Run("an unusable list", func(t *testing.T) {
		fixture := newRepo(t)
		fixture.write(listPath, "k8s/*\n")
		fixture.write("k8s/app.yaml", "x\n")
		fixture.commit("base")
		if err := restrict(fixture.dir, &bytes.Buffer{}); err == nil {
			t.Fatal("a list with a pattern must be refused")
		}
	})
	t.Run("no list", func(t *testing.T) {
		fixture := newRepo(t)
		fixture.write("k8s/app.yaml", "x\n")
		fixture.commit("base")
		if err := restrict(fixture.dir, &bytes.Buffer{}); err == nil {
			t.Fatal("a missing list must be refused")
		}
	})
	t.Run("a listed file deleted from the working tree", func(t *testing.T) {
		fixture, _ := baseline(t)
		fixture.remove("ksail.prod.yaml")
		fixture.git("update-index", "--assume-unchanged", "ksail.prod.yaml")
		err := restrict(fixture.dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "ksail.prod.yaml") {
			t.Fatalf("want the missing production input named, got %v", err)
		}
	})
}

// A link under a listed path would let a file outside the list stand in for
// one inside it: the list would call a change to the target "not production"
// while a deploy from the whole checkout reads it through the link.
func TestRestrictRefusesLinksUnderListedPaths(t *testing.T) {
	t.Run("a symbolic link", func(t *testing.T) {
		fixture, _ := baseline(t)
		if err := os.Symlink("../../docs/k8s/guide.md", filepath.Join(fixture.dir, "k8s", "apps", "guide.yaml")); err != nil {
			t.Skipf("this filesystem cannot hold a symbolic link: %v", err)
		}
		fixture.commit("link a manifest to a document")
		err := restrict(fixture.dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "k8s/apps/guide.yaml") {
			t.Fatalf("want the link named, got %v", err)
		}
		if !fixture.exists("docs/k8s/guide.md") {
			t.Fatal("a refused tree must be left alone")
		}
	})
	t.Run("a submodule", func(t *testing.T) {
		fixture, base := baseline(t)
		fixture.git("update-index", "--add", "--cacheinfo", "160000,"+base+",k8s/vendored")
		fixture.git("commit", "--quiet", "-m", "pin a submodule under the manifests")
		err := restrict(fixture.dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "k8s/vendored") {
			t.Fatalf("want the submodule named, got %v", err)
		}
	})
	t.Run("a link outside the list is removed like any other file", func(t *testing.T) {
		fixture, _ := baseline(t)
		if err := os.Symlink("guide.md", filepath.Join(fixture.dir, "docs", "k8s", "latest.md")); err != nil {
			t.Skipf("this filesystem cannot hold a symbolic link: %v", err)
		}
		fixture.commit("link one document to another")
		if err := restrict(fixture.dir, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if fixture.exists("docs/k8s/latest.md") {
			t.Fatal("an unlisted link survived the restriction")
		}
	})
}
