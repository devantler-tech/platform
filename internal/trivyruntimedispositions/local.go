package trivyruntimedispositions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Prove that the native renderer's complete reachable input closure is local.
// A new remote base must fail before kubectl can fetch it.
func localKustomize(root, dir string, seen map[string]bool) error {
	if seen[dir] {
		return nil
	}
	seen[dir] = true
	var data []byte
	found := 0
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		rel := filepath.ToSlash(filepath.Join(dir, name))
		if _, err := os.Lstat(filepath.Join(root, rel)); os.IsNotExist(err) {
			continue
		}
		b, err := read(root, rel)
		if err != nil {
			return err
		}
		data = b
		found++
	}
	if found != 1 {
		return fmt.Errorf("%s: expected one local Kustomize configuration", dir)
	}
	config, err := object(data)
	if err != nil {
		return err
	}
	visit := func(ref string) error {
		if ref == "" || filepath.IsAbs(ref) || strings.ContainsAny(ref, ":?") {
			return fmt.Errorf("%s: nonlocal Kustomize input refused", dir)
		}
		rel := filepath.ToSlash(filepath.Clean(filepath.Join(dir, ref)))
		if rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("%s: Kustomize input escapes source root", dir)
		}
		info, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			return fmt.Errorf("%s: missing local Kustomize input", rel)
		}
		if info.IsDir() {
			return localKustomize(root, rel, seen)
		}
		_, err = read(root, rel)
		return err
	}
	stringsFrom := func(value any) ([]string, error) {
		if value == nil {
			return nil, nil
		}
		rows, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: invalid Kustomize input list", dir)
		}
		result := make([]string, 0, len(rows))
		for _, v := range rows {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s: nonliteral Kustomize input", dir)
			}
			result = append(result, s)
		}
		return result, nil
	}
	for _, key := range []string{"resources", "bases", "components", "transformers", "generators", "configurations", "crds", "patchesStrategicMerge"} {
		refs, err := stringsFrom(config[key])
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if key == "patchesStrategicMerge" {
				if inline, parseErr := object([]byte(ref)); parseErr == nil && inline["apiVersion"] != nil && inline["kind"] != nil {
					continue // inline Kubernetes patch, not a loader path
				}
			}
			if err = visit(ref); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"patches", "patchesJson6902", "replacements"} {
		rows, _ := config[key].([]any)
		for _, v := range rows {
			row, _ := v.(map[string]any)
			if path, ok := row["path"].(string); ok {
				if err = visit(path); err != nil {
					return err
				}
			}
		}
	}
	if openapi, ok := config["openapi"].(map[string]any); ok {
		if path, ok := openapi["path"].(string); ok {
			if err = visit(path); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"configMapGenerator", "secretGenerator"} {
		rows, _ := config[key].([]any)
		for _, v := range rows {
			generator, _ := v.(map[string]any)
			for _, field := range []string{"files", "envs"} {
				refs, err := stringsFrom(generator[field])
				if err != nil {
					return err
				}
				for _, ref := range refs {
					if field == "files" {
						if i := strings.Index(ref, "="); i >= 0 {
							ref = ref[i+1:]
						}
					}
					if err = visit(ref); err != nil {
						return err
					}
				}
			}
			if env, ok := generator["env"].(string); ok {
				if err = visit(env); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
