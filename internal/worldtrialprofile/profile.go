// Package worldtrialprofile binds the owner-only trial grant to one audited release.
package worldtrialprofile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	profileDir = "k8s/providers/hetzner/apps/world-at-ruin"
	auditDir   = "internal/worldtrialprofile/testdata/audit"
	receiptSHA = "4bc06320bf809f14c9b1b519149c72f60dae0e54668e7fc6689d4d5f6a4c7709"
)

//go:embed testdata/audit/receipt.json.golden
var receiptBytes []byte

type receipt struct {
	Schema         int               `json:"schema"`
	Artifact       string            `json:"artifact"`
	Image          string            `json:"image"`
	Source         string            `json:"source"`
	Publisher      string            `json:"publisher"`
	Issuer         string            `json:"issuer"`
	FixtureFiles   map[string]string `json:"fixtureFiles"`
	PayloadFiles   map[string]string `json:"payloadFiles"`
	HostFiles      map[string]string `json:"hostFiles"`
	ReferenceFiles map[string]string `json:"referenceFiles"`
}

func reviewed() (receipt, error) {
	var r receipt
	if hash(receiptBytes) != receiptSHA {
		return r, fmt.Errorf("immutable World audit receipt changed; new release audit required")
	}
	if err := json.Unmarshal(receiptBytes, &r); err != nil {
		return r, err
	}
	return r, nil
}

// Validate is offline and reads only the supplied source root. The complete
// private-trial directory, owner grant/binding, isolation, budget, lifecycle,
// Vault policy/identity, and immutable public artifact receipt must remain exact.
// This is a fixed-release configuration proof, not an assertion about arbitrary
// process behaviour or runtime state. A future release requires a new audit.
func Validate(root string) error {
	r, err := reviewed()
	if err != nil {
		return err
	}
	for path, want := range r.HostFiles {
		if _, err := readExact(root, path, want); err != nil {
			return err
		}
	}
	if err := exactDirectory(root, profileDir, r.HostFiles, profileDir+"/"); err != nil {
		return err
	}
	if err := checkReferences(root, r.ReferenceFiles); err != nil {
		return err
	}
	fixtures := make(map[string]string, len(r.FixtureFiles)+1)
	for path, want := range r.FixtureFiles {
		fixtures[path] = want
		if _, err := readExact(root, auditDir+"/"+path, want); err != nil {
			return err
		}
	}
	fixtures["receipt.json.golden"] = receiptSHA
	if _, err := readExact(root, auditDir+"/receipt.json.golden", receiptSHA); err != nil {
		return err
	}
	if err := exactDirectory(root, auditDir, fixtures, ""); err != nil {
		return err
	}
	layer, err := readExact(root, auditDir+"/layer.tar.gz.golden", r.FixtureFiles["layer.tar.gz.golden"])
	if err != nil {
		return err
	}
	_, err = inspectLayer(layer)
	return err
}

// Outside the fixed trial directory, bind only normalized World references and
// the complete dedicated Vault policy/role stanzas. Unrelated app edits remain
// possible. The runtime disposition guard separately compares the effective
// World objects across all six native Kustomize layers, catching generic overlay
// patches and grants with no literal World reference in their source file.
func checkReferences(root string, want map[string]string) error {
	got := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, "k8s"), func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in World source inventory: %s", path)
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		values, err := worldReferences(b)
		if err != nil {
			return fmt.Errorf("world input inventory %s: %w", path, err)
		}
		if len(values) == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = hash(encoded)
		return nil
	})
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("world reference source inventory changed")
	}
	for path, sha := range got {
		if sha != want[path] {
			return fmt.Errorf("world reference/namespace/grant source changed: %s", path)
		}
	}
	return nil
}

