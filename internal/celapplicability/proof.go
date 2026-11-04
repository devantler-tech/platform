package celapplicability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const advisorySHA256 = "43d93519af430be9decc03ed724462f17dcf457de548306d2007cf77c044075e"

type owner struct{ Directory, Module string }

var owners = []owner{
	{"scripts/migrate-longhorn-v2/testdata/retirement-fence-proof", "github.com/devantler-tech/platform/scripts/migrate-longhorn-v2/retirement-fence-proof"},
	{"scripts/tests/longhorn-benchmark-delete-guard/testdata/native-proof", "github.com/devantler-tech/platform/testfixtures/longhorn-benchmark-delete-guard"},
}

// Scope is a single source occurrence admitted by the proof, never a global ID waiver.
type Scope struct {
	GoModPath   string `json:"go_mod_path"`
	GoSumPath   string `json:"go_sum_path"`
	OwnerModule string `json:"owner_module"`
	Module      string `json:"module"`
	Version     string `json:"version"`
	ModuleSum   string `json:"module_sum"`
	GoModSum    string `json:"go_mod_sum"`
	SourceFiles int    `json:"first_party_source_files"`
}

// Proof records the narrow VEX facts and the immutable inputs that support them.
// Its static identifier assessment does not model arbitrary runtime-loaded code.
type Proof struct {
	Vulnerability    string         `json:"vulnerability"`
	Aliases          []string       `json:"aliases"`
	Justification    string         `json:"justification"`
	Head             string         `json:"head"`
	Tree             string         `json:"tree"`
	InventorySHA256  string         `json:"snapshot_sha256"`
	AdvisorySHA256   string         `json:"advisory_sha256"`
	DatabaseModified time.Time      `json:"database_modified"`
	VerifiedAt       time.Time      `json:"verified_at"`
	SourceSHA256     string         `json:"analyzed_source_sha256"`
	SourceFiles      int            `json:"analyzed_source_files"`
	Declarations     map[string]int `json:"canonical_declarations"`
	Scopes           []Scope        `json:"scopes"`
}

