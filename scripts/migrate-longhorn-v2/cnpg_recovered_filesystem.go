package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
)

const recoveredPostgresMountPath = "/var/lib/postgresql/data"

// findmnt includes child mounts even when they have the same device number.
// The exact observation also binds the mount ID across the filesystem walk.
func recoveredFilesystemMountDevice(raw []byte) (uint64, error) {
	refused := errors.New("recovery data does not use one writable Ext4 mount")
	fields := map[string][]string{"": {"filesystems"}, "filesystems[]": {"target", "fsroot", "fstype", "maj:min", "id", "vfs-options"}}
	if len(raw) > 16384 || strictjson.Validate(raw, fields) != nil {
		return 0, refused
	}
	var response struct {
		Filesystems []struct {
			Target  string `json:"target"`
			Root    string `json:"fsroot"`
			Type    string `json:"fstype"`
			Device  string `json:"maj:min"`
			ID      uint64 `json:"id"`
			Options string `json:"vfs-options"`
		} `json:"filesystems"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || len(response.Filesystems) != 1 {
		return 0, refused
	}
	mount := response.Filesystems[0]
	options := strings.Split(mount.Options, ",")
	if mount.Target != recoveredPostgresMountPath || mount.Root != "/" || mount.Type != "ext4" || mount.ID == 0 ||
		!slices.Contains(options, "rw") || slices.Contains(options, "ro") {
		return 0, refused
	}
	majorText, minorText, ok := strings.Cut(mount.Device, ":")
	major, majorErr := strconv.ParseUint(majorText, 10, 32)
	minor, minorErr := strconv.ParseUint(minorText, 10, 32)
	if !ok || majorErr != nil || minorErr != nil || major == 0 || strconv.FormatUint(major, 10) != majorText || strconv.FormatUint(minor, 10) != minorText {
		return 0, refused
	}
	// Linux dev_t encoding shared by GNU stat %d and find %D.
	return (major&0xfffff000)<<32 | (minor&0xffffff00)<<12 | (major&0xfff)<<8 | minor&0xff, nil
}

// Necessary filesystem evidence only. The caller must independently bind the
// actual Pod mount, process, physical history and complete V2 storage graph.
type cnpgFilesystemBinding struct {
	Device, Entries uint64
	Directories     [4]uint64
}

func bindCNPGRecoveredFilesystem(before, walk, after []byte) (cnpgFilesystemBinding, error) {
	refused := errors.New("recovered filesystem placement is unverified")
	var zero, result cnpgFilesystemBinding
	if len(before) == 0 || len(before) > 1024 || !bytes.Equal(before, after) || before[len(before)-1] != '\n' || len(walk) == 0 || len(walk) > 16<<20 || walk[len(walk)-1] != '\n' {
		return zero, refused
	}
	number := func(value string) (uint64, bool) {
		n, err := strconv.ParseUint(value, 10, 64)
		return n, err == nil && n > 0 && strconv.FormatUint(n, 10) == value
	}
	directories := strings.Split(string(before[:len(before)-1]), "\n")
	if len(directories) != len(result.Directories) {
		return zero, refused
	}
	seen := map[uint64]bool{}
	for i, line := range directories {
		fields := strings.Split(line, " ")
		if len(fields) != 3 || fields[2] != "directory" {
			return zero, refused
		}
		device, validDevice := number(fields[0])
		inode, validInode := number(fields[1])
		if !validDevice || !validInode || seen[inode] || result.Device != 0 && result.Device != device {
			return zero, refused
		}
		result.Device, result.Directories[i], seen[inode] = device, inode, true
	}
	var fileCount uint64
	walkTypes := map[uint64]string{}
	for _, line := range strings.Split(string(walk[:len(walk)-1]), "\n") {
		fields := strings.Split(line, " ")
		if len(fields) != 3 || fields[0] != "d" && fields[0] != "f" || result.Entries == 0 && fields[0] != "d" {
			return zero, refused
		}
		device, validDevice := number(fields[1])
		inode, validInode := number(fields[2])
		if !validDevice || !validInode || device != result.Device || result.Entries == 0 && inode != result.Directories[1] {
			return zero, refused
		}
		if prior := walkTypes[inode]; prior != "" && (prior != fields[0] || prior == "d") {
			return zero, refused
		}
		walkTypes[inode] = fields[0]
		if fields[0] == "f" {
			if seen[inode] {
				return zero, refused
			}
			fileCount++
		}
		result.Entries++
		if result.Entries > 1000000 {
			return zero, refused
		}
	}
	// PGDATA, WAL and pg_tblspc must all appear, alongside actual regular data.
	// The fixed GNU walk's successful, bounded exit establishes its completeness.
	if walkTypes[result.Directories[1]] != "d" || walkTypes[result.Directories[2]] != "d" || walkTypes[result.Directories[3]] != "d" || fileCount == 0 {
		return zero, refused
	}
	return result, nil
}
