package acrapplicability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

const affectedPackage = Module + "/pkg/credhelper"
const controlName = "TestACRReachabilityControl"

type object = map[string]any

func str(m object, key string) string { s, _ := m[key].(string); return s }
func array(v any) []any               { a, _ := v.([]any); return a }
func mapping(v any) object            { m, _ := v.(map[string]any); return m }
func canonical(b []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func advisoryScope(data []byte) (object, error) {
	var entry object
	if err := json.Unmarshal(data, &entry); err != nil || str(entry, "id") != Advisory || str(entry, "modified") == "" {
		return nil, fmt.Errorf("canonical ACR advisory is invalid")
	}
	affected := array(entry["affected"])
	if len(affected) != 1 {
		return nil, fmt.Errorf("ACR advisory module scope changed")
	}
	a := mapping(affected[0])
	pkg := mapping(a["package"])
	if str(pkg, "name") != Module || str(pkg, "ecosystem") != "Go" {
		return nil, fmt.Errorf("ACR advisory module identity changed")
	}
	ranges := array(a["ranges"])
	if len(ranges) != 1 || str(mapping(ranges[0]), "type") != "SEMVER" {
		return nil, fmt.Errorf("ACR advisory version scope changed")
	}
	events := array(mapping(ranges[0])["events"])
	if len(events) != 1 || len(mapping(events[0])) != 1 || str(mapping(events[0]), "introduced") != "0" {
		return nil, fmt.Errorf("ACR advisory affected ranges changed")
	}
	imports := array(mapping(a["ecosystem_specific"])["imports"])
	if len(imports) != 1 {
		return nil, fmt.Errorf("ACR advisory import scope changed")
	}
	imp := mapping(imports[0])
	if str(imp, "path") != affectedPackage || len(array(imp["goos"])) != 0 || len(array(imp["goarch"])) != 0 {
		return nil, fmt.Errorf("ACR advisory target scope changed")
	}
	var symbols []string
	for _, s := range array(imp["symbols"]) {
		value, ok := s.(string)
		if !ok {
			return nil, fmt.Errorf("invalid advisory symbol")
		}
		symbols = append(symbols, value)
	}
	slices.Sort(symbols)
	if !slices.Equal(symbols, []string{"ACRCredHelper.Get", "isACRRegistry"}) {
		return nil, fmt.Errorf("ACR advisory symbols changed")
	}
	if _, withdrawn := entry["withdrawn"]; withdrawn {
		return nil, fmt.Errorf("ACR advisory was withdrawn")
	}
	return entry, nil
}

type nativeVerdict struct {
	Reachable, ModuleFindings, PackageFindings int
	Control                                    bool
	Roots, OtherIDs                            []string
}

func nativeReport(data, advisory []byte, modified string, paired bool, positiveRoots []string) (nativeVerdict, error) {
	var result nativeVerdict
	decoder := json.NewDecoder(bytes.NewReader(data))
	configs, sboms, osvs := 0, 0, 0
	other := map[string]bool{}
	for {
		var frame map[string]json.RawMessage
		err := decoder.Decode(&frame)
		if err == io.EOF {
			break
		}
		if err != nil || len(frame) != 1 {
			return result, fmt.Errorf("incomplete or malformed native JSON protocol")
		}
		for kind, raw := range frame {
			var m object
			if err := json.Unmarshal(raw, &m); err != nil || m == nil {
				return result, fmt.Errorf("invalid native JSON message")
			}
			switch kind {
			case "config":
				configs++
				for key, want := range map[string]string{"protocol_version": "v1.0.0", "scanner_name": "govulncheck", "scanner_version": "v1.8.0", "db": "https://vuln.go.dev", "db_last_modified": modified, "go_version": "go1.27.3", "scan_level": "symbol", "scan_mode": "source"} {
					if str(m, key) != want {
						return result, fmt.Errorf("native scan identity or mode mismatch: %s", key)
					}
				}
			case "SBOM":
				sboms++
				if str(m, "go_version") != "go1.27.3" {
					return result, fmt.Errorf("native SBOM compiler mismatch")
				}
				found := 0
				for _, v := range array(m["modules"]) {
					mod := mapping(v)
					if target(str(mod, "path")) {
						if str(mod, "path") != Module || str(mod, "version") != Version {
							return result, fmt.Errorf("native SBOM affected version mismatch")
						}
						found++
					}
				}
				if found != 1 {
					return result, fmt.Errorf("native SBOM omitted or duplicated the affected module")
				}
				for _, v := range array(m["roots"]) {
					r, ok := v.(string)
					if !ok || strings.ContainsAny(r, " \n\t") {
						return result, fmt.Errorf("invalid native code root")
					}
					result.Roots = append(result.Roots, r)
				}
			case "osv":
				if str(m, "id") == Advisory {
					osvs++
					actual, e := canonical(raw)
					if e != nil {
						return result, e
					}
					want, e := canonical(advisory)
					if e != nil || !bytes.Equal(actual, want) {
						return result, fmt.Errorf("native advisory differs from the fresh canonical record")
					}
				}
			case "finding":
				id := str(m, "osv")
				if id == "" {
					return result, fmt.Errorf("native finding has no advisory")
				}
				if id != Advisory {
					other[id] = true
					continue
				}
				trace := array(m["trace"])
				if len(trace) == 0 {
					return result, fmt.Errorf("target finding has no trace")
				}
				first := mapping(trace[0])
				for _, key := range []string{"function", "receiver", "package"} {
					if value, exists := first[key]; exists && value != nil {
						if _, ok := value.(string); !ok {
							return result, fmt.Errorf("malformed native target trace field: %s", key)
						}
					}
				}
				if str(first, "module") != Module || str(first, "version") != Version {
					return result, fmt.Errorf("target finding has an unproven module or version")
				}
				fn := str(first, "function")
				pkg := str(first, "package")
				if fn != "" {
					if pkg != affectedPackage {
						return result, fmt.Errorf("target finding has an unknown affected package")
					}
					symbol := fn
					if receiver := strings.TrimPrefix(str(first, "receiver"), "*"); receiver != "" {
						symbol = receiver + "." + fn
					}
					if symbol != "ACRCredHelper.Get" && symbol != "isACRRegistry" {
						return result, fmt.Errorf("native finding refers to an unreviewed symbol")
					}
					result.Reachable++
					for _, v := range trace {
						f := mapping(v)
						if str(f, "function") == controlName && str(f, "package") == ownerModule {
							result.Control = true
						}
					}
				} else if pkg != "" {
					if pkg != affectedPackage {
						return result, fmt.Errorf("native finding package changed")
					}
					result.PackageFindings++
				} else {
					result.ModuleFindings++
				}
			case "progress":
			default:
				return result, fmt.Errorf("unknown native protocol message")
			}
		}
	}
	if configs != 1 || sboms != 1 || osvs != 1 || result.ModuleFindings < 1 || result.PackageFindings < 1 {
		return result, fmt.Errorf("native report did not positively examine the affected module and package")
	}
	for _, root := range append(slices.Clone(positiveRoots), ownerModule+".test") {
		if !slices.Contains(result.Roots, root) {
			return result, fmt.Errorf("native report omitted a compiled code or test root")
		}
	}
	if (!paired && result.Reachable != 0) || (paired && (result.Reachable == 0 || !result.Control)) {
		return result, fmt.Errorf("native ACR reachability or paired control did not meet its expectation")
	}
	for id := range other {
		result.OtherIDs = append(result.OtherIDs, id)
	}
	slices.Sort(result.OtherIDs)
	return result, nil
}

type listedPackage struct {
	ImportPath, Dir, ForTest                                     string
	GoFiles, CgoFiles, TestGoFiles, XTestGoFiles, IgnoredGoFiles []string
	Incomplete                                                   bool
	Error                                                        any
	DepsErrors                                                   []any
	Module                                                       *struct {
		Path string
		Main bool
	}
}

func compiledInputs(root string, files []string, data []byte) ([]string, error) {
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	seen := map[string]bool{}
	var roots []string
	ownerDir := filepath.Join(root, filepath.Dir(Owner))
	for {
		var p listedPackage
		err := decoder.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil || p.Error != nil || p.Incomplete || len(p.DepsErrors) > 0 {
			return nil, fmt.Errorf("native Go package loading was incomplete")
		}
		if p.Module == nil || p.Module.Path != ownerModule || !p.Module.Main {
			return nil, fmt.Errorf("package root escaped the owner module")
		}
		p.Dir, err = filepath.EvalSymlinks(p.Dir)
		if err != nil {
			return nil, fmt.Errorf("first-party package directory is missing")
		}
		if len(p.IgnoredGoFiles) > 0 || len(p.CgoFiles) > 0 {
			return nil, fmt.Errorf("first-party Go build variants were not examined")
		}
		if p.ForTest == "" && !strings.HasSuffix(p.ImportPath, ".test") && len(p.GoFiles) > 0 {
			roots = append(roots, p.ImportPath)
		}
		for _, name := range append(append(slices.Clone(p.GoFiles), p.TestGoFiles...), p.XTestGoFiles...) {
			path := name
			if !filepath.IsAbs(path) {
				path = filepath.Join(p.Dir, name)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(path, ownerDir+string(filepath.Separator)) {
				seen[filepath.ToSlash(rel)] = true
			} else if p.ImportPath != ownerModule+".test" {
				return nil, fmt.Errorf("first-party compilation used an external Go source")
			}
		}
	}
	if len(roots) == 0 || !slices.Contains(roots, ownerModule) {
		return nil, fmt.Errorf("owner has no positive Go code root")
	}
	for _, file := range files {
		if !seen[file] {
			return nil, fmt.Errorf("tracked first-party Go input was not compiled: %s", file)
		}
	}
	slices.Sort(roots)
	return slices.Compact(roots), nil
}
