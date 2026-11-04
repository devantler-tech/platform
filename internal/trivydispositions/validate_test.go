package trivydispositions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repository(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func copyInputs(t *testing.T) string {
	t.Helper()
	root, out := repository(t), t.TempDir()
	for _, path := range []string{".trivyignore.yaml", "k8s/bases/apps/homepage/config-map.yaml", "k8s/bases/infrastructure/coroot/components/security-progress/config-map.yaml", "k8s/bases/bootstrap/limit-ranges/kube-system.yaml"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(out, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Validate(out); err != nil {
		t.Fatalf("reviewed baseline is invalid: %v", err)
	}
	return out
}

func mutate(t *testing.T, root, path string, change func(map[string]any)) {
	t.Helper()
	file := filepath.Join(root, path)
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	change(obj)
	b, err = yaml.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedDispositions(t *testing.T) {
	if err := Validate(repository(t)); err != nil {
		t.Fatal(err)
	}
}

func TestEquivalentSerializationPreservesTheDisposition(t *testing.T) {
	root := copyInputs(t)
	for _, path := range []string{"k8s/bases/apps/homepage/config-map.yaml", "k8s/bases/infrastructure/coroot/components/security-progress/config-map.yaml", "k8s/bases/bootstrap/limit-ranges/kube-system.yaml"} {
		file := filepath.Join(root, path)
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := yaml.Unmarshal(b, &obj); err != nil {
			t.Fatal(err)
		}
		b, err = json.MarshalIndent(obj, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Validate(root); err != nil {
		t.Fatalf("equivalent parsed input refused: %v", err)
	}
}

func TestConfigMapChangesInvalidateTheDisposition(t *testing.T) {
	for _, path := range []string{"k8s/bases/apps/homepage/config-map.yaml", "k8s/bases/infrastructure/coroot/components/security-progress/config-map.yaml"} {
		for name, change := range map[string]func(map[string]any){
			"added data": func(o map[string]any) { o["data"].(map[string]any)["new"] = "added" },
			"changed data": func(o map[string]any) {
				for k, v := range o["data"].(map[string]any) {
					o["data"].(map[string]any)[k] = v.(string) + "\nchanged-value"
					break
				}
			},
			"deleted data": func(o map[string]any) {
				for k := range o["data"].(map[string]any) {
					delete(o["data"].(map[string]any), k)
					break
				}
			},
			"actual secret":   func(o map[string]any) { o["data"].(map[string]any)["password"] = "synthetic-boundary-secret" },
			"non-string data": func(o map[string]any) { o["data"].(map[string]any)["new"] = 17 },
			"kind":            func(o map[string]any) { o["kind"] = "Secret" },
			"apiVersion":      func(o map[string]any) { o["apiVersion"] = "other.example/v1" },
			"name":            func(o map[string]any) { o["metadata"].(map[string]any)["name"] = "other" },
			"namespace":       func(o map[string]any) { o["metadata"].(map[string]any)["namespace"] = "other" },
			"owner": func(o map[string]any) {
				o["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"name": "other"}}
			},
			"binaryData": func(o map[string]any) { o["binaryData"] = map[string]any{"key": "dmFsdWU="} },
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				root := copyInputs(t)
				mutate(t, root, path, change)
				if err := Validate(root); err == nil {
					t.Fatal("changed input retained its disposition")
				}
			})
		}
	}
}

func TestLimitRangeIdentityAndEveryPropertyAreBound(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"namespace": func(o map[string]any) { o["metadata"].(map[string]any)["namespace"] = "application" },
		"name":      func(o map[string]any) { o["metadata"].(map[string]any)["name"] = "application-limits" },
		"kind":      func(o map[string]any) { o["kind"] = "Pod" },
		"group":     func(o map[string]any) { o["apiVersion"] = "example/v1" },
		"owner": func(o map[string]any) {
			o["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"name": "other"}}
		},
		"changed default": func(o map[string]any) {
			o["spec"].(map[string]any)["limits"].([]any)[0].(map[string]any)["default"].(map[string]any)["cpu"] = "3"
		},
		"added limit": func(o map[string]any) {
			o["spec"].(map[string]any)["limits"].([]any)[0].(map[string]any)["default"].(map[string]any)["memory"] = "1Gi"
		},
		"removed defaultRequest": func(o map[string]any) {
			delete(o["spec"].(map[string]any)["limits"].([]any)[0].(map[string]any), "defaultRequest")
		},
		"added rule": func(o map[string]any) {
			o["spec"].(map[string]any)["limits"] = append(o["spec"].(map[string]any)["limits"].([]any), map[string]any{"type": "Pod"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := copyInputs(t)
			mutate(t, root, "k8s/bases/bootstrap/limit-ranges/kube-system.yaml", change)
			if err := Validate(root); err == nil {
				t.Fatal("changed LimitRange retained its disposition")
			}
		})
	}
}

func TestWidenedOrMissingScopesRefuse(t *testing.T) {
	for _, id := range []string{"KSV-0037", "KSV-0109", "KSV-01010"} {
		for _, mode := range []string{"global", "widen", "duplicate", "missing", "statement"} {
			t.Run(id+"/"+mode, func(t *testing.T) {
				root := copyInputs(t)
				mutate(t, root, ".trivyignore.yaml", func(o map[string]any) {
					entries := o["misconfigurations"].([]any)
					for i, e := range entries {
						m := e.(map[string]any)
						if m["id"] != id {
							continue
						}
						switch mode {
						case "global":
							delete(m, "paths")
						case "widen":
							m["paths"] = []any{"k8s/**"}
						case "duplicate":
							o["misconfigurations"] = append(entries, m)
						case "missing":
							o["misconfigurations"] = append(entries[:i], entries[i+1:]...)
						case "statement":
							for _, entry := range entries {
								n := entry.(map[string]any)
								if n["id"] == id {
									n["statement"] = "unreviewed"
								}
							}
						}
						break
					}
				})
				if err := Validate(root); err == nil {
					t.Fatal("unsafe scope retained its disposition")
				}
			})
		}
	}
}

func TestUnreadableLinkedAndMultipleDocumentsRefuse(t *testing.T) {
	for _, mode := range []string{"missing", "directory", "linked-file", "linked-parent", "unreadable", "extra-document", "duplicate-key", "moved-path"} {
		t.Run(mode, func(t *testing.T) {
			root := copyInputs(t)
			path := filepath.Join(root, "k8s/bases/apps/homepage/config-map.yaml")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unreadable":
				err = os.Chmod(path, 0000)
			case "extra-document":
				err = os.WriteFile(path, append(b, []byte("\n---\nkind: Pod\n")...), 0600)
			case "duplicate-key":
				err = os.WriteFile(path, append(b, []byte("\nkind: ConfigMap\n")...), 0600)
			case "linked-parent":
				parent := filepath.Dir(path)
				err = os.Rename(parent, parent+"-real")
				if err == nil {
					err = os.Symlink(parent+"-real", parent)
				}
			default:
				err = os.Remove(path)
				if err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "directory":
					err = os.Mkdir(path, 0700)
				case "linked-file":
					target := filepath.Join(root, "outside")
					err = os.WriteFile(target, b, 0600)
					if err == nil {
						err = os.Symlink(target, path)
					}
				case "moved-path":
					moved := strings.Replace(path, "homepage/", "unlisted/", 1)
					err = os.MkdirAll(filepath.Dir(moved), 0700)
					if err == nil {
						err = os.WriteFile(moved, b, 0600)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(root); err == nil {
				t.Fatal("unverifiable input retained its disposition")
			}
		})
	}
}
