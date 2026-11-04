package celapplicability

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type trackedFile struct {
	Path, OID, Mode string
	Data            []byte
}

type snapshot struct {
	Head, Tree, InventorySHA256 string
	Files                       map[string]trackedFile
}

func readSnapshot(ctx context.Context, root string) (snapshot, error) {
	var result snapshot
	git := func(args ...string) ([]byte, error) {
		return run(ctx, root, "git", append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	kind, err := git("cat-file", "-t", "HEAD")
	if err != nil || strings.TrimSpace(string(kind)) != "commit" {
		return result, errors.New("CEL proof requires a commit-backed HEAD")
	}
	tree, err := git("rev-parse", "HEAD^{tree}")
	if err != nil {
		return result, err
	}
	format, err := git("rev-parse", "--show-object-format")
	if err != nil || strings.TrimSpace(string(format)) != "sha1" {
		return result, errors.New("CEL proof requires the declared SHA1 Git object format")
	}
	untracked, err := git("ls-files", "--others", "-z")
	if err != nil || len(untracked) != 0 {
		return result, errors.New("CEL proof requires a complete tracked snapshot without untracked or ignored files")
	}
	index, err := git("ls-files", "--stage", "-z")
	if err != nil {
		return result, err
	}
	treeFiles, err := git("ls-tree", "-r", "-z", "HEAD")
	if err != nil {
		return result, err
	}
	entries, err := matchingEntries(index, treeFiles)
	if err != nil {
		return result, err
	}
	result.Files = make(map[string]trackedFile, len(entries))
	inventory := sha256.New()
	_, _ = inventory.Write(index)
	var totalBytes int64
	for _, file := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		full := filepath.Join(root, filepath.FromSlash(file.Path))
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 || (info.Mode().Perm()&0111 != 0) != (file.Mode == "100755") {
			return result, fmt.Errorf("snapshot file is missing, linked, oversized or has changed mode: %s", file.Path)
		}
		file.Data, err = os.ReadFile(full)
		if err != nil {
			return result, err
		}
		digest := sha1.New()
		_, _ = fmt.Fprintf(digest, "blob %d\x00", len(file.Data))
		_, _ = digest.Write(file.Data)
		if hex.EncodeToString(digest.Sum(nil)) != file.OID {
			return result, fmt.Errorf("snapshot bytes differ from HEAD/index: %s", file.Path)
		}
		totalBytes += int64(len(file.Data))
		if totalBytes > 512<<20 {
			return result, errors.New("source snapshot exceeds its byte bound")
		}
		content := sha256.Sum256(file.Data)
		_, _ = fmt.Fprintf(inventory, "%s\x00%x\n", file.Path, content)
		result.Files[file.Path] = file
	}
	result.Head, result.Tree = strings.TrimSpace(string(head)), strings.TrimSpace(string(tree))
	result.InventorySHA256 = hex.EncodeToString(inventory.Sum(nil))
	return result, nil
}

func matchingEntries(index, tree []byte) ([]trackedFile, error) {
	indexRows, treeRows := bytes.Split(bytes.TrimSuffix(index, []byte{0}), []byte{0}), bytes.Split(bytes.TrimSuffix(tree, []byte{0}), []byte{0})
	if len(indexRows) == 0 || len(indexRows) != len(treeRows) || len(indexRows) > 50000 {
		return nil, errors.New("HEAD/index inventories differ or exceed their bound")
	}
	var files []trackedFile
	seen := map[string]bool{}
	for n, row := range indexRows {
		meta, name, ok := strings.Cut(string(row), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[2] != "0" || (fields[0] != "100644" && fields[0] != "100755") || len(fields[1]) != 40 ||
			name == "" || filepath.IsAbs(name) || filepath.ToSlash(filepath.Clean(name)) != name || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\r\n") || seen[name] {
			return nil, errors.New("invalid, linked, conflicted or duplicate tracked source entry")
		}
		expected := fields[0] + " blob " + fields[1] + "\t" + name
		if string(treeRows[n]) != expected {
			return nil, errors.New("index does not match HEAD")
		}
		seen[name] = true
		files = append(files, trackedFile{Path: name, Mode: fields[0], OID: fields[1]})
	}
	return files, nil
}