// Prove checks only GHSA-gcjh-h69q-9w9g / GO-2026-6094 for the two known CEL
// v0.26.0 fixture owners. Native SCA must read this same immutable checkout and
// separately reject any match outside the exact scopes returned here.
func Prove(parent context.Context, root string) (Proof, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	var proof Proof
	if runtime.Version() != "go1.27.3" {
		return proof, errors.New("CEL source proof requires Go 1.27.3")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return proof, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return proof, err
	}
	before, err := readSnapshot(ctx, root)
	if err != nil {
		return proof, err
	}
	if err := checkOwners(ctx, root, before.Files); err != nil {
		return proof, err
	}
	data, err := run(ctx, root, "go", "env", "-json", "GOVERSION", "GOMODCACHE")
	if err != nil {
		return proof, err
	}
	var environment struct{ GOVERSION, GOMODCACHE string }
	if json.Unmarshal(data, &environment) != nil || environment.GOVERSION != "go1.27.3" || !filepath.IsAbs(environment.GOMODCACHE) || within(root, environment.GOMODCACHE) {
		return proof, errors.New("unknown Go toolchain or unsafe module cache")
	}
	modified, err := checkAdvisory(ctx, time.Now().UTC())
	if err != nil {
		return proof, err
	}
	declarations := map[string]int{}
	digest := sha256.New()
	seenModules := map[string]nativeModule{}
	for _, owner := range owners {
		modules, cleanup, err := loadModules(ctx, owner, environment.GOMODCACHE, before.Files)
		if err != nil {
			return proof, err
		}
		defer cleanup()
		physicalCache, err := filepath.EvalSymlinks(environment.GOMODCACHE)
		if err != nil || physicalCache == root || within(root, physicalCache) {
			return proof, errors.New("module cache overlaps the source snapshot")
		}
		var paths []string
		for name := range before.Files {
			if strings.HasPrefix(name, owner.Directory+"/") && sourceExtension(name) {
				paths = append(paths, name)
			}
		}
		slices.Sort(paths)
		count := 0
		for _, name := range paths {
			if err := ctx.Err(); err != nil {
				return proof, err
			}
			file := before.Files[name]
			if err := analyze(sourceFile{Path: name, FirstParty: true, Data: file.Data}, declarations, digest); err != nil {
				return proof, err
			}
			proof.SourceFiles++
			if strings.HasSuffix(name, ".go") {
				count++
			}
		}
		if count == 0 {
			return proof, errors.New("CEL owner has no examined first-party Go sources")
		}
		proof.Scopes = append(proof.Scopes, Scope{GoModPath: owner.Directory + "/go.mod", GoSumPath: owner.Directory + "/go.sum", OwnerModule: owner.Module, Module: modulePath, Version: moduleVersion, ModuleSum: celSum, GoModSum: celModSum, SourceFiles: count})
		for _, module := range modules {
			if module.Main {
				continue
			}
			key := module.Path + "@" + module.Version
			if old, ok := seenModules[key]; ok {
				if old.Sum != module.Sum || old.GoModSum != module.GoModSum || old.Dir != module.Dir {
					return proof, errors.New("module source changed between owners")
				}
				continue
			}
			seenModules[key] = module
			if err := filepath.WalkDir(module.Dir, func(name string, entry fs.DirEntry, walkErr error) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if walkErr != nil {
					return walkErr
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return errors.New("linked dependency source is unsupported")
				}
				if entry.IsDir() || !sourceExtension(name) || strings.HasSuffix(name, "_test.go") {
					return nil
				}
				info, err := entry.Info()
				if err != nil || !info.Mode().IsRegular() || info.Size() > 32<<20 {
					return errors.New("missing, nonregular or oversized dependency source")
				}
				data, err := os.ReadFile(name)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(module.Dir, name)
				if err != nil {
					return err
				}
				if err := analyze(sourceFile{Module: module.Path, Version: module.Version, Path: filepath.ToSlash(rel), Data: data}, declarations, digest); err != nil {
					return err
				}
				proof.SourceFiles++
				if proof.SourceFiles > 100000 {
					return errors.New("CEL source inventory exceeds its bound")
				}
				return nil
			}); err != nil {
				return proof, err
			}
		}
		for _, module := range modules {
			if module.Main {
				if _, err := run(ctx, module.Dir, "go", "mod", "verify"); err != nil {
					return proof, err
				}
			}
		}
	}
	if err := completeDeclarations(declarations); err != nil {
		return proof, err
	}
	after, err := readSnapshot(ctx, root)
	if err != nil {
		return proof, err
	}
	if before.Head != after.Head || before.Tree != after.Tree || before.InventorySHA256 != after.InventorySHA256 {
		return proof, errors.New("source snapshot changed during CEL proof")
	}
	proof.Vulnerability, proof.Aliases = "GHSA-gcjh-h69q-9w9g", []string{"GO-2026-6094"}
	proof.Justification = "vulnerable_code_not_in_execute_path"
	proof.Head, proof.Tree, proof.InventorySHA256 = before.Head, before.Tree, before.InventorySHA256
	proof.AdvisorySHA256, proof.DatabaseModified, proof.VerifiedAt = advisorySHA256, modified, time.Now().UTC()
	proof.SourceSHA256, proof.Declarations = hex.EncodeToString(digest.Sum(nil)), declarations
	return proof, nil
}

func sourceExtension(path string) bool {
	switch filepath.Ext(path) {
	case ".go", ".s", ".S", ".asm", ".c", ".cc", ".cpp", ".cxx", ".h", ".m", ".mm", ".syso", ".o", ".a":
		return true
	}
	return false
}

func analyze(source sourceFile, declarations map[string]int, digest io.Writer) error {
	if strings.HasSuffix(source.Path, ".go") {
		if err := inspectGo(source, declarations); err != nil {
			return err
		}
	} else if err := inspectNative(source); err != nil {
		return err
	}
	sum := sha256.Sum256(source.Data)
	_, err := fmt.Fprintf(digest, "%s\x00%s\x00%s\x00%x\n", source.Module, source.Version, source.Path, sum)
	return err
}

