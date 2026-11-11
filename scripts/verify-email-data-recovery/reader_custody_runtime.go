package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

func readReaderCustodyToken(directory string) (token string, err error) {
	fail := func() (string, error) { return "", errors.New("reader-custody-token") }
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fail()
	}
	info, err = os.Lstat(filepath.Join(directory, "token"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 16 || info.Size() > 256 {
		return fail()
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fail()
	}
	defer func() {
		if root.Close() != nil {
			token, err = fail()
		}
	}()
	file, err := root.Open("token")
	if err != nil {
		return fail()
	}
	defer func() {
		if file.Close() != nil {
			token, err = fail()
		}
	}()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fail()
	}
	raw, err := io.ReadAll(io.LimitReader(file, 257))
	if err != nil || !regexp.MustCompile(`^[A-Za-z0-9._-]{16,256}$`).Match(raw) {
		return fail()
	}
	return string(raw), nil
}

func loadReaderCustodyStore(env func(string) string) (*readerCustodyStore, error) {
	directory := env("EMAIL_READER_INPUTS_WORK_DIR")
	if !regexp.MustCompile(`^/dev/shm/email-reader-inputs\.[A-Za-z0-9]{8}$`).MatchString(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("reader-custody-workspace")
	}
	token, err := readReaderCustodyToken(directory)
	if err != nil {
		return nil, errors.New("reader-custody-token")
	}
	return newReaderCustodyStore("http://127.0.0.1:38200", token)
}

func verifyReaderCustodySource(ctx context.Context, revision string) error {
	if !recoveryRevisionPattern.MatchString(revision) {
		return errors.New("reader-custody-source")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "./scripts/verify-prod-recovery-source.sh", revision)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.WaitDelay = 5 * time.Second
	if command.Run() != nil {
		return errors.New("reader-custody-source")
	}
	return nil
}
