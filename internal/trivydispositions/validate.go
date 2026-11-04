// Package trivydispositions binds narrow Trivy dispositions to reviewed source objects.
package trivydispositions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	homepage     = "k8s/bases/apps/homepage/config-map.yaml"
	progress     = "k8s/bases/infrastructure/coroot/components/security-progress/config-map.yaml"
	limits       = "k8s/bases/bootstrap/limit-ranges/kube-system.yaml"
	homepageData = "9abfd145907ad4c1dd5096b4ed3456c375d010411304ed5ad91331419d877bd3"
	progressData = "0d54ba88c2aad1521c6b5ec9576bda09c5ea0e0c327bdf5c4c9792626b5fdbf0"
)

var reviewedPaths = map[string][]string{
	"KSV-0037":  {"k8s/bases/infrastructure/**", "k8s/providers/*/infrastructure/**", limits},
	"KSV-0109":  {"k8s/bases/apps/actual-budget/config-map.yaml", "k8s/clusters/prod/bootstrap/config-map.yaml", progress},
	"KSV-01010": {"k8s/bases/apps/actual-budget/config-map.yaml", "k8s/clusters/local/bootstrap/config-map.yaml", "k8s/clusters/prod/bootstrap/config-map.yaml", homepage, progress},
}

// Validate refuses changed source, identity or scope before the native ignorefile is used.
// ConfigMap fingerprints are SHA-256 of encoding/json.Marshal on the complete parsed
// map[string]string data field, including every key and every value byte.
func Validate(root string) error {
	ignore, err := readObject(root, ".trivyignore.yaml")
	if err != nil {
		return err
	}
	if err := checkScopes(ignore); err != nil {
		return err
	}
	for _, cm := range []struct {
		path, name, namespace, hash string
		annotations                 map[string]any
	}{
		{homepage, "homepage", "homepage", homepageData, map[string]any{"flagger.app/config-tracking": "disabled"}},
		{progress, "coroot-security-progress", "observability", progressData, nil},
	} {
		obj, err := readObject(root, cm.path)
		if err != nil {
			return err
		}
		metadata := map[string]any{"name": cm.name, "namespace": cm.namespace}
		if cm.annotations != nil {
			metadata["annotations"] = cm.annotations
		}
		if !keysEqual(obj, []string{"apiVersion", "kind", "metadata", "data"}) || obj["apiVersion"] != "v1" || obj["kind"] != "ConfigMap" || !reflect.DeepEqual(obj["metadata"], metadata) {
			return fmt.Errorf("%s: reviewed ConfigMap identity, ownership or shape changed", cm.path)
		}
		values, ok := obj["data"].(map[string]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("%s: missing string data", cm.path)
		}
		data := map[string]string{}
		for key, value := range values {
			str, ok := value.(string)
			if !ok {
				return fmt.Errorf("%s: non-string data", cm.path)
			}
			data[key] = str
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("%s: cannot fingerprint data", cm.path)
		}
		if fmt.Sprintf("%x", sha256.Sum256(encoded)) != cm.hash {
			return fmt.Errorf("%s: complete parsed data fingerprint changed; review the disposition", cm.path)
		}
	}
	obj, err := readObject(root, limits)
	if err != nil {
		return err
	}
	want := map[string]any{
		"apiVersion": "v1", "kind": "LimitRange", "metadata": map[string]any{"name": "default-limitrange", "namespace": "kube-system"},
		"spec": map[string]any{"limits": []any{map[string]any{"type": "Container", "default": map[string]any{"cpu": "2"}, "defaultRequest": map[string]any{"cpu": "15m", "memory": "128Mi"}}}},
	}
	if !reflect.DeepEqual(obj, want) {
		return fmt.Errorf("%s: namespace-local LimitRange identity, ownership or properties changed", limits)
	}
	return nil
}

func checkScopes(ignore map[string]any) error {
	entries, ok := ignore["misconfigurations"].([]any)
	if !ok {
		return fmt.Errorf("missing misconfiguration dispositions")
	}
	got := map[string][]string{}
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid disposition entry")
		}
		id, _ := m["id"].(string)
		if _, reviewed := reviewedPaths[id]; !reviewed {
			continue
		}
		paths, ok := m["paths"].([]any)
		if !ok || len(paths) == 0 {
			return fmt.Errorf("%s: unscoped disposition", id)
		}
		for _, value := range paths {
			path, ok := value.(string)
			if !ok || path == "" {
				return fmt.Errorf("%s: invalid disposition path", id)
			}
			got[id] = append(got[id], path)
			statement, _ := m["statement"].(string)
			var premise string
			switch path {
			case homepage:
				premise = "sha256:" + homepageData
			case progress:
				premise = "sha256:" + progressData
			case limits:
				premise = "namespace-local default-limitrange"
			}
			if premise != "" && !strings.Contains(statement, premise) {
				return fmt.Errorf("%s at %s: reviewed premise missing", id, path)
			}
		}
	}
	for id, paths := range reviewedPaths {
		want, actual := slices.Clone(paths), got[id]
		slices.Sort(want)
		slices.Sort(actual)
		if !slices.Equal(want, actual) {
			return fmt.Errorf("%s: disposition scope changed, duplicated or missing", id)
		}
	}
	return nil
}

func keysEqual(obj map[string]any, want []string) bool {
	if len(obj) != len(want) {
		return false
	}
	for _, key := range want {
		if _, ok := obj[key]; !ok {
			return false
		}
	}
	return true
}

func readObject(root, rel string) (map[string]any, error) {
	current := filepath.Clean(root)
	info, err := os.Lstat(current)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("source root must be a real readable directory")
	}
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err = os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: missing or linked source", rel)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("%s: non-directory source parent", rel)
		}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 || info.Size() > 4<<20 {
		return nil, fmt.Errorf("%s: unreadable or nonregular source", rel)
	}
	file, err := os.Open(current)
	if err != nil {
		return nil, fmt.Errorf("%s: source could not be read", rel)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s: source changed during read", rel)
	}
	b, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return nil, fmt.Errorf("%s: source read failed", rel)
	}
	after, err := os.Lstat(current)
	if err != nil || !os.SameFile(info, after) || after.ModTime() != info.ModTime() || after.Size() != info.Size() {
		return nil, fmt.Errorf("%s: source changed during read", rel)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("%s: invalid YAML", rel)
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("%s: expected exactly one YAML document", rel)
	}
	var obj map[string]any
	if err := node.Decode(&obj); err != nil || len(obj) == 0 {
		return nil, fmt.Errorf("%s: invalid or duplicate YAML mapping", rel)
	}
	return obj, nil
}