func checkAdvisory(ctx context.Context, now time.Time) (time.Time, error) {
	get := func(url string) ([]byte, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
		if err != nil {
			return nil, err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return nil, errors.New("primary Go advisory database is unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			return nil, errors.New("primary Go advisory response is unreadable or oversized")
		}
		return data, nil
	}
	index, err := get("https://vuln.go.dev/index/db.json")
	if err != nil {
		return time.Time{}, err
	}
	data, err := get("https://vuln.go.dev/ID/GO-2026-6094.json")
	if err != nil {
		return time.Time{}, err
	}
	return validateAdvisory(index, data, now)
}

func validateAdvisory(index, data []byte, now time.Time) (time.Time, error) {
	var database struct {
		Modified time.Time `json:"modified"`
	}
	if json.Unmarshal(index, &database) != nil || database.Modified.IsZero() || now.Sub(database.Modified) > 72*time.Hour || database.Modified.After(now.Add(5*time.Minute)) {
		return time.Time{}, errors.New("primary Go advisory database freshness is unknown")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != advisorySHA256 {
		return time.Time{}, errors.New("reviewed CEL advisory, alias or symbol scope changed")
	}
	return database.Modified, nil
}

func checkOwners(ctx context.Context, root string, files map[string]trackedFile) error {
	allowed := map[string]owner{}
	for _, owner := range owners {
		allowed[owner.Directory] = owner
	}
	counts := map[string]int{}
	for path, file := range files {
		if filepath.Base(path) == "go.work" {
			return errors.New("workspace source is unsupported by CEL proof")
		}
		if filepath.Base(path) == "go.sum" || filepath.Base(path) == "go.work.sum" {
			for _, line := range strings.Split(string(file.Data), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[0] == modulePath && (fields[1] == moduleVersion || fields[1] == moduleVersion+"/go.mod") {
					owner, ok := allowed[filepath.ToSlash(filepath.Dir(path))]
					if !ok || len(fields) != 3 {
						return errors.New("unexpected CEL v0.26.0 source occurrence")
					}
					expected := celSum
					if strings.HasSuffix(fields[1], "/go.mod") {
						expected = celModSum
					}
					if fields[2] != expected {
						return errors.New("CEL source occurrence has an unreviewed checksum")
					}
					counts[owner.Directory+"/"+fields[1]]++
				}
			}
		}
		if filepath.Base(path) != "go.mod" {
			continue
		}
		data, err := run(ctx, root, "go", "mod", "edit", "-json", filepath.Join(root, path))
		if err != nil {
			return err
		}
		var mod struct {
			Module  struct{ Path string }
			Require []struct{ Path, Version string }
			Replace []struct {
				Old, New struct{ Path, Version string }
			}
		}
		if json.Unmarshal(data, &mod) != nil {
			return errors.New("unreadable tracked Go module")
		}
		for _, replacement := range mod.Replace {
			if replacement.Old.Path == modulePath || replacement.New.Path == modulePath {
				return errors.New("CEL module replacement is outside this proof")
			}
		}
		for _, dependency := range mod.Require {
			if dependency.Path != modulePath {
				continue
			}
			owner, ok := allowed[filepath.ToSlash(filepath.Dir(path))]
			if !ok {
				if dependency.Version == moduleVersion {
					return errors.New("unexpected owner of CEL v0.26.0")
				}
				continue
			}
			if dependency.Version != moduleVersion || mod.Module.Path != owner.Module || len(mod.Replace) != 0 {
				return errors.New("CEL fixture owner, version or replacements changed")
			}
			counts[owner.Directory+"/go.mod"]++
		}
	}
	for _, owner := range owners {
		for _, suffix := range []string{"go.mod", moduleVersion, moduleVersion + "/go.mod"} {
			if counts[owner.Directory+"/"+suffix] != 1 {
				return errors.New("missing or duplicate CEL owner/module occurrence")
			}
		}
	}
	return nil
}
