// alert-publisher is deliberately not wired to a deployment or existing route.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type remoteFactory func(int64, []byte) (githubAPI, error)

func command(args []string, input io.Reader, output io.Writer, factory remoteFactory) int {
	report := func(code int, message any) int {
		if _, err := fmt.Fprintln(output, message); err != nil && code == 0 {
			return 2
		}
		return code
	}
	flags := flag.NewFlagSet("alert-publisher", flag.ContinueOnError)
	// Flag parse failures can echo untrusted argument values. Emit fixed errors.
	flags.SetOutput(io.Discard)
	enabled := flags.Bool("enabled", false, "enable the prepared publisher")
	drain := flags.Bool("drain", false, "process pending journal entries only")
	dir := flags.String("state-dir", "", "private durable state directory")
	keyPath := flags.String("key-file", "", "projected App private key file")
	installation := flags.Int64("installation-id", 0, "configured existing App installation")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return report(2, "Invalid publisher invocation.")
	}
	if !*enabled {
		return report(0, "Publisher is disabled.")
	}
	if !filepath.IsAbs(*dir) || !filepath.IsAbs(*keyPath) || *installation <= 0 {
		return report(2, "Publisher deployment facts are incomplete.")
	}
	var event *observation
	if !*drain {
		value, err := decodeObservation(input)
		if err != nil {
			return report(2, errInput)
		}
		event = &value
	}
	keyFile, err := os.Open(*keyPath)
	if err != nil {
		return report(2, "Publisher credential is unavailable.")
	}
	info, statErr := keyFile.Stat()
	key, readErr := io.ReadAll(io.LimitReader(keyFile, 64*1024+1))
	closeErr := keyFile.Close()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0027 != 0 || readErr != nil || closeErr != nil || len(key) == 0 || len(key) > 64*1024 {
		return report(2, "Publisher credential is unavailable.")
	}
	api, err := factory(*installation, key)
	if err != nil {
		return report(2, "Publisher App identity is unverified.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	p := publisher{dir: *dir, api: api, now: time.Now}
	if err = p.run(ctx, event); err != nil {
		if errors.Is(err, errQuarantined) {
			return report(4, err)
		}
		if errors.Is(err, errStore) || errors.Is(err, errFull) || errors.Is(err, errInput) {
			return report(2, err)
		}
		return report(3, err)
	}
	return report(0, "Observation processed; confirmed receipts are recorded in the journal.")
}
func main() {
	os.Exit(command(os.Args[1:], os.Stdin, os.Stderr, func(id int64, key []byte) (githubAPI, error) { return newGitHubRemote(id, key) }))
}
