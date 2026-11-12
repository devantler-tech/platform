package main

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"

	"github.com/devantler-tech/platform/internal/strictjson"
)

// writeRestartWitnessFile creates the private snapshot once, without following links or replacing a prior handoff.
func writeRestartWitnessFile(directory string, witness object) (resultErr error) {
	fail := func() error { return errors.New("reader-restart-workspace") }
	raw, err := json.Marshal(witness)
	if err != nil || len(raw) == 0 || len(raw) > 1<<20 {
		return fail()
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fail()
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fail()
	}
	defer func() {
		if root.Close() != nil {
			resultErr = fail()
		}
	}()
	info, err = root.Stat(".")
	if err != nil {
		return fail()
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fail()
	}
	file, err := root.OpenFile("restart-witness.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fail()
	}
	defer func() {
		if file.Close() != nil {
			resultErr = fail()
		}
	}()
	if n, err := file.Write(raw); err != nil || n != len(raw) || file.Sync() != nil {
		return fail()
	}
	return nil
}

// readRestartWitnessFile cannot select arbitrary files or accept duplicate-key handoffs.
func readRestartWitnessFile(directory string) (object, error) {
	raw, err := readOwnedProofFile(directory, "restart-witness.json", 1<<20)
	var witness object
	if err != nil || strictjson.Validate(raw, nil) != nil || json.Unmarshal(raw, &witness) != nil || witness == nil {
		return nil, errors.New("reader-restart-workspace")
	}
	return witness, nil
}
