package acrapplicability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inputs(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		Owner: "module " + ownerModule + "\n\ngo 1.26.10\nrequire " + Module + " " + Version + "\nreplace k8s.io/pod-security-admission => github.com/kyverno/pod-security-admission v0.0.0-20261015135348-40bb0fb141a9\n",
		strings.TrimSuffix(Owner, "go.mod") + "go.sum":       Module + " " + Version + " " + moduleSum + "\n" + Module + " " + Version + "/go.mod " + moduleModSum + "\n",
		strings.TrimSuffix(Owner, "go.mod") + "main.go":      "package main\nfunc main() {}\n",
		strings.TrimSuffix(Owner, "go.mod") + "main_test.go": "package main\nimport \"testing\"\nfunc TestInput(t *testing.T) {}\n",
		"go.mod": "module root.example\ngo 1.27.3\n",
	}
	var names []string
	for path, data := range files {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		names = append(names, path)
	}
	return root, names
}

func TestInventoryPinsTheOnlyOwnerAndBothSums(t *testing.T) {
	root, files := inputs(t)
	got, err := inspectInputs(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GoFiles) != 2 || len(got.Hashes) != 5 {
		t.Fatalf("incomplete source binding: %+v", got)
	}
}

func TestReceiptRefusesAFirstPartyVariantAddedAfterAnalysis(t *testing.T) {
	root, files := inputs(t)
	receipt, err := inspectInputs(root, files)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.Dir(Owner), "late_windows.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := receipt.verify(root); err == nil {
		t.Fatal("late first-party source escaped the source receipt")
	}
}

func TestInventoryRefusesScopeAndResolutionChanges(t *testing.T) {
	for _, kind := range []string{"another require", "another sum", "duplicate sum", "wrong version", "wrong checksum", "target replace", "new owner replace", "workspace", "vendor", "nested module", "untracked Go", "symlink", "missing input"} {
		t.Run(kind, func(t *testing.T) {
			root, files := inputs(t)
			if _, err := inspectInputs(root, files); err != nil {
				t.Fatal(err)
			}
			appendFile := func(path, data string) {
				t.Helper()
				p := filepath.Join(root, path)
				f, e := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = f.WriteString(data); e != nil {
					t.Fatal(e)
				}
				if e = f.Close(); e != nil {
					t.Fatal(e)
				}
			}
			switch kind {
			case "another require":
				appendFile("go.mod", "require "+Module+" "+Version+"\n")
			case "another sum":
				appendFile("go.sum", Module+" "+Version+" "+moduleSum+"\n")
				files = append(files, "go.sum")
			case "duplicate sum":
				p := filepath.Join(root, filepath.Dir(Owner), "go.sum")
				if e := os.WriteFile(p, []byte(strings.Repeat(Module+" "+Version+" "+moduleSum+"\n", 2)), 0600); e != nil {
					t.Fatal(e)
				}
			case "wrong version":
				p := filepath.Join(root, Owner)
				b, _ := os.ReadFile(p)
				if e := os.WriteFile(p, []byte(strings.ReplaceAll(string(b), Version, "v0.0.1")), 0600); e != nil {
					t.Fatal(e)
				}
			case "wrong checksum":
				p := filepath.Join(root, filepath.Dir(Owner), "go.sum")
				if e := os.WriteFile(p, []byte(Module+" "+Version+" h1:wrong\n"), 0600); e != nil {
					t.Fatal(e)
				}
			case "target replace":
				appendFile("go.mod", "replace "+Module+" => ./other\n")
			case "new owner replace":
				appendFile(Owner, "replace example.invalid/changed => ./other\n")
			case "workspace":
				appendFile("go.work", "go 1.27.3\n")
				files = append(files, "go.work")
			case "vendor":
				if e := os.MkdirAll(filepath.Join(root, "vendor"), 0700); e != nil {
					t.Fatal(e)
				}
				appendFile("vendor/modules.txt", "# module\n")
				files = append(files, "vendor/modules.txt")
			case "nested module":
				p := filepath.Join(filepath.Dir(Owner), "nested/go.mod")
				if e := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0700); e != nil {
					t.Fatal(e)
				}
				appendFile(p, "module nested.example\n")
				files = append(files, p)
			case "untracked Go":
				appendFile(filepath.Join(filepath.Dir(Owner), "ignored_windows.go"), "package main\n")
			case "symlink":
				p := filepath.Join(root, Owner)
				if e := os.Remove(p); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink("/outside", p); e != nil {
					t.Fatal(e)
				}
			case "missing input":
				if e := os.Remove(filepath.Join(root, Owner)); e != nil {
					t.Fatal(e)
				}
			}
			if _, err := inspectInputs(root, files); err == nil {
				t.Fatal("unsupported source retained a proof scope")
			}
		})
	}
}