func worldReferences(b []byte) ([]string, error) {
	values := []string{}
	d := yaml.NewDecoder(bytes.NewReader(b))
	var visit func(*yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "world-at-ruin") {
			if strings.Contains(n.Value, "bao policy write app-world-at-ruin ") {
				lines := strings.Split(n.Value, "\n")
				capture := ""
				for _, line := range lines {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "bao policy write app-world-at-ruin ") {
						capture = "policy"
					}
					if strings.HasPrefix(line, "bao write auth/kubernetes/role/app-world-at-ruin ") {
						capture = "role"
					}
					if capture != "" {
						values = append(values, line)
						if capture == "policy" && line == "POLICY" || capture == "role" && !strings.HasSuffix(line, "\\") {
							capture = ""
						}
					} else if strings.Contains(line, "world-at-ruin") {
						values = append(values, line)
					}
				}
			} else {
				values = append(values, n.Value)
			}
		}
		for _, child := range n.Content {
			visit(child)
		}
	}
	for {
		var node yaml.Node
		err := d.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		visit(&node)
	}
	slices.Sort(values)
	return values, nil
}

func hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// Every ancestor below root must be a real directory; files must be regular.
// In particular a symlinked profile/receipt cannot borrow an approved identity.
func readExact(root, rel, want string) ([]byte, error) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	path := root
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("unsafe reviewed path %q", rel)
		}
		path = filepath.Join(path, part)
		st, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("world profile %s: %w", rel, err)
		}
		if i < len(parts)-1 && !st.IsDir() || i == len(parts)-1 && !st.Mode().IsRegular() {
			return nil, fmt.Errorf("world profile %s: nonregular path", rel)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if hash(b) != want {
		return nil, fmt.Errorf("world profile %s changed; new private-trial audit required", rel)
	}
	return b, nil
}

func exactDirectory(root, dir string, inventory map[string]string, prefix string) error {
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return err
	}
	count := 0
	for key := range inventory {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	if len(entries) != count {
		return fmt.Errorf("world %s: missing or extra source/artifact input", dir)
	}
	for _, entry := range entries {
		if _, ok := inventory[prefix+entry.Name()]; !ok {
			return fmt.Errorf("world %s: unreviewed input %s", dir, entry.Name())
		}
	}
	return nil
}

// inspectLayer validates the entire archive before any payload file is written.
// Only the seven audited regular files and one root-directory entry are admitted.
// It deliberately also runs on adverse inputs before the outer digest check in tests.
func inspectLayer(raw []byte) (map[string][]byte, error) {
	r, err := reviewed()
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("oversized World layer")
	}
	compressed := bytes.NewReader(raw)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)
	uncompressed, err := io.ReadAll(io.LimitReader(gz, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(uncompressed) > 1<<20 || compressed.Len() != 0 {
		return nil, fmt.Errorf("oversized or trailing World archive content")
	}
	remaining := bytes.NewReader(uncompressed)
	tr := tar.NewReader(remaining)
	files := map[string][]byte{}
	rootSeen := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Name == "." && h.Typeflag == tar.TypeDir && !rootSeen && h.Size == 0 {
			rootSeen = true
			continue
		}
		want, ok := r.PayloadFiles[h.Name]
		if !ok || h.Typeflag != tar.TypeReg || h.Linkname != "" || h.Size < 0 || h.Size > 64<<10 || len(h.PAXRecords) != 0 {
			return nil, fmt.Errorf("unsafe or unreviewed World layer entry %q", h.Name)
		}
		if _, duplicate := files[h.Name]; duplicate {
			return nil, fmt.Errorf("duplicate World layer entry %q", h.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if hash(data) != want {
			return nil, fmt.Errorf("world artifact %s changed; object/image/credential/persistence audit required", h.Name)
		}
		files[h.Name] = data
	}
	if !rootSeen || len(files) != len(r.PayloadFiles) {
		return nil, fmt.Errorf("world archive incomplete")
	}
	padding, err := io.ReadAll(remaining)
	if err != nil {
		return nil, err
	}
	for _, b := range padding {
		if b != 0 {
			return nil, fmt.Errorf("nonzero trailing World tar content")
		}
	}
	return files, nil
}
