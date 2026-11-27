package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveredFilesystemBindsTheMountDataWALAndTablespaceDirectories(t *testing.T) {
	stat := []byte("42 10 directory\n42 11 directory\n42 12 directory\n42 13 directory\n")
	walk := []byte("d 42 11\nd 42 12\nf 42 14\nd 42 13\nf 42 15\n")
	got, err := bindCNPGRecoveredFilesystem(stat, walk, stat)
	if err != nil || got.Device != 42 || got.Entries != 5 || got.Directories != [4]uint64{10, 11, 12, 13} {
		t.Fatalf("unchanged single-filesystem directory evidence did not bind: %+v %v", got, err)
	}
}

func TestRecoveredFilesystemRejectsUnresolvedLinksMountsAndPartialReads(t *testing.T) {
	stat := "42 10 directory\n42 11 directory\n42 12 directory\n42 13 directory\n"
	walk := "d 42 11\nd 42 12\nf 42 14\nd 42 13\n"
	for _, tc := range []struct{ name, before, walk, after string }{
		{"WAL symlink", strings.Replace(stat, "42 12 directory", "42 12 symbolic link", 1), walk, stat},
		{"descendant symlink", stat, walk + "l 42 15\n", stat},
		{"foreign mount boundary", stat, walk + "d 43 16\n", stat},
		{"foreign fixed directory", strings.Replace(stat, "42 12", "43 12", 1), walk, stat},
		{"special file", stat, walk + "s 42 15\n", stat},
		{"changed directory", stat, walk, strings.Replace(stat, "42 12", "42 14", 1)},
		{"same inode alias", strings.Replace(stat, "42 12", "42 11", 1), walk, stat},
		{"noncanonical device", strings.ReplaceAll(stat, "42 ", "042 "), walk, stat},
		{"zero inode", strings.Replace(stat, "42 12", "42 0", 1), walk, stat},
		{"missing fixed directory", stat, walk, "42 10 directory\n42 11 directory\n42 12 directory\n"},
		{"partial walk", stat, strings.TrimSuffix(walk, "\n"), stat},
		{"empty walk", stat, "", stat},
		{"file root", stat, strings.Replace(walk, "d 42 11", "f 42 11", 1), stat},
		{"truncated at root", stat, "d 42 11\n", stat},
		{"foreign root inode", stat, strings.Replace(walk, "d 42 11", "d 42 99", 1), stat},
		{"missing WAL inode", stat, strings.Replace(walk, "d 42 12", "d 42 99", 1), stat},
		{"zero walk inode", stat, strings.Replace(walk, "f 42 14", "f 42 0", 1), stat},
		{"duplicate directory", stat, walk + "d 42 12\n", stat},
		{"directory then file", stat, walk + "d 42 99\nf 42 99\n", stat},
		{"file then directory", stat, walk + "f 42 99\nd 42 99\n", stat},
		{"extra output", stat, walk + "private-canary\n", stat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bindCNPGRecoveredFilesystem([]byte(tc.before), []byte(tc.walk), []byte(tc.after))
			if err == nil || got != (cnpgFilesystemBinding{}) || strings.Contains(err.Error(), "private-canary") {
				t.Fatalf("unproven filesystem placement accepted or leaked: %+v %v", got, err)
			}
		})
	}
}

func TestRecoveredFilesystemNativeGNUCommandsDetectLinksAndDirectoryReplacement(t *testing.T) {
	find, stat := os.Getenv("LONGHORN_FIND_NATIVE_BIN"), os.Getenv("LONGHORN_STAT_NATIVE_BIN")
	if find == "" || stat == "" {
		t.Skip("set LONGHORN_FIND_NATIVE_BIN and LONGHORN_STAT_NATIVE_BIN to GNU tools")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	run := func(program string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
		out, err := cmd.CombinedOutput()
		if err != nil || len(out) > 1<<20 {
			t.Fatalf("owned native filesystem command failed: %v", err)
		}
		return out
	}
	if !strings.Contains(string(run(find, "--version")), "GNU findutils") || !strings.Contains(string(run(stat, "--version")), "GNU coreutils") {
		t.Fatal("native filesystem test requires GNU command semantics")
	}
	root := t.TempDir()
	data := filepath.Join(root, "pgdata")
	wal, tablespaces := filepath.Join(data, "pg_wal"), filepath.Join(data, "pg_tblspc")
	for _, directory := range []string{wal, tablespaces} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, "PG_VERSION"), []byte("18\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(data, "PG_VERSION"), filepath.Join(data, "regular-hard-link")); err != nil {
		t.Fatal(err)
	}
	readStat := func() []byte { return run(stat, "--format=%d %i %F", "--", root, data, wal, tablespaces) }
	readWalk := func() []byte { return run(find, "-P", data, "-xdev", "-printf", "%y %D %i\n") }
	before := readStat()
	if _, err := bindCNPGRecoveredFilesystem(before, readWalk(), readStat()); err != nil {
		t.Fatalf("real owned GNU filesystem observations refused: %v", err)
	}
	other := filepath.Join(root, "other-data")
	for _, directory := range []string{"pg_wal", "pg_tblspc"} {
		if err := os.MkdirAll(filepath.Join(other, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(other, "PG_VERSION"), []byte("18\n"), 0600); err != nil {
		t.Fatal(err)
	}
	foreignWalk := run(find, "-P", other, "-xdev", "-printf", "%y %D %i\n")
	if _, err := bindCNPGRecoveredFilesystem(before, foreignWalk, readStat()); err == nil {
		t.Fatal("a different native directory tree on the same filesystem was accepted")
	}
	link := filepath.Join(data, "unrelated-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := bindCNPGRecoveredFilesystem(readStat(), readWalk(), readStat()); err == nil {
		t.Fatal("real descendant symlink escaped placement verification")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(wal, filepath.Join(root, "old-wal")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(wal, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := bindCNPGRecoveredFilesystem(before, readWalk(), readStat()); err == nil {
		t.Fatal("real directory replacement escaped the inode bracket")
	}
}
