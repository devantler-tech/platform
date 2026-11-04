package acrapplicability

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
)

const (
	// Advisory is the only vulnerability this proof can adjudicate.
	Advisory = "GO-2026-6225"
	// Module is the affected upstream module, never a package-name wildcard.
	Module = "github.com/chrismellard/docker-credential-acr-env"
	// Version is the sole admitted module version.
	Version = "v0.0.0-20230304212654-82a0ddb27589"
	// Owner is the sole admitted source metadata location.
	Owner        = "scripts/tests/tenant-webapp-admission/go.mod"
	ownerModule  = "github.com/devantler-tech/platform/scripts/tests/tenant-webapp-admission"
	moduleSum    = "h1:krfRl01rzPzxSxyLyrChD+U+MzsBXbm0OwYYB67uF+4="
	moduleModSum = "h1:OuDyvmLnMCwa2ep4Jkm6nyA0ocJuZlGyk2gGseVzERM="
)

type inputInventory struct {
	Hashes       map[string]string
	GoFiles      []string
	TrackedFiles []string
}

func digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func target(path string) bool   { return path == Module || strings.HasPrefix(path, Module+"/") }

func inspectInputs(root string, files []string) (inputInventory, error) {
	result := inputInventory{Hashes: map[string]string{}, TrackedFiles: slices.Clone(files)}
	tracked := map[string]bool{}
	for _, path := range files {
		if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, "../") || tracked[path] {
			return result, fmt.Errorf("unsupported or duplicate tracked path")
		}
		tracked[path] = true
	}
	ownerDir := filepath.Dir(Owner)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("linked source is unsupported")
		}
		name := entry.Name()
		if name == "go.work" || name == "go.work.sum" || name == "vendor" || strings.Contains("/"+filepath.ToSlash(rel), "/vendor/") {
			return fmt.Errorf("workspace or vendor resolution is unsupported")
		}
		if entry.IsDir() {
			return nil
		}
		isGo := strings.HasPrefix(rel, ownerDir+"/") && strings.HasSuffix(name, ".go")
		isMetadata := name == "go.mod" || name == "go.sum"
		if (isGo || isMetadata) && !tracked[rel] {
			return fmt.Errorf("untracked analysis input is unsupported")
		}
		return nil
	}); err != nil {
		return result, err
	}
	requires, sums := 0, 0
	seenSums := map[string]bool{}
	for _, path := range files {
		base := filepath.Base(path)
		isGo := strings.HasPrefix(path, ownerDir+"/") && strings.HasSuffix(path, ".go")
		if base != "go.mod" && base != "go.sum" && !isGo {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0444 == 0 || info.Size() > 8<<20 {
			return result, fmt.Errorf("analysis input is missing, unreadable or linked")
		}
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return result, err
		}
		result.Hashes[path] = digest(b)
		if isGo {
			result.GoFiles = append(result.GoFiles, path)
			continue
		}
		if base == "go.mod" {
			if strings.HasPrefix(path, ownerDir+"/") && path != Owner {
				return result, fmt.Errorf("nested owner module is unsupported")
			}
			m, err := modfile.Parse(path, b, nil)
			if err != nil || m.Module == nil {
				return result, fmt.Errorf("invalid module metadata")
			}
			if target(m.Module.Mod.Path) {
				return result, fmt.Errorf("affected module cannot be a first-party root")
			}
			if path == Owner && m.Module.Mod.Path != ownerModule {
				return result, fmt.Errorf("owner module identity changed")
			}
			for _, r := range m.Require {
				if target(r.Mod.Path) {
					if path != Owner || r.Mod.Path != Module || r.Mod.Version != Version {
						return result, fmt.Errorf("affected module has an unproven owner or version")
					}
					requires++
				}
			}
			for _, r := range m.Replace {
				if target(r.Old.Path) || target(r.New.Path) {
					return result, fmt.Errorf("affected module replacement is unsupported")
				}
				if path == Owner && (r.Old.Path != "k8s.io/pod-security-admission" || r.Old.Version != "" || r.New.Path != "github.com/kyverno/pod-security-admission" || r.New.Version != "v0.0.0-20261015135348-40bb0fb141a9") {
					return result, fmt.Errorf("unreviewed owner replacement")
				}
			}
		} else {
			for _, line := range strings.Split(string(b), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				if len(fields) != 3 {
					return result, fmt.Errorf("invalid checksum metadata")
				}
				if !target(fields[0]) {
					continue
				}
				if path != filepath.Join(ownerDir, "go.sum") || fields[0] != Module {
					return result, fmt.Errorf("affected module checksum has another owner")
				}
				if (fields[1] != Version || fields[2] != moduleSum) && (fields[1] != Version+"/go.mod" || fields[2] != moduleModSum) {
					return result, fmt.Errorf("affected module checksum or version changed")
				}
				if seenSums[fields[1]] {
					return result, fmt.Errorf("duplicate affected checksum entry")
				}
				seenSums[fields[1]] = true
				sums++
			}
		}
	}
	if requires != 1 || sums != 2 || len(result.GoFiles) == 0 {
		return result, fmt.Errorf("incomplete unique owner inventory")
	}
	slices.Sort(result.GoFiles)
	return result, nil
}

func (i inputInventory) verify(root string) error {
	if _, err := inspectInputs(root, i.TrackedFiles); err != nil {
		return fmt.Errorf("source inventory changed during ACR proof: %w", err)
	}
	for path, want := range i.Hashes {
		info, e := os.Lstat(filepath.Join(root, path))
		if e != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("source changed during ACR proof")
		}
		b, e := os.ReadFile(filepath.Join(root, path))
		if e != nil || digest(b) != want {
			return fmt.Errorf("source changed during ACR proof")
		}
	}
	return nil
}
