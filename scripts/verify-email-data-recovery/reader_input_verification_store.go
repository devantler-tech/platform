package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/devantler-tech/platform/internal/emailreaderinputs"
)

// readInputProofFile refuses links, foreign ownership, shared modes and incomplete private handoffs.
func readInputProofFile(directory, name string, limit int64) (result []byte, resultErr error) {
	fail := func() ([]byte, error) { return nil, errors.New("input-proof-file") }
	if name != "token" && name != "expected.json" {
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
			result, resultErr = fail()
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
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail()
	}
	defer func() {
		if file.Close() != nil {
			result, resultErr = fail()
		}
	}()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() <= 0 || info.Size() > limit {
		return fail()
	}
	owner, ok = info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return fail()
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) != info.Size() {
		return fail()
	}
	return raw, nil
}

// loadReaderInputVerificationStore uses only the verification workflow's separate RAM directory and archived token.
func loadReaderInputVerificationStore(env func(string) string) (*readerCustodyStore, error) {
	directory := env("EMAIL_READER_INPUTS_WORK_DIR")
	if !regexp.MustCompile(`^/dev/shm/email-input-native\.[A-Za-z0-9]{8}$`).MatchString(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("input-proof-workspace")
	}
	return readReaderInputVerificationStore(directory)
}

// readReaderInputVerificationStore binds its immutable record expectation before any live record request.
func readReaderInputVerificationStore(directory string) (*readerCustodyStore, error) {
	token, err := readInputProofFile(directory, "token", 256)
	if err != nil || !regexp.MustCompile(`^[A-Za-z0-9._-]{16,256}$`).Match(token) {
		return nil, errors.New("input-proof-token")
	}
	raw, err := readInputProofFile(directory, "expected.json", 1<<20)
	if err != nil {
		return nil, errors.New("input-proof-expectation")
	}
	expected, err := emailreaderinputs.Expectation(raw)
	if err != nil {
		return nil, errors.New("input-proof-expectation")
	}
	store, err := newReaderCustodyStore("http://127.0.0.1:38200", string(token))
	if err != nil {
		return nil, errors.New("input-proof-client")
	}
	store.originalRecord = expected
	return store, nil
}
